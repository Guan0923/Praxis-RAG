"""Delivery boundaries: SQL lease, duplicate workers, failures and dead letters.

Everything here runs against the real SQLite state database with the in-memory
broker stand-in from :mod:`test_document_tasks`; no worker shares Redis state.
"""

import threading
import time
from concurrent.futures import ThreadPoolExecutor
from uuid import uuid4

import pytest
from sqlalchemy import select, update

from rag_mcp.document_worker import DocumentWorker
from rag_mcp.state.document_jobs import heartbeat_seconds
from rag_mcp.state.schema import document_job_outbox, document_jobs
from rag_mcp.state.uploads import UploadConflict
from test_document_tasks import build_service, submit, use_transport


@pytest.fixture
def service(tmp_path, monkeypatch):
    state, queue, ingester, runtime = build_service(tmp_path, monkeypatch)
    try:
        yield state, queue, ingester, runtime
    finally:
        state.close()


def outbox_rows(state):
    with state.engine.connect() as conn:
        return [dict(row) for row in conn.execute(select(document_job_outbox)).mappings()]


def expire_lease(state, artifact_id):
    with state.engine.begin() as conn:
        conn.execute(update(document_jobs).where(
            document_jobs.c.artifact_id == artifact_id).values(lease_until=0.0))


def test_lease_defers_a_second_worker_until_it_expires(service):
    """A live SQL lease keeps a duplicate delivery unacknowledged."""

    state, queue, ingester, runtime = service
    artifact_id = submit(service, b"leased")
    envelope = queue.sent[0]
    task = state.document_jobs.claim(envelope)
    assert task is not None and task["execution_id"]
    assert DocumentWorker(runtime).process(envelope) is False
    assert ingester.calls == []
    row = state.document_jobs.get(artifact_id)
    assert row["status"] == "processing" and row["execution_id"] == task["execution_id"]
    expire_lease(state, artifact_id)
    assert DocumentWorker(runtime).process(envelope) is True
    assert ingester.calls == [b"leased"]
    assert state.document_jobs.get(artifact_id)["status"] == "succeeded"


def test_independent_workers_do_not_run_duplicate_parser(service):
    """Two workers racing on one delivery produce a single ingest."""

    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    envelope = queue.sent[0]
    entered, release = threading.Event(), threading.Event()
    original = ingester.ingest

    def slow(*args, **kwargs):
        entered.set()
        assert release.wait(5)
        return original(*args, **kwargs)

    ingester.ingest = slow
    first, second = DocumentWorker(runtime), DocumentWorker(runtime)
    with ThreadPoolExecutor(max_workers=1) as pool:
        running = pool.submit(first.process, envelope)
        try:
            assert entered.wait(3)
            assert second.process(envelope) is False
            assert state.document_jobs.dead_letter(envelope) is False
        finally:
            release.set()
        assert running.result(timeout=5) is True
    assert len(ingester.calls) == 1
    assert second.process(envelope) is True
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_processing_job_is_not_finalized_by_dead_letter_while_lease_is_live(service):
    state, queue, _ingester, _runtime = service
    artifact_id = submit(service)
    envelope = queue.sent[0]
    task = state.document_jobs.claim(envelope)
    assert state.document_jobs.dead_letter(envelope) is False
    row = state.document_jobs.get(artifact_id)
    assert row["status"] == "processing" and row["execution_id"] == task["execution_id"]
    expire_lease(state, artifact_id)
    assert state.document_jobs.dead_letter(envelope) is True
    status = state.document_jobs.status("manuals", artifact_id)
    assert status["status"] == "failed" and status["error"] == "Message retry limit reached"


def test_stale_execution_cannot_progress_fail_or_reset_the_current_one(service):
    """An expired execution identity loses every write race to its successor."""

    state, queue, _ingester, _runtime = service
    artifact_id = submit(service)
    envelope = queue.sent[0]
    stale = state.document_jobs.claim(envelope)
    expire_lease(state, artifact_id)
    current = state.document_jobs.claim(envelope)
    assert current is not None and current["execution_id"] != stale["execution_id"]
    with pytest.raises(UploadConflict, match="no longer current|not current"):
        state.document_jobs.progress(stale, "indexing")
    with pytest.raises(UploadConflict):
        state.document_jobs.fail(stale, retry=False, error="stale failure")
    state.document_jobs.fail(stale, retry=True, error="stale retry")
    row = state.document_jobs.get(artifact_id)
    assert row["execution_id"] == current["execution_id"]
    assert row["status"] == "processing"
    state.document_jobs.progress(current, "indexing")
    assert state.document_jobs.get(artifact_id)["stage"] == "indexing"


def test_heartbeat_renews_only_the_current_execution_lease(service):
    state, queue, _ingester, _runtime = service
    artifact_id = submit(service)
    task = state.document_jobs.claim(queue.sent[0])
    with state.engine.begin() as conn:
        conn.execute(update(document_jobs).where(
            document_jobs.c.artifact_id == artifact_id).values(lease_until=1.0))
    state.document_jobs.heartbeat(task)
    assert state.document_jobs.get(artifact_id)["lease_until"] > 100
    stale = {**task, "execution_id": uuid4().hex}
    with state.engine.begin() as conn:
        conn.execute(update(document_jobs).where(
            document_jobs.c.artifact_id == artifact_id).values(lease_until=1.0))
    state.document_jobs.heartbeat(stale)
    assert state.document_jobs.get(artifact_id)["lease_until"] == 1.0


def test_worker_starts_and_stops_a_lease_heartbeat_around_processing(service, monkeypatch):
    state, queue, ingester, runtime = service
    monkeypatch.setenv("RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS", "0.01")
    monkeypatch.setenv("RAG_DOCUMENT_JOB_LEASE_SECONDS", "0.05")
    artifact_id = submit(service)
    entered, release = threading.Event(), threading.Event()
    original = ingester.ingest

    def slow(*args, **kwargs):
        entered.set()
        assert release.wait(5)
        return original(*args, **kwargs)

    ingester.ingest = slow
    with ThreadPoolExecutor(max_workers=1) as pool:
        running = pool.submit(DocumentWorker(runtime).process, queue.sent[0])
        try:
            assert entered.wait(3)
            first = state.document_jobs.get(artifact_id)["lease_until"]
            time.sleep(0.2)
            renewed = state.document_jobs.get(artifact_id)["lease_until"]
            assert renewed > first
            expire_lease(state, artifact_id)
            # The heartbeat thread is the only thing that can re-extend the lease.
            deadline = time.monotonic() + 2
            while time.monotonic() < deadline and state.document_jobs.get(
                    artifact_id)["lease_until"] <= time.time():
                time.sleep(0.02)
            assert state.document_jobs.get(artifact_id)["lease_until"] > time.time()
        finally:
            release.set()
        assert running.result(timeout=5) is True
    assert heartbeat_seconds() == 0.01


def test_rejected_sql_publication_cannot_mark_success(service, monkeypatch):
    """A failed publication keeps the job retryable instead of ACKing it."""

    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    original = state.uploads.commit

    def reject(*args, **kwargs):
        raise OSError("commit rejected")

    monkeypatch.setattr(state.uploads, "commit", reject)
    assert DocumentWorker(runtime).process(queue.sent[0]) is False
    status = runtime.get_document_status("manuals", artifact_id)
    assert status["status"] == "queued" and status["stage"] == "retry_wait"
    assert state.documents.list("manuals") == []
    monkeypatch.setattr(state.uploads, "commit", original)
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_concurrent_admission_with_one_key_has_one_identity(service):
    """The MySQL unique idempotency constraint is the identity arbiter."""

    state, queue, _ingester, _runtime = service
    with ThreadPoolExecutor(max_workers=5) as pool:
        ids = list(pool.map(lambda _: submit(service, idempotency_key="same"), range(10)))
    assert len(set(ids)) == 1
    assert {message["artifact_id"] for message in queue.sent} == set(ids)
    assert len(outbox_rows(state)) == 1


def test_failed_broker_send_is_retried_by_the_outbox_relay_not_the_dispatcher(service):
    """No dispatch loop exists; delivery retries are driven by outbox rows."""

    state, queue, ingester, _runtime = service
    queue.error = ConnectionError("broker down")
    artifact_id = submit(service, idempotency_key="retry")
    assert state.document_dispatcher.run_once() == 0
    assert queue.sent == [] and ingester.calls == []
    row = outbox_rows(state)[0]
    assert row["status"] == "PENDING" and row["artifact_id"] == artifact_id
    queue.error = None
    assert state.document_dispatcher.relay.publish_one(row["id"]) is True
    assert [message["artifact_id"] for message in queue.sent] == [artifact_id]
    assert submit(service, idempotency_key="retry") == artifact_id
    assert len(queue.sent) == 1


def test_dead_letter_never_carries_content_into_status(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    assert state.document_jobs.dead_letter(queue.sent[0]) is True
    status = runtime.get_document_status("manuals", artifact_id)
    assert set(status) == {"artifact_id", "status", "stage", "error"}
    assert status["status"] == "failed" and "content" not in status
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    assert ingester.calls == []


def test_dead_letter_for_a_wrong_namespace_cannot_overwrite_the_job(service):
    """An unknown namespace is indistinguishable from a purged job: ACK, no write."""

    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    assert state.document_jobs.dead_letter({**queue.sent[0], "namespace": "other"}) is True
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "queued"


def test_dead_letter_for_unknown_artifact_is_acknowledged(service):
    state, _queue, _ingester, _runtime = service
    payload = {"schema": 3, "artifact_id": str(uuid4()), "namespace": "manuals",
               "name": "gone.txt", "created_at": 0.0}
    assert state.document_jobs.dead_letter(payload) is True


def test_full_delivery_path_never_touches_redis(service):
    """Commit, notify, consume and status all work with an exploding cache client."""

    state, queue, ingester, runtime = service

    class UnreachableCache:
        enabled = True

        def close(self):
            pass

        def __getattr__(self, name):
            raise AssertionError(f"document delivery touched Redis: {name}")

    state.cache_sync.backend = UnreachableCache()
    artifact_id = submit(service, b"no redis anywhere")
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    assert runtime.get_document_status("manuals", artifact_id) == {
        "artifact_id": artifact_id, "status": "succeeded", "stage": "completed", "error": None}
    assert ingester.calls == [b"no redis anywhere"]


def test_worker_recover_runs_maintenance_and_upload_recovery(service):
    state, queue, _ingester, runtime = service
    use_transport(state, None)
    artifact_id = submit(service, b"stranded")
    with state.engine.begin() as conn:
        conn.execute(update(document_jobs).where(
            document_jobs.c.artifact_id == artifact_id).values(updated_at=0.0))
        conn.execute(update(document_job_outbox).values(created_at=0.0, status="PUBLISHED"))
    DocumentWorker(runtime).recover()
    assert [row["artifact_id"] for row in state.document_jobs.pending_outbox()] == [artifact_id]
