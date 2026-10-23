"""Journal display events share SQL transactions; cached snapshots never fence work."""

import copy
from contextlib import contextmanager
from types import SimpleNamespace

import pytest
from sqlalchemy import create_engine, select, update
from sqlalchemy.pool import StaticPool

from rag_mcp.state.cache import CacheUnavailable
from rag_mcp.state.cache_sync import CacheSynchronizer
from rag_mcp.state.read_views import CachedReadViews
from rag_mcp.state.schema import (
    cache_outbox, cache_revisions, document_leases, metadata, upload_operations,
)
from rag_mcp.state.upload_coordinator import UploadCoordinator
from rag_mcp.state.uploads import LeaseLost, UploadJournal


class MemoryCache:
    enabled = True

    def __init__(self):
        self.entries = {}
        self.down = False

    def get(self, kind, scope):
        if self.down:
            raise CacheUnavailable("offline")
        key = kind, scope
        return key in self.entries, copy.deepcopy(self.entries.get(key))

    def put(self, kind, scope, revision, value, deleted=False):
        if self.down:
            raise CacheUnavailable("offline")
        self.entries[kind, scope] = copy.deepcopy(value)
        return True

    def close(self):
        pass


@pytest.fixture()
def state(monkeypatch):
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    engine = create_engine("sqlite://", poolclass=StaticPool,
                           connect_args={"check_same_thread": False})
    metadata.create_all(engine)
    value = SimpleNamespace(engine=engine)
    value.cache_sync = CacheSynchronizer(value, backend=MemoryCache())
    value.views = CachedReadViews(value)
    value.uploads = UploadJournal(value, auto_heartbeat=False, lease_seconds=300,
                                  cleanup_delay_seconds=0)
    yield value
    value.uploads.close()
    value.cache_sync.close()
    engine.dispose()


def begin(state, **kwargs):
    return state.uploads.begin("ns", "file.txt", "doc", "collection", **kwargs)


def revisions(state):
    with state.engine.connect() as conn:
        return {(row.kind, row.scope): row.revision
                for row in conn.execute(select(cache_revisions))}


def events(state):
    with state.engine.connect() as conn:
        return [dict(row) for row in conn.execute(select(cache_outbox)).mappings()]


def commit(state, op):
    with state.engine.begin() as conn:
        state.uploads.commit(op, {"ok": True}, conn)
    state.uploads._flush_cache("ns", op["id"], heads=True, leases=True)


def status(state, op):
    return state.views.upload_operation("ns", op["id"])


def expire(state, op):
    with state.engine.begin() as conn:
        conn.execute(update(document_leases).where(
            document_leases.c.operation_id == op["id"]).values(expires_at=0))


def test_begin_steps_intent_and_noop_refresh_display(state):
    op = begin(state, idempotency_key="once")
    assert status(state, op)["status"] == "INTENT"
    assert state.views.upload_operations("ns")[0]["id"] == op["id"]
    assert set(revisions(state)) == {("upload_status", op["id"]),
                                     ("upload_list", "ns"), ("leases", "ns")}
    observed = []
    state.uploads.run_step(op, "parse", lambda: observed.append(status(state, op)))
    assert observed[0]["steps"][0]["status"] == "RUNNING"
    assert status(state, op)["steps"][0]["status"] == "DONE"
    state.uploads.intent(op, "publish")
    state.uploads.commit_noop(op, {"ok": True})
    assert status(state, op)["status"] == "COMMITTED"
    assert state.views.leases("ns") == []
    before = events(state)
    assert begin(state, idempotency_key="once")["id"] == op["id"]
    assert events(state) == before
    assert ("heads", "ns") not in revisions(state)


def test_fail_and_rollback_transitions_are_committed_before_callbacks(state):
    op = begin(state)

    def fail_action():
        raise OSError("private detail")

    with pytest.raises(OSError):
        state.uploads.run_step(op, "parse", fail_action)
    assert status(state, op)["status"] == "FAILED"
    assert status(state, op)["steps"][0]["status"] == "FAILED"

    def fail_rollback(current):
        assert status(state, op)["status"] == "ROLLING_BACK"
        raise OSError("retry")

    with pytest.raises(OSError):
        state.uploads.rollback(op, fail_rollback)
    assert status(state, op)["status"] == "ROLLBACK_FAILED"
    state.uploads.rollback(op, lambda current: None)
    assert status(state, op)["status"] == "ROLLED_BACK"
    assert state.views.leases("ns") == []
    before = events(state)
    with pytest.raises(LeaseLost):
        state.uploads.rollback(op, lambda current: pytest.fail("unauthorized callback"))
    assert events(state) == before


def test_publish_outbox_heads_and_leases_rollback_together(state, monkeypatch):
    op = begin(state)
    before_events, before_revisions = events(state), revisions(state)
    flushes = []
    monkeypatch.setattr(state.cache_sync, "flush_pending", lambda **kw: flushes.append(kw))
    with pytest.raises(RuntimeError):
        with state.engine.begin() as conn:
            state.uploads.commit(op, {"ok": True}, conn)
            assert flushes == []
            raise RuntimeError("abort")
    assert events(state) == before_events
    assert revisions(state) == before_revisions
    assert state.uploads.active_versions("ns") == {}
    assert state.uploads.get(op["id"])["status"] == "INTENT"
    state.uploads.assert_owner(op)
    with state.engine.begin() as conn:
        state.uploads.commit(op, {"ok": True}, conn)
    assert flushes == []  # The publication service, not commit(), owns the flush.
    assert state.uploads.active_versions("ns") == {"doc": op["id"]}
    assert revisions(state)["heads", "ns"] == 1


def test_renewal_changes_only_lease_revision_and_authority_reads_emit_nothing(state, monkeypatch):
    op = begin(state)
    before = revisions(state)
    state.uploads._renew(op)
    after = revisions(state)
    assert after["leases", "ns"] == before["leases", "ns"] + 1
    assert after["upload_status", op["id"]] == before["upload_status", op["id"]]
    assert after["upload_list", "ns"] == before["upload_list", "ns"]
    before_events = events(state)
    monkeypatch.setattr(state.cache_sync, "get_or_load", lambda *args, **kw:
                        pytest.fail("authority reads must not consult cache"))
    state.uploads.get(op["id"])
    state.uploads.unfinished("ns")
    state.uploads.active_versions("ns")
    state.uploads.assert_owner(op)
    assert state.uploads.claim_recovery(op["id"]) is None
    assert events(state) == before_events


def test_cached_expiration_timestamps_do_not_grant_ownership(state, monkeypatch):
    op = begin(state)
    leases = state.views.leases("ns")
    assert leases and all(not row["expired"] for row in leases)
    deadline = max(row["expires_at"] for row in leases)
    monkeypatch.setattr("rag_mcp.state.read_views.time.time", lambda: deadline + 1)
    assert all(row["expired"] for row in state.views.leases("ns"))
    # Conversely a stale, unexpired display cannot authorize an expired SQL lease.
    monkeypatch.setattr("rag_mcp.state.read_views.time.time", lambda: deadline - 1)
    expire(state, op)
    assert all(not row["expired"] for row in state.views.leases("ns"))
    before = events(state)
    with pytest.raises(LeaseLost):
        state.uploads.assert_owner(op)
    assert events(state) == before
    claimed = state.uploads.claim_recovery(op["id"])
    assert claimed["owner"] != op["owner"]
    assert status(state, op)["status"] == "ROLLING_BACK"
    with pytest.raises(LeaseLost):
        state.uploads.assert_owner(op)
    state.uploads.assert_owner(claimed)


def test_generic_lock_events_and_empty_reads(state):
    state.uploads.active_versions("empty")
    state.uploads.unfinished("empty")
    assert events(state) == []
    token = state.uploads.acquire_lock("ns")
    assert revisions(state) == {("leases", "ns"): 1}
    before = events(state)
    state.uploads.assert_lock(token)
    assert events(state) == before
    state.uploads.renew_lock(token)
    assert revisions(state)["leases", "ns"] == 2
    state.uploads.release_lock(token)
    assert state.views.leases("ns") == []
    before = events(state)
    state.uploads.release_lock(token)
    assert events(state) == before


def test_cleanup_head_removal_and_purge_events(state):
    first = begin(state)
    commit(state, first)
    second = begin(state)
    commit(state, second)
    assert status(state, second)["cleanup_pending"]
    state.uploads.cleanup(second, lambda current: None)
    assert not status(state, second)["cleanup_pending"]
    assert state.views.published_versions("ns") == {"doc": second["id"]}
    third = begin(state)
    commit(state, third)
    fourth = begin(state)
    commit(state, fourth)
    token = state.uploads.acquire_lock("ns")
    before = events(state)
    with pytest.raises(RuntimeError):
        with state.engine.begin() as conn:
            state.uploads.remove_head("ns", "doc", conn, token)
            state.uploads.purge_document_objects("ns", "doc", conn, token)
            raise RuntimeError("abort delete")
    assert events(state) == before
    assert state.uploads.head("ns", "doc") == fourth["id"]
    assert state.uploads.get(third["id"])["cleanup_pending"]
    with state.engine.begin() as conn:
        state.uploads.remove_head("ns", "doc", conn, token)
        affected_ids = state.uploads.purge_document_objects("ns", "doc", conn, token)
    assert isinstance(affected_ids, tuple)
    assert set(affected_ids) == {third["id"], fourth["id"]}
    state.cache_sync.flush_pending(keys=(("upload_list", "ns"), ("heads", "ns"), *(
        ("upload_status", operation_id) for operation_id in affected_ids)))
    assert state.views.published_versions("ns") == {}
    assert not status(state, third)["cleanup_pending"]
    assert not status(state, fourth)["cleanup_pending"]
    before = events(state)
    with state.engine.begin() as conn:
        assert state.uploads.purge_document_objects("ns", "doc", conn, token) == ()
    assert events(state) == before
    state.uploads.release_lock(token)
    assert all(row["status"] == "DONE" for row in events(state))


def test_coordinator_workspace_and_cleanup_markers(state, monkeypatch):
    op = begin(state)
    state.uploads.register_workspace(op, "registered-workspace")
    commit(state, op)
    coordinator = UploadCoordinator(state)
    monkeypatch.setattr(coordinator, "_remove_workspace", lambda current: True)
    coordinator.cleanup_workspace(op)
    assert state.uploads.get(op["id"])["workspace"] is None
    before = events(state)
    coordinator._clear_workspace(op)
    assert events(state) == before

    def fail_workspace(current):
        raise OSError("workspace unavailable")

    monkeypatch.setattr(coordinator, "_remove_workspace", fail_workspace)
    coordinator.cleanup(op, SimpleNamespace())
    assert status(state, op)["cleanup_pending"]
    before = events(state)
    coordinator.cleanup(op, SimpleNamespace())
    assert events(state) == before


def test_cache_outage_keeps_committed_events_retryable(state):
    state.cache_sync.backend.down = True
    op = begin(state)
    state.uploads.fail(op)
    assert state.uploads.get(op["id"])["status"] == "FAILED"
    assert any(row["status"] != "DONE" for row in events(state))
    state.cache_sync.backend.down = False
    state.cache_sync.flush_pending()
    assert status(state, op)["status"] == "FAILED"
    assert all(row["status"] == "DONE" for row in events(state))


def test_engine_only_state_works_without_cache_synchronizer(state):
    journal = UploadJournal(SimpleNamespace(engine=state.engine), auto_heartbeat=False)
    op = journal.begin("other", "other.txt", "other", "collection")
    journal.commit_noop(op, {})
    assert journal.get(op["id"])["status"] == "COMMITTED"
    assert ("upload_status", op["id"]) in revisions(state)


def test_owned_flush_occurs_after_sql_commit(state, monkeypatch):
    observed = []
    transaction = {"active": False}
    original_begin = state.engine.begin

    @contextmanager
    def tracked_begin():
        transaction["active"] = True
        try:
            with original_begin() as conn:
                yield conn
        finally:
            transaction["active"] = False

    monkeypatch.setattr(state.engine, "begin", tracked_begin)

    def flush(*, keys):
        assert not transaction["active"]
        with state.engine.connect() as conn:
            observed.append((keys, conn.scalar(select(upload_operations.c.status))))

    monkeypatch.setattr(state.cache_sync, "flush_pending", flush)
    op = begin(state)
    assert observed[-1][1] == "INTENT"
    state.uploads.fail(op)
    assert observed[-1][1] == "FAILED"
    assert observed[-1][0] == tuple(sorted(observed[-1][0]))
