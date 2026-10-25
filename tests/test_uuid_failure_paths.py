"""Bounded UUID failure paths through real retrieval, SQL, and reference capture."""

import copy
import json
from dataclasses import replace
from threading import RLock
from types import SimpleNamespace
from unittest.mock import Mock
from uuid import UUID

import pytest
from sqlalchemy import create_engine, event, func, inspect, select, text
from sqlalchemy.exc import OperationalError
from test_soft_delete_references import VECTOR, snapshot, upload
from test_soft_delete_references import stack as stack_fixture

from rag_mcp.orchestrator import RagOrchestrator
from rag_mcp.retrievers.dense import DenseRetriever
from rag_mcp.service_knowledge import RagServiceRuntime
from rag_mcp.state.cache import CacheSettings, RedisStateCache
from rag_mcp.state.result_snapshots import SnapshotUnavailable
from rag_mcp.state.schema import artifact_snapshots
from rag_mcp.state.schema_migrations import migrate_soft_delete
from rag_mcp.state.search_references import SearchReferences
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores

# Re-export the real fixture without shadowing an imported fixture symbol.
stack = stack_fixture
from redis_stub import MemoryRedis
from test_state_cache import FakeRedis


class FaultRedis(MemoryRedis, FakeRedis):
    """Use the existing revision fake for both state views and UUID cache keys."""

    def __init__(self):
        super().__init__()
        self.now = self.clock()
        self.last_script = None
        self.down = False

    def get(self, key):
        if self.down:
            raise OSError("cache unavailable")
        return super().get(key)

    def _check(self, key):
        if self.down:
            raise OSError("cache unavailable")
        super()._check(key)


@pytest.fixture(autouse=True)
def local_state_only(tmp_path, monkeypatch):
    monkeypatch.setenv("RAG_STATE_URL", f"sqlite+pysqlite:///{tmp_path / 'state.sqlite3'}")
    monkeypatch.setenv("RAG_STATE_AUTO_SCHEMA", "1")
    monkeypatch.setenv("RAG_REDIS_ENABLED", "0")
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    for name, value in (("RAG_RESULT_TTL_SECONDS", "3600"),
                        ("RAG_RESULT_MAX_BYTES", "16777216"),
                        ("RAG_ARTIFACT_MAX_BYTES", "67108864"),
                        ("RAG_RESULT_MAX_ARTIFACTS", "100")):
        monkeypatch.setenv(name, value)


@pytest.fixture(autouse=True)
def redis_results(request):
    if "stack" not in request.fixturenames:
        return
    stack = request.getfixturevalue("stack")
    stack[0].cache_sync.backend = RedisStateCache(
        CacheSettings(prefix="uuid-failure", enabled=True), client=FaultRedis())


@pytest.fixture()
def cached_stack(stack, redis_results):
    return stack, stack[0].cache_sync.backend._connection()


def runtime_for(stack):
    state, store, ingester, source = stack
    model = SimpleNamespace(embed_query=Mock(return_value=VECTOR))
    orchestrator = RagOrchestrator(
        {"embedding": DenseRetriever(model, store)}, {}, implementation="bounded-fake-model")
    search = Mock(side_effect=orchestrator.search)
    tenant = SimpleNamespace(lock=RLock(), documents=source, ingester=ingester,
                             orchestrator=SimpleNamespace(search=search))
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = state
    runtime._inference_lock = RLock()
    runtime._tenant = Mock(return_value=tenant)
    return runtime, model, search


def execute(runtime):
    return runtime.search("ns", {"query": "original evidence", "retrieval": "embedding", "rerank": "none"})


def sql_unavailable(*_args, **_kwargs):
    raise OperationalError("injected SQL outage", {}, OSError("database unavailable"))


def fail_insert(table_name):
    def fail(_conn, _cursor, statement, _parameters, _context, _executemany):
        if statement.startswith(f"INSERT INTO {table_name} "):
            sql_unavailable()
    return fail


def table_payload(record):
    first = {"type": "table", "rows": [["证据", "one"]]}
    second = {"type": "table", "rows": [["证据", "two"]]}
    return {"query": "original evidence", "debug": {"nested": {"values": [1, 2]}},
            "results": [
                {"rank": rank, "document_id": record["document_id"], "text": f"hit {rank}",
                 "metadata": {"upload_version": record["operation_id"], "asset_id": name,
                              "nested": {"values": [rank]}}, "asset": copy.deepcopy(asset)}
                for rank, (name, asset) in enumerate(
                    [("first", first), ("first", first), ("second", second)], 1)]}


def assert_original_hits(original, captured):
    assert len(captured["results"]) == len(original["results"])
    for before, after in zip(original["results"], captured["results"], strict=True):
        for key, value in before.items():
            assert after[key] == value
    for key, value in original.items():
        if key != "results":
            assert captured[key] == value


def test_redis_down_capture_and_deleted_replay_never_reinfer_or_resolve_tenant(cached_stack, monkeypatch):
    stack, redis = cached_stack
    state, _, ingester, source = stack
    upload(stack)
    runtime, model, search = runtime_for(stack)
    saved = execute(runtime)
    assert saved["snapshot_available"] is True
    assert UUID(saved["result_id"]).version == 4
    assert saved["results"][0]["text"] == "original immutable evidence"
    artifact = saved["artifacts"][0]
    source.delete("file.txt", ingester)
    runtime._tenant = Mock(side_effect=AssertionError("Replay must not resolve a tenant"))

    redis.down = True
    with pytest.raises(SnapshotUnavailable, match="expired or unavailable"):
        runtime.get_search_result("ns", saved["result_id"])
    # Artifact bytes remain durable SQL records, unlike result snapshots.
    assert runtime.download_artifact("ns", artifact["artifact_id"])[0] == b"original immutable evidence"
    redis.down = False
    assert runtime.get_search_result("ns", saved["result_id"])["results"] == saved["results"]
    with monkeypatch.context() as patch:
        patch.setattr(state.engine, "connect", sql_unavailable)
        assert runtime.get_search_result("ns", saved["result_id"])["results"] == saved["results"]
        redis.down = True
        with pytest.raises(SnapshotUnavailable, match="expired or unavailable"):
            runtime.get_search_result("ns", saved["result_id"])
    redis.down = False
    assert runtime.get_search_result("ns", saved["result_id"])["results"] == saved["results"]
    assert model.embed_query.call_count == search.call_count == 1
    model.embed_query.assert_called_once_with("original evidence")
    runtime._tenant.assert_not_called()


def test_redis_snapshot_failure_returns_original_payload_without_unreadable_uuid(cached_stack):
    stack, redis = cached_stack
    state = stack[0]
    upload(stack)
    runtime, model, search = runtime_for(stack)
    capture = runtime._tenant.return_value.orchestrator.search
    original_search = capture.side_effect
    originals = []

    def record(request):
        payload = original_search(request)
        originals.append(copy.deepcopy(payload))
        return payload

    capture.side_effect = record
    redis.down = True
    response = execute(runtime)
    redis.down = False
    assert_original_hits(originals[0], response)
    assert response["snapshot_available"] is False
    assert response["snapshot_error"]
    assert not {"result_id", "created_at", "expires_at"} & response.keys()
    assert not any(":transient:" in key for key in redis.values)
    assert state.artifacts.read("ns", response["artifacts"][0]["artifact_id"])["content"] == b"original immutable evidence"
    assert model.embed_query.call_count == search.call_count == 1


def test_artifact_insert_failure_warns_but_keeps_real_search_and_durable_snapshot(cached_stack):
    stack, redis = cached_stack
    state = stack[0]
    upload(stack)
    runtime, model, search = runtime_for(stack)
    fault = fail_insert("artifact_snapshots")
    event.listen(state.engine, "before_cursor_execute", fault)
    try:
        saved = execute(runtime)
    finally:
        event.remove(state.engine, "before_cursor_execute", fault)
    assert saved["snapshot_available"] is True
    assert saved["artifact_warning"] == "Some artifact references could not be stored"
    assert saved["artifacts"] == saved["results"][0]["artifacts"] == []
    assert saved["results"][0]["text"] == "original immutable evidence"
    assert saved["debug"]["returned_count"] == 1
    redis.down = True
    runtime._tenant = Mock(side_effect=AssertionError("Replay must not resolve a tenant"))
    with pytest.raises(SnapshotUnavailable):
        runtime.get_search_result("ns", saved["result_id"])
    redis.down = False
    assert runtime.get_search_result("ns", saved["result_id"])["results"] == saved["results"]
    with state.engine.connect() as conn:
        assert conn.scalar(select(func.count()).select_from(artifact_snapshots)) == 0
    assert model.embed_query.call_count == search.call_count == 1
    runtime._tenant.assert_not_called()


@pytest.mark.parametrize("budget", ["count", "bytes"])
def test_capture_limits_charge_unique_bytes_and_never_remove_hits(stack, monkeypatch, budget):
    state = stack[0]
    content = b"source"
    record = upload(stack, content)
    payload = table_payload(record)
    original = copy.deepcopy(payload)
    exact_bytes = len(content) + len(json.dumps(payload["results"][0]["asset"], ensure_ascii=False).encode())
    state.results.settings = replace(state.results.settings,
        max_artifacts_per_search=2 if budget == "count" else 100,
        max_artifact_bytes=exact_bytes if budget == "bytes" else 10000)
    register = Mock(wraps=state.artifacts.register)
    monkeypatch.setattr(state.artifacts, "register", register)
    saved = SearchReferences(state).capture("ns", payload)
    assert payload == original
    assert saved["snapshot_available"] is True
    assert saved["artifact_warning"]
    assert len(saved["results"]) == 3
    assert len(saved["artifacts"]) == register.call_count == 2
    first, duplicate, blocked = saved["results"]
    assert first["asset"]["artifact_id"] == duplicate["asset"]["artifact_id"]
    assert "artifact_id" not in blocked["asset"]
    assert [hit["text"] for hit in saved["results"]] == [hit["text"] for hit in original["results"]]
    assert [hit["asset"]["rows"] for hit in saved["results"]] == [hit["asset"]["rows"] for hit in original["results"]]
    retained_bytes = sum(len(state.artifacts.read("ns", ref["artifact_id"])["content"]) for ref in saved["artifacts"])
    assert retained_bytes == exact_bytes
    assert retained_bytes <= state.results.settings.max_artifact_bytes
    with state.engine.connect() as conn:
        assert conn.scalar(select(func.count()).select_from(artifact_snapshots)) == 2


def test_pages_keep_deterministic_frozen_metadata_without_sql_fallback(cached_stack, monkeypatch):
    stack, redis = cached_stack
    state = stack[0]
    record = upload(stack)
    saved = SearchReferences(state).capture("ns", table_payload(record))
    pages = [state.results.get("ns", saved["result_id"], page=page, page_size=2) for page in (1, 2)]
    assert pages[0]["results"] + pages[1]["results"] == saved["results"]
    for page, expected_next in zip(pages, (2, None), strict=True):
        assert page["total_results"] == 3 and page["page_size"] == 2
        assert page["next_page"] == expected_next
        assert {k: v for k, v in page.items() if k not in {
            "results", "page", "page_size", "total_results", "next_page"}} == {
                k: v for k, v in saved.items() if k != "results"}
    with monkeypatch.context() as patch:
        patch.setattr(state.engine, "connect", sql_unavailable)
        for number, expected in enumerate(pages, 1):
            assert state.results.get("ns", saved["result_id"], page=number, page_size=2) == expected
        redis.down = True
        with pytest.raises(SnapshotUnavailable):
            state.results.get("ns", saved["result_id"])
    redis.down = False
    with pytest.raises(ValueError, match="page does not exist"):
        state.results.get("ns", saved["result_id"], page=3, page_size=2)


def test_capture_input_response_and_replay_are_deeply_detached(cached_stack):
    stack, redis = cached_stack
    state = stack[0]
    record = upload(stack)
    payload = table_payload(record)
    saved = SearchReferences(state).capture("ns", payload)
    frozen = copy.deepcopy(saved)
    assert "artifact_id" not in payload["results"][0]["asset"]
    payload["results"][0]["asset"]["rows"][0][0] = "changed input"
    saved["results"][0]["metadata"]["nested"]["values"].append("changed response")
    saved["results"][0]["artifacts"][0]["name"] = "changed reference"
    saved["debug"]["nested"]["values"].clear()
    replay = state.results.get("ns", saved["result_id"])
    assert replay["results"] == frozen["results"]
    assert replay["debug"] == frozen["debug"]
    replay["results"][0]["asset"]["rows"].clear()
    replay["artifacts"][0]["name"] = "changed replay"
    assert state.results.get("ns", saved["result_id"])["results"] == frozen["results"]
    redis.down = True
    with pytest.raises(SnapshotUnavailable):
        state.results.get("ns", saved["result_id"])
    redis.down = False
    redis.delete(state.results.transient._key("search-result", saved["result_id"]))
    with pytest.raises(SnapshotUnavailable):
        state.results.get("ns", saved["result_id"])


def test_stale_display_cache_never_controls_live_get_or_new_queries_and_flush_repairs(cached_stack, monkeypatch):
    stack, redis = cached_stack
    state, store, ingester, source = stack
    record = upload(stack)
    issued = snapshot(stack)
    old_documents = state.views.documents("ns")
    old_heads = state.views.published_versions("ns")
    old_counts = state.views.knowledge_bases()
    assert old_documents and old_heads and old_counts
    # A display cache can lag delivery; strict reads and Qdrant must use SQL.
    with monkeypatch.context() as patch:
        patch.setattr(state.cache_sync, "flush_pending", lambda *args, **kwargs: None)
        source.delete("file.txt", ingester)
    assert state.views.documents("ns") == old_documents
    assert state.views.published_versions("ns") == old_heads
    assert state.views.knowledge_bases() == old_counts
    assert state.documents.get("ns", "file.txt") is None
    assert state.documents.get_by_id("ns", record["document_id"]) is None
    assert state.documents.candidates("ns", record["sha256"]) == []
    assert state.documents.visible_versions("ns")[record["document_id"]] == ""
    assert store.dense_search(VECTOR, limit=10, filters={}) == []
    assert state.results.get("ns", issued["result_id"])["results"] == issued["results"]
    redis.down = True
    assert state.views.documents("ns") == []
    assert state.views.published_versions("ns") == {}
    assert state.views.knowledge_bases() == []
    redis.down = False
    state.cache_sync.flush_pending()
    assert state.views.documents("ns") == []
    assert state.views.published_versions("ns") == {}
    assert state.views.knowledge_bases() == []
    assert not state.cache_sync.backend.put("documents", "ns", 0, old_documents)
    assert state.views.documents("ns") == []


def test_raw_legacy_schema_migrates_twice_and_startup_preserves_live_and_deleted_rows(tmp_path):
    settings = StateSettings(url=f"sqlite+pysqlite:///{tmp_path / 'legacy.sqlite3'}")
    engine = create_engine(settings.url)
    try:
        with engine.begin() as conn:
            conn.execute(text("""CREATE TABLE documents (
                namespace VARCHAR(64) NOT NULL, name VARCHAR(200) NOT NULL,
                document_id VARCHAR(64) NOT NULL, size BIGINT NOT NULL,
                sha256 VARCHAR(64) NOT NULL, chunking TEXT NOT NULL,
                collection VARCHAR(128) NOT NULL, chunk_count INTEGER NOT NULL,
                modified_at VARCHAR(40) NOT NULL, indexed_at VARCHAR(40) NOT NULL,
                diagnostics TEXT NOT NULL, skipped_object_count INTEGER NOT NULL,
                PRIMARY KEY (namespace, name), UNIQUE (namespace, document_id))"""))
            for name in ("live.txt", "deleted.txt"):
                conn.execute(text("""INSERT INTO documents VALUES
                    ('ns', :name, :name, 17, 'retained-hash', '{"strategy":"character"}',
                     'legacy-index', 2, 'old-modified', 'old-indexed', '["retained"]', 3)"""), {"name": name})
        migrate_soft_delete(engine)
        migrate_soft_delete(engine)
        assert [column["name"] for column in inspect(engine).get_columns("documents")].count("del_flag") == 1
        with engine.begin() as conn:
            assert conn.execute(text("SELECT del_flag FROM documents")).scalars().all() == [0, 0]
            conn.execute(text("UPDATE documents SET del_flag=1 WHERE name='deleted.txt'"))
            before = conn.execute(text("SELECT * FROM documents ORDER BY name")).all()
    finally:
        engine.dispose()
    for _ in range(2):
        state = StateStores.create(settings=settings)
        try:
            with state.engine.connect() as conn:
                assert conn.execute(text("SELECT * FROM documents ORDER BY name")).all() == before
            live = state.documents.get("ns", "live.txt")
            assert live["diagnostics"] == ["retained"] and live["sha256"] == "retained-hash"
            assert live["chunking"] == {"strategy": "character"}
            assert [row["name"] for row in state.documents.list("ns")] == ["live.txt"]
            assert state.documents.get("ns", "deleted.txt") is None
            assert state.documents.namespaces() == {"ns": 1}
        finally:
            state.close()
