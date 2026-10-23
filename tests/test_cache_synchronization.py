"""Outbox/revision fault tests: cache hits never inspect MySQL."""

import copy

import pytest
from sqlalchemy import event, select, update

from rag_mcp.state.cache import CacheUnavailable
from rag_mcp.state.schema import cache_outbox, cache_revisions
from rag_mcp.state.store import StateStores


class MemoryCache:
    enabled = True

    def __init__(self):
        self.entries = {}
        self.watermarks = {}
        self.down = False
        self.closed = False

    def get(self, kind, scope):
        if self.down:
            raise CacheUnavailable("unreachable")
        key = kind, scope
        return key in self.entries, copy.deepcopy(self.entries.get(key))

    def put(self, kind, scope, revision, value, deleted=False):
        if self.down:
            raise CacheUnavailable("unreachable")
        key = kind, scope
        if revision < self.watermarks.get(key, -1):
            return False
        self.watermarks[key] = revision
        self.entries[key] = copy.deepcopy(value)
        return True

    def close(self):
        self.closed = True


class Queue:
    def __init__(self):
        self.messages = []
        self.down = False

    def send(self, event_id):
        if self.down:
            raise RuntimeError("queue unavailable")
        self.messages.append(event_id)

    def consume_once(self, callback):
        pending, self.messages = self.messages, []
        for event_id in pending:
            if not callback(event_id):
                self.messages.append(event_id)

    def close(self):
        pass


@pytest.fixture()
def state(tmp_path, monkeypatch):
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    value = StateStores.create(tmp_path)
    value.cache_sync.backend = MemoryCache()
    value.cache_sync.transport = Queue()
    yield value
    value.close()


def kb(namespace="ns", collection="c"):
    return dict(namespace=namespace, collection=collection, embedding_model="model", reranker_model="",
                embedding={"base_url": "http://provider", "protected_key": "private-file:NEVER_CACHE"},
                reranker={}, strategy="character", chunk_size=100, chunk_overlap=10)


def rows(state):
    with state.engine.connect() as conn:
        return [dict(row) for row in conn.execute(select(cache_outbox)).mappings()]


def test_cache_hit_reads_no_sql_and_configuration_is_redacted(state):
    state.knowledge_bases.create(kb())
    executed = []
    def count(*_):
        executed.append(True)
    event.listen(state.engine, "before_cursor_execute", count)
    try:
        assert state.views.knowledge_base("ns")["embedding"] == {
            "base_url": "http://provider", "api_key_set": True}
        assert len(state.views.knowledge_bases()) == 1
        assert state.views.documents("ns") == []
        assert executed == []
        assert "NEVER_CACHE" not in repr(state.cache_sync.backend.entries)
    finally:
        event.remove(state.engine, "before_cursor_execute", count)


def test_write_refreshes_values_and_delete_is_versioned(state):
    state.knowledge_bases.create(kb())
    first = state.cache_sync.backend.watermarks["kb_public", "ns"]
    state.knowledge_bases.update(kb(collection="updated"))
    assert state.views.knowledge_base("ns")["collection"] == "updated"
    state.knowledge_bases.delete("ns")
    assert state.views.knowledge_base("ns") is None
    assert not state.cache_sync.backend.put("kb_public", "ns", first, kb())
    assert state.views.knowledge_base("ns") is None
    assert all(row["status"] == "DONE" for row in rows(state))


def test_business_transaction_rollback_also_rolls_back_outbox(state):
    before = rows(state)
    with pytest.raises(OSError):
        with state.engine.begin() as conn:
            state.documents.upsert("ns", {"name": "a", "document_id": "doc", "collection": "c"}, conn=conn)
            raise OSError("abort")
    assert rows(state) == before
    assert state.documents.list("ns") == []


def test_redis_failure_queues_only_event_id_then_consumer_refreshes_latest(state):
    state.cache_sync.backend.down = True
    state.knowledge_bases.create(kb())
    assert state.knowledge_bases.get("ns") is not None
    assert all(row["status"] == "QUEUED" for row in rows(state))
    assert all(len(message) == 32 for message in state.cache_sync.transport.messages)
    state.knowledge_bases.update(kb(collection="latest"))
    state.cache_sync.backend.down = False
    # At-least-once and out-of-order callbacks load the latest committed snapshot.
    for message in reversed(state.cache_sync.transport.messages * 2):
        assert state.cache_sync.refresh_event(message)
    assert state.views.knowledge_base("ns")["collection"] == "latest"
    assert all(row["status"] == "DONE" for row in rows(state))


def test_both_redis_and_queue_failures_leave_durable_retry(state):
    state.cache_sync.backend.down = True
    state.cache_sync.transport.down = True
    state.knowledge_bases.create(kb())
    assert all(row["status"] == "PENDING" for row in rows(state))
    restarted = StateStores(state.settings)
    try:
        restarted.cache_sync.backend = MemoryCache()
        with restarted.engine.begin() as conn:
            conn.execute(update(cache_outbox).values(retry_at=0))
        restarted.cache_sync.flush_pending()
        assert restarted.views.knowledge_base("ns")["collection"] == "c"
        assert all(row["status"] == "DONE" for row in rows(restarted))
    finally:
        restarted.close()


def test_stale_miss_cannot_overwrite_newer_write(state):
    state.knowledge_bases.create(kb())
    backend = state.cache_sync.backend
    old_revision = backend.watermarks["kb_public", "ns"]
    old_value = state.views.knowledge_base("ns")
    state.knowledge_bases.update(kb(collection="new"))
    # A previously paused cache-miss SQL snapshot completes after refresh.
    assert backend.put("kb_public", "ns", old_revision, old_value) is False
    assert state.views.knowledge_base("ns")["collection"] == "new"


def test_upload_publication_rollback_never_refreshes_uncommitted_list(state):
    assert state.views.documents("ns") == []
    with pytest.raises(OSError):
        with state.engine.begin() as conn:
            state.documents.upsert("ns", {"name": "a", "document_id": "doc", "collection": "c"}, conn=conn)
            raise OSError("publish failure")
    state.cache_sync.flush_pending()
    assert state.views.documents("ns") == []
    with state.engine.connect() as conn:
        assert conn.scalar(select(cache_revisions.c.revision).where(
            cache_revisions.c.kind == "documents", cache_revisions.c.scope == "ns")) == 0


def test_lifespan_stop_closes_queue_clients(state):
    queue = state.cache_sync.transport
    closed = []
    queue.close = lambda: closed.append(True)
    state.cache_sync.start()
    state.cache_sync.stop()
    assert closed and state.cache_sync._worker is None


def test_synchronous_write_refresh_not_skipped_while_relay_is_active(state):
    state.knowledge_bases.create(kb())
    with state.cache_sync._mutex:
        state.knowledge_bases.update(kb(collection="synchronously-updated"))
        assert state.views.knowledge_base("ns")["collection"] == "synchronously-updated"
