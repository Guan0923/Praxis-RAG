"""Opt-in isolated MySQL/Redis/RocketMQ cache delivery validation."""

import json
import os
import time
from uuid import uuid4

import pytest
from sqlalchemy import delete, select
from sqlalchemy.engine import make_url

from rag_mcp.state.cache import CacheSettings, RedisStateCache
from rag_mcp.state.cache_queue import RocketMQCacheQueue, RocketMQSettings
from rag_mcp.state.cache_sync import CacheSynchronizer
from rag_mcp.state.read_views import CachedReadViews
from rag_mcp.state.schema import cache_outbox, metadata
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores

pytestmark = pytest.mark.skipif(not os.getenv("RAG_TEST_CACHE_MYSQL_URL"), reason="Requires disposable cache MySQL schema")


@pytest.fixture()
def live_state(monkeypatch):
    url = os.environ["RAG_TEST_CACHE_MYSQL_URL"]
    assert make_url(url).database.startswith("praxis_cache_test_"), "Disposable schema only"
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    state = StateStores(StateSettings(url=url))
    with state.engine.begin() as conn:
        for table in reversed(metadata.sorted_tables):
            conn.execute(delete(table))
    backend = RedisStateCache(CacheSettings(
        url=os.getenv("RAG_TEST_REDIS_URL", "redis://127.0.0.1:6379/0"),
        prefix="rag:cache-test:" + uuid4().hex, timeout_seconds=1))
    state.cache_sync.close()
    state.cache_sync = CacheSynchronizer(state, backend=backend)
    for repo in (state.api_keys, state.knowledge_bases, state.documents, state.catalogs, state.uploads):
        repo.cache_sync = state.cache_sync
    state.views = CachedReadViews(state)
    try:
        yield state
    finally:
        client = backend._connection()
        from rag_mcp.state.schema import cache_revisions
        with state.engine.connect() as conn:
            keys = list(conn.execute(select(cache_revisions.c.kind, cache_revisions.c.scope)))
        for kind, scope in keys:
            client.delete(*backend._keys(kind, scope))
        state.close()


def config():
    return dict(namespace="ns", collection="c", embedding_model="m", reranker_model="",
                embedding={"base_url": "http://provider", "protected_key": "never-cache"}, reranker={})


def test_mysql_redis_commit_refresh_and_hit_without_database(live_state):
    from sqlalchemy import event
    state = live_state
    state.knowledge_bases.create(config())
    queries = []
    def track(*_):
        queries.append(1)
    event.listen(state.engine, "before_cursor_execute", track)
    try:
        assert state.views.knowledge_base("ns")["collection"] == "c"
        assert state.views.knowledge_bases()[0]["namespace"] == "ns"
        assert state.views.documents("ns") == []
        assert queries == []
    finally:
        event.remove(state.engine, "before_cursor_execute", track)
    assert "never-cache" not in json.dumps(state.cache_sync.backend.get("kb_public", "ns"))
    state.knowledge_bases.delete("ns")
    assert state.views.knowledge_base("ns") is None


def test_real_rocketmq_failed_refresh_redelivery(live_state):
    state = live_state
    prefix = uuid4().hex
    queue = RocketMQCacheQueue(RocketMQSettings(
        endpoint=os.getenv("RAG_TEST_ROCKETMQ_ENDPOINT", "127.0.0.1:8081"),
        topic=os.getenv("RAG_TEST_ROCKETMQ_TOPIC", "praxis_state_cache_refresh"),
        group=os.environ["RAG_TEST_ROCKETMQ_GROUP"]))
    state.cache_sync.transport = queue
    original_put = state.cache_sync.backend.put
    from rag_mcp.state.cache import CacheUnavailable
    def fail(*args, **kwargs):
        raise CacheUnavailable("injected Redis write failure")
    state.cache_sync.backend.put = fail
    state.knowledge_bases.create(config())
    with state.engine.connect() as conn:
        events = [dict(r) for r in conn.execute(select(cache_outbox)).mappings()]
    assert events and all(e["status"] == "QUEUED" for e in events)
    state.cache_sync.backend.put = original_put
    seen = []
    def refresh(event_id):
        seen.append(event_id)
        return state.cache_sync.refresh_event(event_id)
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        queue.consume_once(refresh)
        with state.engine.connect() as conn:
            pending = conn.scalar(select(cache_outbox.c.id).where(cache_outbox.c.status != "DONE").limit(1))
        if pending is None:
            break
    assert seen, prefix
    assert state.views.knowledge_base("ns")["collection"] == "c"
    with state.engine.connect() as conn:
        assert conn.scalar(select(cache_outbox.c.id).where(cache_outbox.c.status != "DONE").limit(1)) is None


def test_real_redis_unreachable_falls_back_to_mysql(live_state):
    state = live_state
    state.knowledge_bases.create(config())
    working = state.cache_sync.backend
    down = RedisStateCache(CacheSettings(url="redis://127.0.0.1:63999/0", timeout_seconds=0.1))
    state.cache_sync.backend = down
    try:
        assert state.views.knowledge_base("ns")["collection"] == "c"
    finally:
        state.cache_sync.backend = working
        down.close()


def test_real_rocketmq_unacked_delivery_retries(live_state):
    queue = RocketMQCacheQueue(RocketMQSettings(
        endpoint=os.getenv("RAG_TEST_ROCKETMQ_ENDPOINT", "127.0.0.1:8081"),
        topic=os.getenv("RAG_TEST_ROCKETMQ_TOPIC", "praxis_state_cache_refresh"),
        group=os.environ["RAG_TEST_ROCKETMQ_GROUP"]))
    event_id = uuid4().hex
    attempts = []
    def callback(received):
        if received != event_id:
            return True
        attempts.append(received)
        return len(attempts) > 1
    try:
        queue.send(event_id)
        deadline = time.monotonic() + 65
        while time.monotonic() < deadline and len(attempts) < 2:
            queue.consume_once(callback)
        assert attempts == [event_id, event_id]
    finally:
        queue.close()


def test_real_mysql_cache_outbox_rolls_back_with_document_transaction(live_state):
    state = live_state
    assert state.views.documents("ns") == []
    with state.engine.connect() as conn:
        before = list(conn.execute(select(cache_outbox.c.id)).scalars())
    with pytest.raises(OSError):
        with state.engine.begin() as conn:
            state.documents.upsert("ns", {"name": "uncommitted.txt", "document_id": "uncommitted", "collection": "c"}, conn=conn)
            raise OSError("abort business write")
    state.cache_sync.flush_pending()
    assert state.views.documents("ns") == []
    assert state.documents.list("ns") == []
    with state.engine.connect() as conn:
        assert list(conn.execute(select(cache_outbox.c.id)).scalars()) == before


def test_second_batch_real_redis_status_hits_and_strict_authority(live_state):
    from sqlalchemy import event, update
    from rag_mcp.api_keys import ApiKeyStore, ApiKeyError
    from rag_mcp.state.schema import document_leases, cache_revisions
    from rag_mcp.state.uploads import LeaseLost
    state = live_state
    keys = ApiKeyStore(state)
    created = keys.create("display-key", ["ns"])
    op = state.uploads.begin("ns", "file.txt", "doc", "c")
    state.uploads.step(op, "parse", lambda: None, {"not_public": "private-details"})
    state.cache_sync.flush_pending()
    assert state.views.upload_operation("ns", op["id"])["status"] == "RUNNING"
    assert state.views.upload_operations("ns")
    assert state.views.leases("ns")
    assert state.views.published_versions("ns") == {}
    assert keys.list()[0]["enabled"]
    calls = []
    def count(*_):
        calls.append(True)
    event.listen(state.engine, "before_cursor_execute", count)
    try:
        assert state.views.upload_operation("ns", op["id"])["steps"][0]["status"] == "DONE"
        assert state.views.upload_operations("ns")
        assert state.views.leases("ns")
        assert state.views.published_versions("ns") == {}
        assert keys.list()[0]["enabled"]
        assert calls == []
    finally:
        event.remove(state.engine, "before_cursor_execute", count)
    backend = state.cache_sync.backend
    original = backend.put
    from rag_mcp.state.cache import CacheUnavailable
    backend.put = lambda *a, **k: (_ for _ in ()).throw(CacheUnavailable("injected"))
    keys.update(created["id"], "disabled", ["ns"], False)
    assert keys.list()[0]["enabled"]  # Only the administration display is stale.
    with pytest.raises(ApiKeyError):
        keys.namespaces_for_key(created["key"])
    backend.put = original
    with state.engine.begin() as conn:
        conn.execute(update(document_leases).where(document_leases.c.operation_id == op["id"]).values(expires_at=0))
    with pytest.raises(LeaseLost):
        state.uploads.assert_owner(op)
    recovered = state.uploads.claim_recovery(op["id"])
    state.uploads.rollback(recovered, lambda _: None)
    state.cache_sync.flush_pending()
    assert state.views.upload_operation("ns", op["id"])["status"] == "ROLLED_BACK"
    assert state.views.upload_operation("other", op["id"]) is None
    # No raw/protected secrets or ownership tokens in any actual Redis payload.
    with state.engine.connect() as conn:
        scopes = list(conn.execute(select(cache_revisions.c.kind, cache_revisions.c.scope)))
    for kind, scope in scopes:
        payload = backend._connection().get(backend._keys(kind, scope)[0]) or b""
        assert created["key"].encode() not in payload
        assert op["owner"].encode() not in payload
        assert b"private-details" not in payload


def test_second_batch_head_commit_and_rollback_cache_follow_mysql(live_state):
    from rag_mcp.state.uploads import LeaseLost
    state = live_state
    op = state.uploads.begin("ns", "file.txt", "doc", "c")
    assert state.views.published_versions("ns") == {}
    with pytest.raises(OSError):
        with state.engine.begin() as conn:
            state.uploads.commit(op, {"ok": True}, conn)
            raise OSError("abort publication")
    state.cache_sync.flush_pending()
    assert state.views.published_versions("ns") == {}
    assert state.uploads.active_versions("ns") == {}
    with state.engine.begin() as conn:
        state.uploads.commit(op, {"ok": True}, conn)
    state.cache_sync.flush_pending()
    assert state.views.published_versions("ns") == {"doc": op["id"]}
    assert state.views.upload_operation("ns", op["id"])["status"] == "COMMITTED"
    assert state.views.leases("ns") == []
    with pytest.raises(LeaseLost):
        state.uploads.rollback(op, lambda _: pytest.fail("active version must not be removed"))


def test_second_batch_actual_upload_refreshes_committed_status_and_heads(live_state, monkeypatch):
    import io
    from types import SimpleNamespace
    from qdrant_client import QdrantClient
    from rag_mcp.chunk_strategies import ChunkingConfig
    from rag_mcp.config import RagConfig
    from rag_mcp.service_knowledge import DenseDocumentIngester, KnowledgeDocumentStore
    from rag_mcp.sparse import Bm25SparseEncoder
    from rag_mcp.storage.qdrant_store import QdrantStore
    state = live_state
    collection = "cache_test_" + uuid4().hex
    client = QdrantClient(url=os.getenv("RAG_TEST_QDRANT_URL", "http://127.0.0.1:6333"))
    store = QdrantStore(collection=collection, client=client, namespace="ns",
                        active_versions=lambda: state.documents.visible_versions("ns"))
    vector = [1.0] + [0.0] * 1023
    ingester = DenseDocumentIngester(RagConfig(backend="qdrant", collection=collection),
        SimpleNamespace(embed_documents=lambda texts: [vector for _ in texts]), Bm25SparseEncoder(), store)
    documents = KnowledgeDocumentStore(state, "ns")
    config = ChunkingConfig("character", 100, 10)
    try:
        first = documents.store_and_index("a.txt", io.BytesIO(b"published"), ingester, config)
        assert state.views.published_versions("ns") == {first["document_id"]: first["operation_id"]}
        detail = state.views.upload_operation("ns", first["operation_id"])
        assert detail["status"] == "COMMITTED"
        assert all(step["status"] == "DONE" for step in detail["steps"])
        assert state.views.leases("ns") == []
        original_commit = state.uploads.commit
        def fail_after_sql(*args, **kwargs):
            original_commit(*args, **kwargs)
            raise OSError("publication aborted")
        monkeypatch.setattr(state.uploads, "commit", fail_after_sql)
        with pytest.raises(OSError):
            documents.store_and_index("a.txt", io.BytesIO(b"not published"), ingester, config)
        assert state.views.published_versions("ns") == {first["document_id"]: first["operation_id"]}
        assert state.views.documents("ns")[0]["operation_id"] == first["operation_id"]
        failed = next(op for op in state.views.upload_operations("ns") if op["status"] == "ROLLED_BACK")
        assert state.views.upload_operation("ns", failed["id"])["status"] == "ROLLED_BACK"
        assert [point.payload["text"] for point in store.dense_search(vector, limit=10, filters={})] == ["published"]
        documents.delete("a.txt", ingester)
        assert state.views.published_versions("ns") == {}
        assert state.views.documents("ns") == []
        assert state.views.upload_operation("ns", first["operation_id"])["status"] == "COMMITTED"
    finally:
        if client.collection_exists(collection):
            client.delete_collection(collection)
        client.close()
