"""Upload journal tests use only local SQLite and version-scoped fake callbacks."""

import threading
import time
from concurrent.futures import ThreadPoolExecutor

import pytest
from sqlalchemy import select, update
from sqlalchemy.dialects import mysql
from sqlalchemy.schema import CreateTable

from rag_mcp.state.schema import document_leases, upload_objects, upload_operations
from rag_mcp.state.store import StateStores
from rag_mcp.state.uploads import LeaseLost, UploadConflict, UploadJournal


@pytest.fixture()
def state(tmp_path, monkeypatch):
    monkeypatch.delenv("RAG_STATE_URL", raising=False)
    monkeypatch.delenv("RAG_STATE_BACKEND", raising=False)
    state = StateStores.create(tmp_path)
    yield state
    state.close()


@pytest.fixture()
def journal(state):
    journal = UploadJournal(state, lease_seconds=5, auto_heartbeat=False,
                            cleanup_delay_seconds=0)
    yield journal
    journal.close()


def begin(journal, **kwargs):
    return journal.begin("ns", "file.txt", "doc", "collection", **kwargs)


def commit(state, journal, op, result=None):
    with state.engine.begin() as conn:
        return journal.commit(op, result or {"ok": True}, conn)


def expire(state, op):
    with state.engine.begin() as conn:
        conn.execute(update(document_leases).where(
            document_leases.c.operation_id == op["id"]
        ).values(expires_at=0))


def test_intent_steps_workspace_and_immutable_objects(state, journal):
    op = begin(journal)
    assert len(op["id"]) == 32 and op["id"] == op["version"]
    assert journal.get(op["id"])["status"] == "INTENT"
    journal.register_workspace(op, "workspace\\" + op["id"], node="node-one")
    saved = journal.get(op["id"])
    assert saved["workspace"] == op["workspace"]
    assert saved["workspace_node"] == "node-one"
    with pytest.raises(UploadConflict):
        journal.register_workspace(op, "other")

    def action():
        persisted = journal.get(op["id"])
        assert persisted["status"] == "RUNNING"
        assert persisted["steps"][0]["status"] == "RUNNING"
        journal.put_object(op, "source", "file.txt", b"hello", metadata={"ref": "source"})
        return 42

    assert journal.step(op, "source", action, {"key": "file.txt"}) == 42
    assert journal.get(op["id"])["steps"][0]["status"] == "DONE"
    journal.put_object(op, "source", "file.txt", b"hello", metadata={"ref": "source"})
    with pytest.raises(UploadConflict, match="immutable"):
        journal.put_object(op, "source", "file.txt", b"changed")
    with pytest.raises(UploadConflict):
        journal.run_step(op, "source", lambda: pytest.fail("must not replay"))
    assert journal.get_object(op["id"], "source", "file.txt") == b"hello"
    assert journal.get_object(op["id"], "source", "missing") is None
    with state.engine.connect() as conn:
        assert isinstance(conn.scalar(select(upload_operations.c.created_at)), float)


def test_commit_atomic_with_documents_blobs_and_replay(state, journal):
    op = begin(journal, idempotency_key="key", fingerprint="sha")
    journal.put_object(op, "source", "file.txt", b"new")
    with pytest.raises(OSError):
        with state.engine.begin() as conn:
            state.blobs.put("ns", "source", "file.txt", b"new", conn=conn)
            journal.commit(op, {"file": "file.txt"}, conn)
            raise OSError("rollback caller transaction")
    assert journal.head("ns", "doc") is None
    assert journal.get(op["id"])["status"] == "INTENT"
    assert state.blobs.get("ns", "source", "file.txt") is None
    with state.engine.begin() as conn:
        state.documents.upsert("ns", {"name": "file.txt", "document_id": "doc",
                                      "collection": "collection"}, conn=conn)
        state.blobs.put("ns", "source", "file.txt", b"new", conn=conn)
        journal.commit(op, {"file": "file.txt"}, conn)
    replay = begin(journal, idempotency_key="key", fingerprint="sha")
    assert replay["id"] == op["id"] and replay["status"] == "COMMITTED"
    assert replay["result"] == {"file": "file.txt"}
    assert journal.active_versions("ns") == {"doc": op["id"]}
    assert state.documents.get("ns", "file.txt")["document_id"] == "doc"
    assert journal.unfinished() == []
    for fingerprint in ("different",):
        with pytest.raises(UploadConflict):
            begin(journal, idempotency_key="key", fingerprint=fingerprint)
    with pytest.raises(LeaseLost):
        journal.rollback(op, lambda _: pytest.fail("committed callback"))
    assert journal.get_object(op["id"], "source", "file.txt") == b"new"


def test_document_namespace_and_name_locks(journal):
    op = begin(journal)
    for action in (
        lambda: begin(journal),
        lambda: journal.begin("ns", "other.txt", "doc", "collection"),
        lambda: journal.begin("ns", "file.txt", "other", "collection"),
        lambda: journal.acquire_lock("ns"),
        lambda: journal.acquire_lock("ns", "doc"),
    ):
        with pytest.raises(UploadConflict):
            action()
    other = journal.begin("ns", "other.txt", "other", "collection")
    journal.rollback(other, lambda _: None)
    journal.rollback(op, lambda _: None)
    with journal.lock("ns") as token:
        journal.assert_lock(token)
        with pytest.raises(UploadConflict):
            begin(journal)
    begin(journal)


def test_expired_recovery_fences_old_worker_and_preserves_active(state, journal):
    first = begin(journal)
    journal.put_object(first, "source", "file.txt", b"active")
    commit(state, journal, first)
    failed = begin(journal, idempotency_key="failed")
    journal.put_object(failed, "source", "file.txt", b"staged")
    assert failed["old_version"] == first["id"]
    assert journal.claim_recovery(failed["id"]) is None
    with pytest.raises(UploadConflict):
        begin(journal, idempotency_key="failed")
    expire(state, failed)
    recovery = journal.claim_recovery(failed["id"])
    assert recovery["owner"] != failed["owner"]
    for action in (
        lambda: journal.assert_owner(failed),
        lambda: journal.run_step(failed, "late", lambda: pytest.fail("stale action")),
        lambda: journal.put_object(failed, "source", "x", b"bad"),
        lambda: commit(state, journal, failed),
        lambda: journal.rollback(failed, lambda _: pytest.fail("stale cleanup")),
    ):
        with pytest.raises(LeaseLost):
            action()
    seen = []
    journal.rollback(recovery, lambda op: seen.append(
        (op["collection"], op["document_id"], op["version"])))
    assert seen == [("collection", "doc", failed["id"])]
    assert journal.active_versions("ns") == {"doc": first["id"]}
    assert journal.get_object(first["id"], "source", "file.txt") == b"active"
    assert journal.get_object(failed["id"], "source", "file.txt") is None
    assert journal.claim_recovery(failed["id"]) is None


def test_failures_are_audited_and_rollback_retries(state, journal):
    op = begin(journal)
    journal.put_object(op, "source", "file.txt", b"staged")

    def fail():
        raise OSError("secret must not be persisted")

    with pytest.raises(OSError):
        journal.run_step(op, "network", fail)
    saved = journal.get(op["id"])
    assert saved["status"] == "FAILED"
    assert saved["error"] == "OSError"
    assert saved["steps"][0]["status"] == "FAILED"
    assert journal.claim_recovery(op["id"]) is None  # FAILED is not permission to steal live work.
    with pytest.raises(OSError):
        journal.rollback(op, lambda _: fail())
    assert journal.get(op["id"])["status"] == "ROLLBACK_FAILED"
    assert journal.get_object(op["id"], "source", "file.txt") == b"staged"
    expire(state, op)
    recovery = journal.claim_recovery(op["id"])
    journal.rollback(recovery, lambda _: None)
    assert journal.get(op["id"])["status"] == "ROLLED_BACK"
    assert journal.get(op["id"])["steps"][0]["status"] == "FAILED"


def test_fence_after_side_effect_keeps_uncertain_step_for_recovery(state, journal):
    op = begin(journal)

    def action():
        expire(state, op)
        assert journal.claim_recovery(op["id"]) is not None

    with pytest.raises(LeaseLost):
        journal.run_step(op, "uncertain", action)
    assert journal.get(op["id"])["steps"][0]["status"] == "RUNNING"


def test_commit_heartbeat_scope_and_cleanup_retry(state, journal):
    first = begin(journal)
    journal.put_object(first, "source", "file.txt", b"old")
    with journal.heartbeat(first):
        commit(state, journal, first)
    second = begin(journal)
    journal.put_object(second, "source", "file.txt", b"new")
    commit(state, journal, second)
    assert journal.old_version(second) == first["id"]
    assert journal.unfinished("ns")[0]["cleanup_pending"] is True

    def unavailable(_):
        raise OSError("retry later")

    with pytest.raises(OSError):
        journal.cleanup(second, unavailable)
    assert journal.get(second["id"])["cleanup_pending"] is True
    seen = []
    journal.cleanup(second, lambda op: seen.append(op["old_version"]))
    journal.cleanup(second, lambda _: pytest.fail("already clean"))
    assert seen == [first["id"]]
    assert journal.get_object(first["id"], "source", "file.txt") is None
    assert journal.get_object(second["id"], "source", "file.txt") == b"new"
    assert journal.head("ns", "doc") == second["id"]
    assert journal.unfinished() == []


def test_default_cleanup_delay(state, journal):
    first = begin(journal)
    commit(state, journal, first)
    second = begin(journal)
    commit(state, journal, second)
    delayed = UploadJournal(state, auto_heartbeat=False)
    try:
        assert delayed.cleanup_delay_seconds >= 3600
        assert delayed.cleanup(second, lambda _: pytest.fail("too early"))["cleanup_pending"]
    finally:
        delayed.close()


def test_heartbeat_renews_during_long_callback(state):
    journal = UploadJournal(state, lease_seconds=0.3, auto_heartbeat=False)
    try:
        op = begin(journal)

        def action():
            time.sleep(0.7)
            assert journal.claim_recovery(op["id"]) is None
            return "ok"

        assert journal.run_step(op, "slow", action) == "ok"
        journal.rollback(op, lambda _: None)
    finally:
        journal.close()


def test_automatic_heartbeat_covers_work_between_steps(state):
    journal = UploadJournal(state, lease_seconds=0.3)
    try:
        op = begin(journal)
        time.sleep(0.7)
        assert journal.claim_recovery(op["id"]) is None
        journal.assert_owner(op)
        journal.rollback(op, lambda _: None)
    finally:
        journal.close()


def test_durable_restart_and_concurrent_begin(state, journal):
    barrier = threading.Barrier(2)

    def race():
        barrier.wait()
        try:
            return begin(journal, idempotency_key="same")
        except UploadConflict:
            return None

    with ThreadPoolExecutor(max_workers=2) as workers:
        results = list(workers.map(lambda _: race(), range(2)))
    assert sum(op is not None for op in results) == 1
    op = next(item for item in results if item)
    second_state = StateStores(state.settings)
    restarted = UploadJournal(second_state, auto_heartbeat=False)
    try:
        assert restarted.get(op["id"])["status"] == "INTENT"
        assert restarted.claim_recovery(op["id"]) is None
        expire(state, op)
        recovery = restarted.claim_recovery(op["id"])
        restarted.rollback(recovery, lambda _: None)
        assert journal.get(op["id"])["status"] == "ROLLED_BACK"
    finally:
        restarted.close()
        second_state.close()


def test_remove_head_requires_covering_lock_and_transaction(state, journal):
    op = begin(journal)
    commit(state, journal, op)
    with journal.lock("other") as token:
        with state.engine.begin() as conn, pytest.raises(LeaseLost):
            journal.remove_head("ns", "doc", conn, token)
    with journal.lock("ns") as token:
        with pytest.raises(OSError):
            with state.engine.begin() as conn:
                journal.remove_head("ns", "doc", conn, token)
                raise OSError()
        assert journal.head("ns", "doc") == op["id"]
        with state.engine.begin() as conn:
            journal.remove_head("ns", "doc", conn, token)
    assert journal.active_versions("ns") == {}


def test_objects_roll_back_and_mysql_blob_compiles(state, journal):
    op = begin(journal)
    with pytest.raises(OSError):
        with state.engine.begin() as conn:
            journal.put_object(op, "source", "file.txt", b"bytes", conn=conn)
            raise OSError()
    assert journal.get_object(op["id"], "source", "file.txt") is None
    ddl = str(CreateTable(upload_objects).compile(dialect=mysql.dialect()))
    assert "LONGBLOB" in ddl
    assert "utf8mb4_bin" in ddl
