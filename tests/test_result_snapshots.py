"""Durable result references: exact replay, TTL, namespace and optional caching."""

import io
import json
from concurrent.futures import ThreadPoolExecutor
from threading import Barrier, RLock
from types import SimpleNamespace
from unittest.mock import Mock

import pytest
from qdrant_client import QdrantClient
from sqlalchemy import select
from sqlalchemy.exc import OperationalError
from uuid import uuid4

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.config import RagConfig
from rag_mcp.service_knowledge import DenseDocumentIngester, KnowledgeDocumentStore, RagServiceRuntime
from rag_mcp.sparse import Bm25SparseEncoder
from rag_mcp.state.cache import CacheSettings, RedisStateCache
from rag_mcp.state.result_snapshots import ResultSnapshots, SnapshotSettings, SnapshotUnavailable
from rag_mcp.state.schema import search_result_snapshots
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from rag_mcp.storage.qdrant_store import QdrantStore


class SnapshotRedis:
    def __init__(self):
        self.values = {}
        self.down = False

    def set(self, key, value, *, ex, nx):
        if self.down:
            raise OSError("redis://password-must-not-leak")
        if nx and key in self.values:
            return False
        self.values[key] = bytes(value)
        return True

    def get(self, key):
        if self.down:
            raise OSError("unreachable")
        return self.values.get(key)

    def close(self):
        pass


@pytest.fixture()
def state(tmp_path):
    state = StateStores.create(settings=StateSettings(url=f"sqlite+pysqlite:///{tmp_path / 'state.db'}"))
    client = SnapshotRedis()
    state.cache_sync.backend = RedisStateCache(CacheSettings(prefix="test"), client=client)
    yield state, client
    state.close()


def test_saved_result_pagination_never_executes_search(state):
    state, client = state
    payload = {"results": [{"rank": i + 1, "text": f"text {i}", "score": i / 100} for i in range(23)],
               "debug": {"retrieval": "hybrid", "rerank": "cross_encoder"}}
    saved = state.results.save("ns", payload)
    payload["results"][0]["text"] = "mutated"
    page = state.results.get("ns", saved["result_id"], page=2, page_size=10)
    assert page["results"] == saved["results"][10:20]
    assert page["total_results"] == 23 and page["next_page"] == 3
    assert state.results.get("ns", saved["result_id"])["results"][0]["text"] == "text 0"
    assert saved["results"][0]["text"] == "text 0"
    assert saved["expires_at"] - saved["created_at"] == 3600
    with pytest.raises(SnapshotUnavailable):
        state.results.get("other", saved["result_id"])
    with pytest.raises(ValueError):
        state.results.get("ns", saved["result_id"], page=0)


def test_expiry_eviction_and_redis_loss_never_recompute(state):
    state, client = state
    clock = Mock(return_value=1000)
    snapshots = ResultSnapshots(state, clock=clock)
    saved = snapshots.save("ns", {"results": []})
    clock.return_value = saved["expires_at"]
    with pytest.raises(SnapshotUnavailable):
        snapshots.get("ns", saved["result_id"])
    client.values.clear()
    clock.return_value = 1000
    assert snapshots.get("ns", saved["result_id"])["result_id"] == saved["result_id"]
    client.down = True
    assert snapshots.get("ns", saved["result_id"])["results"] == []
    durable = snapshots.save("ns", {"results": [{"text": "saved during outage"}]})
    assert snapshots.get("ns", durable["result_id"])["results"] == durable["results"]


def test_snapshot_limit_and_empty_page(state):
    state, client = state
    snapshots = ResultSnapshots(state, settings=SnapshotSettings(max_result_bytes=10))
    with pytest.raises(SnapshotUnavailable, match="size"):
        snapshots.save("ns", {"results": [{"text": "too large"}]})
    saved = state.results.save("ns", {"results": []})
    assert state.results.get("ns", saved["result_id"])["next_page"] is None
    with pytest.raises(ValueError):
        state.results.get("ns", saved["result_id"], page=2)


def test_runtime_replay_and_artifacts_survive_replacement_and_deletion(state, tmp_path):
    state, client = state
    # Cache synchronization uses its normal disabled backend; snapshots get a separate
    # injected Redis backend so revision Lua isn't needed in this execution fixture.
    snapshot_backend = state.cache_sync.backend
    state.cache_sync.backend = RedisStateCache(CacheSettings(enabled=False))
    class SnapshotStorage:
        enabled = True
        settings = snapshot_backend.settings
        _connection = snapshot_backend._connection
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = state
    runtime._inference_lock = RLock()
    qdrant = QdrantClient(":memory:")
    store = QdrantStore(collection="snapshots", namespace="ns", client=qdrant,
                        active_versions=lambda: state.uploads.active_versions("ns"))
    vector = [1.0] + [0.0] * 1023
    ingester = DenseDocumentIngester(RagConfig(backend="qdrant", collection="snapshots"),
        SimpleNamespace(embed_documents=lambda texts: [vector for _ in texts]), Bm25SparseEncoder(), store)
    documents = KnowledgeDocumentStore(state, "ns")
    config = ChunkingConfig("character", 100, 10)
    first = documents.store_and_index("source.txt", io.BytesIO(b"original source"), ingester, config)
    response = {"results": [{"rank": 1, "document_id": first["document_id"], "chunk_id": "c",
                             "source": "source.txt", "text": "original source", "score": 0.9,
                             "metadata": {"upload_version": first["operation_id"]}}]}
    search = Mock(return_value=response)
    runtime._tenant = lambda ns: SimpleNamespace(lock=RLock(), orchestrator=SimpleNamespace(search=search))
    # Enable only while searching; fixture backend doesn't support revision Lua.
    original_backend = state.cache_sync.backend
    state.cache_sync.backend = SnapshotStorage()
    # Journal status refresh is unrelated to result storage for this fixture.
    original_flush = state.cache_sync.flush_pending
    state.cache_sync.flush_pending = lambda *a, **k: None
    try:
        saved = runtime.search("ns", {"query": "anything", "rerank": "none"})
        assert saved["snapshot_available"] and saved["result_id"]
        source_ref = saved["results"][0]["artifacts"][0]
        state.cache_sync.backend = original_backend
        documents.store_and_index("source.txt", io.BytesIO(b"replacement source"), ingester, config)
        documents.delete("source.txt", ingester)
        state.cache_sync.backend = SnapshotStorage()
        replay = runtime.get_search_result("ns", saved["result_id"])
        assert replay["results"][0]["text"] == "original source"
        assert search.call_count == 1
        content, mime, name = runtime.download_artifact("ns", source_ref["artifact_id"])
        assert content == b"original source" and name == "source.txt"
        assert runtime.get_artifact("ns", source_ref["artifact_id"])["encoding"] == "base64"
        with pytest.raises(SnapshotUnavailable):
            runtime.download_artifact("other", source_ref["artifact_id"])
        # Cache loss replays SQL without inference or deleting the retained file.
        client.values.clear()
        assert runtime.get_search_result("ns", saved["result_id"])["results"] == replay["results"]
        state.cache_sync.backend = original_backend
        assert runtime.get_search_result("ns", saved["result_id"])["results"] == replay["results"]
        assert search.call_count == 1
        assert runtime.download_artifact("ns", source_ref["artifact_id"])[0] == b"original source"
    finally:
        state.cache_sync.backend = original_backend
        state.cache_sync.flush_pending = original_flush
        qdrant.close()
        snapshot_backend.close()


def test_search_still_returns_original_results_when_redis_write_fails(state):
    state, client = state
    client.down = True
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = state
    runtime._inference_lock = RLock()
    payload = {"results": [{"document_id": "not-local", "text": "evidence", "score": 0.7}],
               "query": "query"}
    search = Mock(return_value=payload)
    runtime._tenant = lambda ns: SimpleNamespace(lock=RLock(), orchestrator=SimpleNamespace(search=search))
    result = runtime.search("ns", {"query": "query", "rerank": "none"})
    assert result["snapshot_available"] is True
    assert result["result_id"]
    assert state.results.get("ns", result["result_id"])["results"] == result["results"]
    assert result["results"][0]["text"] == "evidence"
    assert search.call_count == 1


@pytest.mark.parametrize("corrupt", [
    b"not json", b"\xff", b"[]", b"null", b"{}",
    b'{"namespace":"ns","expires_at":99999999999,"payload":null}',
    b'{"namespace":"ns","expires_at":NaN,"payload":{}}',
])
def test_malformed_redis_snapshot_falls_back_to_sql(state, corrupt):
    state, client = state
    saved = state.results.save("ns", {"results": [{"text": "durable"}]})
    key = state.results._key("ns", saved["result_id"])
    client.values[key] = corrupt
    assert state.results.get("ns", saved["result_id"])["results"] == saved["results"]
    assert json.loads(client.values[key])["payload"] == saved


def test_redis_disabled_and_process_restart_preserve_full_response(state):
    state, client = state
    state.cache_sync.backend = RedisStateCache(CacheSettings(enabled=False))
    payload = {"query": "public query", "collection": "knowledge", "total": 1,
               "results": [{"rank": 1, "text": "证据", "metadata": {"nested": [1, {"value": True}]},
                            "artifacts": [{"artifact_id": "external-reference"}]}],
               "debug": {"timings": {"rerank": 0.25}, "retrieval": "hybrid"},
               "custom": {"null": None, "items": [1, 2]}}
    saved = state.results.save("ns", payload)
    payload["results"][0]["metadata"]["nested"].append("mutation")
    saved["debug"]["timings"]["rerank"] = 99
    other = StateStores.create(settings=state.settings)
    other.cache_sync.backend = RedisStateCache(CacheSettings(enabled=False))
    try:
        replay = other.results.get("ns", saved["result_id"])
        assert replay["debug"]["timings"]["rerank"] == 0.25
        assert replay["results"][0]["metadata"]["nested"] == [1, {"value": True}]
        assert replay["query"] == "public query" and replay["custom"] == payload["custom"]
        assert replay["results"][0]["artifacts"] == payload["results"][0]["artifacts"]
        with pytest.raises(SnapshotUnavailable):
            other.results.get("other", saved["result_id"])
    finally:
        other.close()


def test_uuid_is_immutable_globally_and_failed_write_does_not_replace_cache(state):
    state, client = state
    snapshots = ResultSnapshots(state, clock=lambda: 1000)
    saved = snapshots.save("ns", {"results": [{"text": "original"}]})
    before = dict(client.values)
    for namespace, payload in [("ns", {"results": []}), ("other", {"results": []}),
                               ("ns", {"results": saved["results"]})]:
        with pytest.raises(SnapshotUnavailable, match="already exists"):
            snapshots.save(namespace, payload, result_id=saved["result_id"])
        assert client.values == before
    assert snapshots.get("ns", saved["result_id"])["results"] == saved["results"]


def test_expiry_is_strict_for_cache_and_sql_and_purge_is_bounded(state):
    state, client = state
    clock = Mock(return_value=1000)
    snapshots = ResultSnapshots(state, clock=clock)
    expired = [snapshots.save("ns", {"results": []}) for _ in range(3)]
    clock.return_value = 2000
    live = snapshots.save("ns", {"results": []})
    clock.return_value = expired[0]["expires_at"] - 0.01
    assert snapshots.get("ns", expired[0]["result_id"])["results"] == []
    clock.return_value = expired[0]["expires_at"]
    for backend in [state.cache_sync.backend, RedisStateCache(CacheSettings(enabled=False))]:
        state.cache_sync.backend = backend
        with pytest.raises(SnapshotUnavailable):
            snapshots.get("ns", expired[0]["result_id"])
    assert snapshots.purge_expired(limit=2) == 2
    assert snapshots.purge_expired(batch_size=2) == 1
    assert snapshots.purge_expired(limit=2) == 0
    assert snapshots.get("ns", live["result_id"])["results"] == []
    for size in [0, 1001, True, "1"]:
        for keyword in ["limit", "batch_size"]:
            with pytest.raises(ValueError):
                snapshots.purge_expired(**{keyword: size})


@pytest.mark.parametrize("page,page_size", [(0, 10), (True, 10), (1, False), (1, 101), (1, 0)])
def test_invalid_paging_is_rejected(state, page, page_size):
    state, client = state
    saved = state.results.save("ns", {"results": []})
    with pytest.raises(ValueError):
        state.results.get("ns", saved["result_id"], page=page, page_size=page_size)


@pytest.mark.parametrize("value", ["", "not-a-uuid", "ABCDEFAB-1234-1234-1234-123456789ABC", None])
def test_invalid_result_id_is_unavailable(state, value):
    state, client = state
    with pytest.raises(SnapshotUnavailable):
        state.results.get("ns", value)


def test_cache_is_published_only_after_sql_commit(state, monkeypatch):
    state, client = state
    original_set = client.set
    published = []

    def inspect_commit(key, value, *, ex, nx):
        envelope = json.loads(value)
        with state.engine.connect() as conn:
            row = conn.execute(select(search_result_snapshots).where(
                search_result_snapshots.c.id == envelope["payload"]["result_id"]
            )).mappings().one()
        assert json.loads(row["content"]) == envelope
        assert ex == 3600
        published.append(key)
        return original_set(key, value, ex=ex, nx=nx)

    monkeypatch.setattr(client, "set", inspect_commit)
    snapshots = ResultSnapshots(state, clock=lambda: 1000)
    snapshots.save("ns", {"results": []})
    assert len(published) == 1


def test_concurrent_same_uuid_keeps_one_immutable_snapshot(state):
    state, client = state
    state.cache_sync.backend = RedisStateCache(CacheSettings(enabled=False))
    result_id = str(uuid4())
    barrier = Barrier(2)

    def save(text):
        barrier.wait(timeout=10)
        try:
            return state.results.save("ns", {"results": [{"text": text}]}, result_id=result_id)
        except SnapshotUnavailable:
            return None

    with ThreadPoolExecutor(max_workers=2) as executor:
        outcomes = list(executor.map(save, ["first", "second"]))
    successes = [outcome for outcome in outcomes if outcome is not None]
    assert len(successes) == 1
    assert state.results.get("ns", result_id)["results"] == successes[0]["results"]


def test_sql_failure_never_publishes_cache_or_leaks_connection_details(state, monkeypatch):
    state, client = state
    private_error = OperationalError("private SQL", {}, OSError("password-must-not-leak"))
    with monkeypatch.context() as patch:
        patch.setattr(state.engine, "begin", Mock(side_effect=private_error))
        with pytest.raises(SnapshotUnavailable) as error:
            state.results.save("ns", {"results": []})
        assert "password" not in str(error.value) and "private SQL" not in str(error.value)
    assert client.values == {}


def test_cached_wrong_namespace_or_uuid_cannot_override_sql(state):
    state, client = state
    saved = state.results.save("ns", {"results": [{"text": "original"}]})
    key = state.results._key("ns", saved["result_id"])
    for field, wrong in [("namespace", "other"), ("result_id", str(uuid4()))]:
        envelope = json.loads(client.values[key])
        if field == "namespace":
            envelope[field] = wrong
        else:
            envelope["payload"][field] = wrong
        envelope["payload"]["results"] = [{"text": "wrong"}]
        client.values[key] = json.dumps(envelope).encode("utf-8")
        assert state.results.get("ns", saved["result_id"])["results"] == saved["results"]


def test_snapshot_bound_measures_encoded_utf8_and_writes_nothing_when_exceeded(state):
    state, client = state
    fixed_id = str(uuid4())
    snapshots = ResultSnapshots(state, clock=lambda: 1000)
    saved = snapshots.save("ns", {"results": [{"text": "证据"}]}, result_id=fixed_id)
    encoded_size = len(next(iter(client.values.values())))
    limited = ResultSnapshots(state, clock=lambda: 1000,
                              settings=SnapshotSettings(max_result_bytes=encoded_size))
    limited.save("ns", {"results": saved["results"]})
    too_small = ResultSnapshots(state, clock=lambda: 1000,
                                settings=SnapshotSettings(max_result_bytes=encoded_size - 1))
    rejected_id = str(uuid4())
    with pytest.raises(SnapshotUnavailable, match="size"):
        too_small.save("ns", {"results": saved["results"]}, result_id=rejected_id)
    with state.engine.connect() as conn:
        assert conn.execute(select(search_result_snapshots.c.id).where(
            search_result_snapshots.c.id == rejected_id)).first() is None
    assert all(rejected_id not in key for key in client.values)


@pytest.mark.parametrize("created", [True, float("nan"), float("inf"), "1000"])
def test_invalid_creation_time_cannot_be_persisted(state, created):
    state, client = state
    with pytest.raises(ValueError, match="creation time"):
        state.results.save("ns", {"results": []}, created_at=created)
    assert client.values == {}


def test_expired_creation_time_cannot_be_persisted(state):
    state, client = state
    snapshots = ResultSnapshots(state, clock=lambda: 4600)
    with pytest.raises(SnapshotUnavailable, match="already expired"):
        snapshots.save("ns", {"results": []}, created_at=1000)
    with state.engine.connect() as conn:
        assert conn.execute(select(search_result_snapshots.c.id)).first() is None
