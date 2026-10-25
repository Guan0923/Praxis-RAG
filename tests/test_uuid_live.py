"""Opt-in UUID lifecycle checks against disposable MySQL, Redis and Qdrant.

Set RAG_TEST_UUID_MYSQL_URL to an existing praxis_uuid_test_* schema and
RAG_TEST_REDIS_URL to enable these tests. RAG_TEST_QDRANT_URL defaults to
http://127.0.0.1:6333. No databases or grants are created. Each test clears
only the guarded schema, its own Redis prefix and its uuid_test_* collection.
"""

from __future__ import annotations

import base64
import io
import json
import os
import shutil
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from threading import Barrier, RLock
from types import SimpleNamespace
from unittest.mock import Mock
from uuid import UUID, uuid4

import pytest
from qdrant_client import QdrantClient
from sqlalchemy import delete, func, select, update
from sqlalchemy.engine import make_url

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.config import DENSE_VECTOR_SIZE, RagConfig
from rag_mcp.orchestrator import RagOrchestrator
from rag_mcp.retrievers.dense import DenseRetriever
from rag_mcp.service_knowledge import DenseDocumentIngester, KnowledgeDocumentStore, RagServiceRuntime
from rag_mcp.sparse import Bm25SparseEncoder
from rag_mcp.state.artifacts import ArtifactUnavailable
from rag_mcp.state.result_snapshots import ResultSnapshots, SnapshotSettings, SnapshotUnavailable
from rag_mcp.state.schema import SOURCE_BUCKET, artifact_snapshots, metadata
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from rag_mcp.storage.qdrant_store import QdrantStore

pytestmark = pytest.mark.skipif(
    not (os.getenv("RAG_TEST_UUID_MYSQL_URL") and os.getenv("RAG_TEST_REDIS_URL")),
    reason="Requires RAG_TEST_UUID_MYSQL_URL and RAG_TEST_REDIS_URL (disposable services)",
)


def _clear_schema(state):
    database = state.engine.url.database
    assert state.engine.dialect.name == "mysql", "UUID live tests require MySQL"
    assert database and database.startswith("praxis_uuid_test_"), "Disposable UUID schema only"
    with state.engine.begin() as conn:
        assert conn.exec_driver_sql("SELECT DATABASE()").scalar_one() == database
        for table in reversed(metadata.sorted_tables):
            conn.execute(delete(table))


def _clear_redis_prefix(backend):
    prefix = backend.settings.prefix
    assert prefix.startswith("rag:uuid-live:"), "Test-owned Redis prefix only"
    client = backend._connection()
    keys = list(client.scan_iter(match=prefix + ":*", count=100))
    for start in range(0, len(keys), 100):
        client.delete(*keys[start:start + 100])


@pytest.fixture()
def live_state(monkeypatch):
    # Validate before constructing an engine or performing any destructive work.
    try:
        url = make_url(os.environ["RAG_TEST_UUID_MYSQL_URL"])
        safe = (url.get_backend_name() == "mysql" and bool(url.database)
                and url.database.startswith("praxis_uuid_test_"))
    except Exception:
        safe = False
    if not safe:
        pytest.fail("RAG_TEST_UUID_MYSQL_URL must name a disposable praxis_uuid_test_* MySQL schema",
                    pytrace=False)

    monkeypatch.setenv("RAG_REDIS_URL", os.environ["RAG_TEST_REDIS_URL"])
    monkeypatch.setenv("RAG_REDIS_PREFIX", "rag:uuid-live:" + uuid4().hex)
    monkeypatch.setenv("RAG_REDIS_ENABLED", "true")
    monkeypatch.setenv("RAG_REDIS_TIMEOUT_SECONDS", "2")
    monkeypatch.setenv("RAG_ROCKETMQ_ENDPOINT", "")
    monkeypatch.setenv("RAG_RESULT_TTL_SECONDS", "120")
    monkeypatch.setenv("RAG_RESULT_MAX_BYTES", str(1024 * 1024))
    monkeypatch.setenv("RAG_ARTIFACT_MAX_BYTES", str(1024 * 1024))
    monkeypatch.setenv("RAG_RESULT_MAX_ARTIFACTS", "20")
    state = None
    try:
        state = StateStores(StateSettings(url=url.render_as_string(hide_password=False),
                                         pool_timeout=5, echo=False))
        state.engine.hide_parameters = True
        state.ping()
        assert state.cache_sync.backend.enabled
        assert state.cache_sync.transport is None
        assert state.cache_sync.backend._connection().ping()
        _clear_schema(state)
    except Exception:
        if state is not None:
            state.close()
        pytest.fail("UUID live MySQL/Redis setup failed; check disposable service configuration", pytrace=False)
    try:
        yield state
    finally:
        try:
            _clear_schema(state)
        finally:
            try:
                _clear_redis_prefix(state.cache_sync.backend)
            finally:
                state.close()


@pytest.fixture()
def live_index(live_state, monkeypatch):
    # Production uploads use tempfile.gettempdir(); keep even their workspaces
    # inside this checkout, not the machine's shared temporary directory.
    workspace = Path.cwd() / (".uuid-live-" + uuid4().hex)
    workspace.mkdir()
    monkeypatch.setattr("rag_mcp.service_knowledge.tempfile.gettempdir", lambda: str(workspace))
    collection = "uuid_test_" + uuid4().hex
    namespace = "uuid-live-" + uuid4().hex
    client = None
    try:
        client = QdrantClient(url=os.getenv("RAG_TEST_QDRANT_URL", "http://127.0.0.1:6333"), timeout=5)
        store = QdrantStore(collection=collection, namespace=namespace, client=client,
                            active_versions=lambda: live_state.documents.visible_versions(namespace))
        store.ensure_collection()
        yield SimpleNamespace(client=client, store=store, namespace=namespace, collection=collection)
    finally:
        try:
            if client is not None:
                try:
                    assert collection.startswith("uuid_test_")
                    if client.collection_exists(collection):
                        client.delete_collection(collection)
                finally:
                    client.close()
        finally:
            shutil.rmtree(workspace)


def test_mysql_concurrent_artifact_upsert_preserves_identity_and_immutable_bytes(live_state):
    state = live_state
    namespace = "upsert-" + uuid4().hex
    content = b"original SQL snapshot\x00\xff"
    now = time.time()
    values = dict(namespace=namespace, document_id="doc", version="version-1", kind="source",
                  name="original.bin", mime_type="application/octet-stream", content=content)
    barrier = Barrier(6, timeout=10)

    def register(index):
        barrier.wait()
        return state.artifacts.register(**values, expires_at=now + 200 + index)

    with ThreadPoolExecutor(max_workers=6) as executor:
        futures = [executor.submit(register, index) for index in range(6)]
        registered = [future.result(timeout=20) for future in futures]
    original = registered[0]
    assert {item["artifact_id"] for item in registered} == {original["artifact_id"]}
    assert str(UUID(original["artifact_id"])) == original["artifact_id"]
    assert UUID(original["artifact_id"]).version == 5
    with state.engine.connect() as conn:
        assert conn.scalar(select(func.count()).select_from(artifact_snapshots)) == 1
        created = conn.scalar(select(artifact_snapshots.c.created_at))
    shortened = state.artifacts.register(**{**values, "mime_type": "text/plain"}, expires_at=now + 100)
    assert shortened["expires_at"] == pytest.approx(now + 205, abs=0.001, rel=0)
    assert shortened["mime_type"] == "application/octet-stream"
    assert state.artifacts.read(namespace, original["artifact_id"])["content"] == content
    with state.engine.connect() as conn:
        assert conn.scalar(select(func.count()).select_from(artifact_snapshots)) == 1
        assert conn.scalar(select(artifact_snapshots.c.created_at)) == created
    with pytest.raises(ArtifactUnavailable):
        state.artifacts.read(namespace + "-other", original["artifact_id"])
    other = state.artifacts.register(**{**values, "namespace": namespace + "-other"})
    assert other["artifact_id"] != original["artifact_id"]


def test_real_redis_snapshot_ttl_nx_namespace_and_exact_key_loss(live_state):
    state = live_state
    snapshots = ResultSnapshots(state, settings=SnapshotSettings(ttl_seconds=30))
    client = state.cache_sync.backend._connection()
    namespace = "redis-" + uuid4().hex
    result_id = str(uuid4())
    payload = {"results": [{"rank": index + 1, "text": f"retained {index}", "score": index / 10}
                           for index in range(5)], "debug": {"retrieval": "embedding"}}
    saved = snapshots.save(namespace, payload, result_id=result_id)
    key = snapshots.transient._key("search-result", result_id)
    assert key.startswith(state.cache_sync.backend.settings.prefix + ":transient:")
    encoded = client.get(key)
    assert json.loads(encoded)["namespace"] == namespace
    assert 0 < client.pttl(key) <= 30_000
    with pytest.raises(SnapshotUnavailable, match="already exists"):
        snapshots.save(namespace, {"results": []}, result_id=result_id)
    assert client.get(key) == encoded
    payload["results"][0]["text"] = "mutated after save"
    assert snapshots.get(namespace, result_id)["results"][0]["text"] == "retained 0"
    assert snapshots.get(namespace, result_id, page=2, page_size=2)["results"] == saved["results"][2:4]
    with pytest.raises(SnapshotUnavailable):
        snapshots.get(namespace + "-other", result_id)

    other = snapshots.save(namespace + "-other", {"results": [{"text": "other tenant"}]})
    other_id = other["result_id"]
    other_key = snapshots.transient._key("search-result", other_id)
    assert other_key != key
    assert snapshots.get(namespace + "-other", other_id)["results"] == other["results"]
    expired = ResultSnapshots(state, settings=snapshots.settings, clock=lambda: saved["expires_at"])
    with pytest.raises(SnapshotUnavailable):
        expired.get(namespace, result_id)
    assert client.exists(key) == 1  # Logical expiry is checked even before Redis eviction.

    assert client.delete(key) == 1  # Eviction is final; SQL never restores result snapshots.
    with pytest.raises(SnapshotUnavailable):
        snapshots.get(namespace, result_id)
    assert client.exists(other_key) == 1
    assert client.pexpire(other_key, 1)
    deadline = time.monotonic() + 3
    while client.exists(other_key) and time.monotonic() < deadline:
        time.sleep(0.01)
    assert client.exists(other_key) == 0
    with pytest.raises(SnapshotUnavailable):
        snapshots.get(namespace + "-other", other_id)


def test_live_upload_search_replay_artifacts_survive_replace_delete_and_redis_loss(
    live_state, live_index, monkeypatch,
):
    state, index = live_state, live_index
    vector = [1.0] + [0.0] * (DENSE_VECTOR_SIZE - 1)
    embedder = SimpleNamespace(
        embed_documents=Mock(side_effect=lambda texts: [list(vector) for _ in texts]),
        embed_query=Mock(return_value=list(vector)),
    )
    ingester = DenseDocumentIngester(
        RagConfig(backend="qdrant", collection=index.collection, namespace=index.namespace),
        embedder, Bm25SparseEncoder(), index.store,
    )
    documents = KnowledgeDocumentStore(state, index.namespace)
    chunking = ChunkingConfig("character", 100, 10)
    name = "retained-source.txt"
    original = ("Original evidence remains immutable after replacement. " * 10).encode()
    first = documents.store_and_index(name, io.BytesIO(original), ingester, chunking)
    assert first["chunk_count"] >= 3
    assert state.uploads.get(first["operation_id"])["status"] == "COMMITTED"
    assert state.cache_sync.backend.enabled  # Real revision Lua/cache writes stay enabled for uploads.
    assert documents.read_source(name) == original

    image = base64.b64decode(
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aN1sAAAAASUVORK5CYII="
    )
    table = {"type": "table", "headers": ["Item", "Value"], "rows": [["retained", "42"]]}

    def project(candidates):
        # Only the public image/table bodies are fixtures; citations and source
        # versions come from the actual uploaded and retrieved Qdrant points.
        results = [candidate.to_result(rank) for rank, candidate in enumerate(candidates, 1)]
        for result, asset_id, asset in zip(results, ("image-fixture", "table-fixture"), (
            {"type": "image", "mime_type": "image/png", "data": base64.b64encode(image).decode()}, table,
        )):
            result["metadata"]["asset_id"] = asset_id
            result["asset"] = asset
        return results

    retriever = DenseRetriever(embedder, index.store)
    search = Mock(wraps=retriever.search)
    reranker = Mock(side_effect=AssertionError("No model reranking is permitted"))
    orchestrator = RagOrchestrator(
        retrievers={"embedding": SimpleNamespace(search=search)},
        rerankers={"cross_encoder": SimpleNamespace(rerank=reranker)},
        implementation="uuid-live-qdrant", projector=project,
    )
    execute = Mock(wraps=orchestrator.search)
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = state
    runtime._inference_lock = RLock()
    runtime._tenant = Mock(return_value=SimpleNamespace(
        lock=RLock(), orchestrator=SimpleNamespace(search=execute),
    ))
    saved = runtime.search(index.namespace, {
        "query": "original evidence", "retrieval": "embedding", "rerank": "none", "top_k": 3,
    })
    assert saved["snapshot_available"] and str(UUID(saved["result_id"])) == saved["result_id"]
    assert len(saved["results"]) == 3
    assert all(hit["document_id"] == first["document_id"] and hit["source"] == name
               and hit["metadata"]["upload_version"] == first["operation_id"] for hit in saved["results"])
    references = {reference["kind"]: reference for reference in saved["artifacts"]}
    assert set(references) == {"source", "image", "table"}
    assert "artifact_warning" not in saved
    for hit in saved["results"]:
        assert hit["artifacts"] == [references["source"]]
    for hit, kind in zip(saved["results"], ("image", "table")):
        assert hit["asset"]["artifact_id"] == references[kind]["artifact_id"]
        assert hit["asset"]["resource_uri"] == references[kind]["resource_uri"]
    source_id = references["source"]["artifact_id"]
    assert runtime.download_artifact(index.namespace, source_id) == (original, "text/plain", name)
    assert runtime.download_artifact(index.namespace, references["image"]["artifact_id"])[0] == image
    assert json.loads(runtime.download_artifact(index.namespace, references["table"]["artifact_id"])[0]) == table

    replacement = b"Replacement evidence is deliberately different. " * 9
    second = documents.store_and_index(name, io.BytesIO(replacement), ingester, chunking)
    assert second["document_id"] == first["document_id"]
    assert second["operation_id"] != first["operation_id"]
    assert documents.read_source(name) == replacement
    points = index.store.dense_search(vector, limit=10, filters={})
    assert points and all(point.payload["upload_version"] == second["operation_id"] for point in points)
    assert runtime.download_artifact(index.namespace, source_id)[0] == original
    documents.delete(name, ingester)
    assert state.documents.get(index.namespace, name) is None
    assert state.blobs.get(index.namespace, SOURCE_BUCKET, name) == replacement
    for operation_id in (first["operation_id"], second["operation_id"]):
        assert state.uploads.get_object(operation_id, SOURCE_BUCKET, name) is not None
    assert index.store.dense_search(vector, limit=10, filters={}) == []

    def no_inference(*_args, **_kwargs):
        raise AssertionError("UUID replay must not construct tenants, search, or execute models")

    runtime._tenant.side_effect = no_inference
    execute.side_effect = no_inference
    search.side_effect = no_inference
    embedder.embed_query.side_effect = no_inference
    embedder.embed_documents.side_effect = no_inference
    query_points = Mock(side_effect=no_inference)
    monkeypatch.setattr(index.client, "query_points", query_points)
    for page in (1, 2):
        replay = runtime.get_search_result(index.namespace, saved["result_id"], page=page, page_size=2)
        assert replay["results"] == saved["results"][(page - 1) * 2:page * 2]
        assert replay["debug"] == saved["debug"]
        assert replay["artifacts"] == saved["artifacts"]
        assert replay["total_results"] == 3
        assert replay["next_page"] == (2 if page == 1 else None)
    for kind, expected in (("source", original), ("image", image)):
        artifact_id = references[kind]["artifact_id"]
        assert runtime.download_artifact(index.namespace, artifact_id)[0] == expected
        envelope = runtime.get_artifact(index.namespace, artifact_id, offset=1, length=7)
        assert base64.b64decode(envelope["data"]) == expected[1:8]
        with pytest.raises(SnapshotUnavailable):
            runtime.download_artifact(index.namespace + "-other", artifact_id)
    with pytest.raises(SnapshotUnavailable):
        runtime.get_search_result(index.namespace + "-other", saved["result_id"])

    client = state.cache_sync.backend._connection()
    key = state.results.transient._key("search-result", saved["result_id"])
    assert client.delete(key) == 1
    with pytest.raises(SnapshotUnavailable):
        runtime.get_search_result(index.namespace, saved["result_id"])
    assert runtime.download_artifact(index.namespace, source_id)[0] == original
    assert runtime.download_artifact(index.namespace, references["image"]["artifact_id"])[0] == image
    assert json.loads(runtime.download_artifact(index.namespace, references["table"]["artifact_id"])[0]) == table

    with state.engine.begin() as conn:
        conn.execute(update(artifact_snapshots).where(
            artifact_snapshots.c.namespace == index.namespace, artifact_snapshots.c.id == source_id,
        ).values(expires_at=0))
    with pytest.raises(SnapshotUnavailable):
        runtime.download_artifact(index.namespace, source_id)
    assert state.artifacts.purge_expired(limit=1) == 1
    with state.engine.connect() as conn:
        assert conn.scalar(select(artifact_snapshots.c.id).where(artifact_snapshots.c.id == source_id)) is None
    assert runtime.download_artifact(index.namespace, references["image"]["artifact_id"])[0] == image
    execute.assert_called_once()
    search.assert_called_once()
    runtime._tenant.assert_called_once_with(index.namespace)
    embedder.embed_query.assert_called_once()
    assert embedder.embed_documents.call_count == 2
    reranker.assert_not_called()
    query_points.assert_not_called()


def test_live_additive_del_flag_upgrade_preserves_existing_mysql_rows(live_state):
    from sqlalchemy import text, inspect
    from rag_mcp.state.schema_migrations import migrate_soft_delete
    state = live_state
    namespace = "migration-" + uuid4().hex
    state.documents.upsert(namespace, {"name": "old.txt", "document_id": "old-doc", "collection": "old"})
    # This fixture schema is disposable and guarded by praxis_uuid_test_*.
    with state.engine.begin() as conn:
        conn.execute(text("ALTER TABLE documents DROP COLUMN del_flag"))
    try:
        migrate_soft_delete(state.engine)
        migrate_soft_delete(state.engine)
        assert "del_flag" in {c["name"] for c in inspect(state.engine).get_columns("documents")}
        assert state.documents.get(namespace, "old.txt")["document_id"] == "old-doc"
        state.documents.delete(namespace, "old.txt")
        assert state.documents.get(namespace, "old.txt") is None
        with state.engine.connect() as conn:
            assert conn.scalar(text("SELECT del_flag FROM documents WHERE namespace=:namespace"),
                               {"namespace": namespace}) == 1
    finally:
        migrate_soft_delete(state.engine)
