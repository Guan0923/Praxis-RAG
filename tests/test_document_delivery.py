"""Admission confirmation, expiry and concurrent document execution boundaries."""

import threading
from concurrent.futures import ThreadPoolExecutor

import pytest
from test_document_tasks import submit

pytest_plugins = ("test_document_tasks",)
from rag_mcp.document_worker import DocumentWorker
from rag_mcp.service_knowledge import KnowledgeError
from rag_mcp.state.uploads import UploadConflict


def test_no_dispatch_loop_can_report_a_failed_send_as_success(service):
    state, queue, ingester, _runtime = service
    queue.error_after = 0
    with pytest.raises(KnowledgeError):
        submit(service, idempotency_key="retry")
    assert state.document_dispatcher.run_once() == 0
    assert queue.sent == [] and ingester.calls == []
    queue.error_after = None
    assert submit(service, idempotency_key="retry")


def test_status_and_reservations_use_configured_result_ttl(service, monkeypatch):
    monkeypatch.setenv("RAG_RESULT_TTL_SECONDS", "7")
    state, queue, ingester, runtime = service
    redis = state.cache_sync.backend._connection()
    now = [1000.0]
    redis.clock = lambda: now[0]
    artifact_id = submit(service)
    assert state.document_tasks.status("manuals", artifact_id)["status"] == "queued"
    now[0] += 8
    with pytest.raises(LookupError):
        state.document_tasks.status("manuals", artifact_id)
    assert DocumentWorker(runtime).process(queue.sent[0])
    assert ingester.calls == [b"original"]


def test_concurrent_admission_same_key_has_one_identity(service):
    _state, queue, _ingester, _runtime = service
    with ThreadPoolExecutor(max_workers=5) as pool:
        ids = list(pool.map(lambda _: submit(service, idempotency_key="same"), range(10)))
    assert len(set(ids)) == 1
    assert {payload["artifact_id"] for payload in queue.sent} == set(ids)


def test_independent_workers_do_not_run_duplicate_parser(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    entered, release = threading.Event(), threading.Event()
    original = ingester.ingest
    def slow(*args, **kwargs):
        entered.set()
        assert release.wait(5)
        return original(*args, **kwargs)
    ingester.ingest = slow
    first, second = DocumentWorker(runtime), DocumentWorker(runtime)
    with ThreadPoolExecutor(max_workers=1) as pool:
        running = pool.submit(first.process, queue.sent[0])
        try:
            assert entered.wait(3)
            assert second.process(queue.sent[0]) is False
            assert state.document_tasks.dead_letter(queue.sent[0]) is False
        finally:
            release.set()
        assert running.result(timeout=5)
    assert len(ingester.calls) == 1
    assert second.process(queue.sent[0])
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_status_expiration_does_not_make_unknown_terminal(service):
    state, _queue, _ingester, _runtime = service
    artifact_id = submit(service)
    state.document_tasks.transient.delete("document", artifact_id)
    assert not state.document_tasks.terminal(artifact_id)


def test_dead_letter_never_carries_chunk_data_into_status(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    assert state.document_tasks.dead_letter(queue.sent[0])
    row = state.document_tasks.transient.get("document", artifact_id)
    assert row["status"] == "failed" and "data" not in row and "content" not in row
    assert DocumentWorker(runtime).process(queue.sent[0])
    assert ingester.calls == []


def test_namespace_metadata_conflict_is_not_terminal_ack(service):
    _state, queue, _ingester, runtime = service
    submit(service)
    assert DocumentWorker(runtime).process(queue.sent[0])
    with pytest.raises(UploadConflict):
        DocumentWorker(runtime).process({**queue.sent[0], "namespace": "other"})


def test_redis_unavailable_after_durable_commit_requires_broker_replay(service, monkeypatch):
    from rag_mcp.state.transient import TransientUnavailable
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    redis = state.cache_sync.backend._connection()
    original = state.uploads.commit
    def commit(*args, **kwargs):
        result = original(*args, **kwargs)
        redis.broken = True
        return result
    monkeypatch.setattr(state.uploads, "commit", commit)
    with pytest.raises(TransientUnavailable):
        DocumentWorker(runtime).process(queue.sent[0])
    redis.broken = False
    # A redelivery reconciles publication even while the crashed execution lock remains.
    assert DocumentWorker(runtime).process(queue.sent[0])
    assert len(ingester.calls) == 1
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_rejected_sql_publication_cannot_mark_success(service, monkeypatch):
    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    original = state.uploads.commit
    def reject(*args, **kwargs):
        raise OSError("commit rejected")
    monkeypatch.setattr(state.uploads, "commit", reject)
    assert DocumentWorker(runtime).process(queue.sent[0]) is False
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "queued"
    assert state.documents.list("manuals") == []
    monkeypatch.setattr(state.uploads, "commit", original)
    assert DocumentWorker(runtime).process(queue.sent[0])
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"



def test_stale_execution_cannot_fail_or_reset_current_execution(service):
    state, queue, _ingester, _runtime = service
    submit(service)
    task = state.document_tasks.assemble(queue.sent[0])
    stale = state.document_tasks.claim(task)
    current = state.document_tasks.claim(task)
    with pytest.raises(UploadConflict):
        state.document_tasks.progress(stale, "indexing")
    with pytest.raises(UploadConflict):
        state.document_tasks.fail(stale, retry=False, error="stale failure")
    state.document_tasks.fail(stale, retry=True, error="stale retry")
    row = state.document_tasks.transient.get("document", task["artifact_id"])
    assert row["execution_id"] == current["execution_id"] and row["status"] == "processing"


def test_wrong_namespace_dead_letter_cannot_overwrite_status(service):
    state, queue, _ingester, _runtime = service
    artifact_id = submit(service)
    with pytest.raises(UploadConflict):
        state.document_tasks.dead_letter({**queue.sent[0], "namespace": "other"})
    assert state.document_tasks.status("manuals", artifact_id)["status"] == "queued"



def test_expired_redis_execution_lock_cannot_steal_live_journal_owner(service):
    state, queue, ingester, runtime = service
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
            execution = state.document_tasks.transient.get("document", artifact_id)["execution_id"]
            transient = state.document_tasks.transient
            redis = state.cache_sync.backend._connection()
            redis.delete(transient._key("lock:document_execution", artifact_id))
            assert DocumentWorker(runtime).process(queue.sent[0]) is False
            assert state.document_tasks.transient.get("document", artifact_id)["execution_id"] == execution
            assert state.document_tasks.dead_letter(queue.sent[0]) is False
        finally:
            release.set()
        assert running.result(timeout=5)
    assert len(ingester.calls) == 1
