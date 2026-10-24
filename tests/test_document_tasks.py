"""Durable admission, broker redeliveries and first-terminal-wins publication."""

import threading
from types import SimpleNamespace
from uuid import UUID, uuid4

import pytest
from sqlalchemy import select, update

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.document_worker import DocumentWorker
from rag_mcp.service_knowledge import (
    IndexedDocument,
    KnowledgeDocumentStore,
    RagServiceRuntime,
)
from rag_mcp.state.cache import CacheUnavailable
from rag_mcp.state.schema import document_task_outbox, document_tasks
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from rag_mcp.state.uploads import UploadConflict, UploadJournal


class Queue:
    def __init__(self):
        self.sent = []
        self.error = False

    def send(self, artifact_id):
        if self.error:
            raise ConnectionError("broker unavailable")
        self.sent.append(artifact_id)

    def close(self):
        pass


class Ingester:
    def __init__(self):
        self.calls = []
        self.fail = None

    def ingest(self, path, *, source_name, chunking=None):
        self.calls.append(path.read_bytes())
        if self.fail:
            raise self.fail
        from rag_mcp.chunking import _stable_id
        return IndexedDocument(_stable_id("doc", source_name.casefold()), 1, "test")


class Runtime(RagServiceRuntime):
    def __init__(self, state, ingester):
        self.state = state
        self._inference_lock = threading.RLock()
        self.tenant = SimpleNamespace(documents=KnowledgeDocumentStore(state, "manuals"),
                                      ingester=ingester, lock=threading.RLock())

    def _tenant(self, namespace):
        return self.tenant

    def _recovery_store(self, operation):
        return None


@pytest.fixture
def service(tmp_path):
    state = StateStores(StateSettings(url=f"sqlite+pysqlite:///{tmp_path / 'tasks.sqlite3'}"))
    settings = {"namespace": "manuals", "collection": "test", "embedding_model": "test",
                "tokenizer": "test", **ChunkingConfig().metadata("test")}
    state.knowledge_bases.create(settings)
    queue = Queue()
    state.document_dispatcher.transport = queue
    ingester = Ingester()
    runtime = Runtime(state, ingester)
    yield state, queue, ingester, runtime
    state.close()


def submit(service, **kwargs):
    _state, _queue, _ingester, runtime = service
    return runtime.add_document("manuals", "manual.txt", b"original", **kwargs)["artifact_id"]


def expire_delivery(state, artifact_id):
    with state.engine.begin() as conn:
        conn.execute(update(document_task_outbox).where(
            document_task_outbox.c.artifact_id == artifact_id).values(retry_at=0, lease_until=0))
    state.document_tasks._dispatch_pause_until = 0


def test_admission_is_durable_and_does_not_index(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    assert queue.sent == []
    assert ingester.calls == []
    assert state.documents.list("manuals") == []
    assert runtime.get_document_status("manuals", artifact_id) == {
        "artifact_id": artifact_id, "status": "queued", "stage": "dispatch_wait", "error": None}
    with state.engine.connect() as conn:
        assert conn.scalar(select(document_tasks.c.content)) == b"original"
        assert conn.scalar(select(document_task_outbox.c.pending)) is True
        assert conn.scalar(select(document_task_outbox.c.attempts)) == 0
    assert state.document_dispatcher.run_once() == 1
    assert queue.sent == [artifact_id]
    assert runtime.get_document_status("manuals", artifact_id)["stage"] == "queued"
    with state.engine.connect() as conn:
        assert conn.scalar(select(document_task_outbox.c.pending)) is False


def test_consumer_publishes_once_and_update_delete_still_sync(service):
    state, _queue, ingester, runtime = service
    artifact_id = submit(service)
    worker = DocumentWorker(runtime)
    assert worker.process(artifact_id)
    assert worker.process(artifact_id)
    assert len(ingester.calls) == 1
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"
    assert state.blobs.get("manuals", "source", "manual.txt") == b"original"
    result = runtime.write_document("manuals", "manual.txt", b"updated", overwrite=True)
    assert result["status"] == "updated"
    assert runtime.delete("manuals", "manual.txt")["deleted"]["name"] == "manual.txt"


def test_broker_failure_preserves_file_and_idempotent_retry(service):
    state, queue, _ingester, runtime = service
    queue.error = True
    artifact_id = submit(service, idempotency_key="retry")
    assert submit(service, idempotency_key="retry") == artifact_id
    assert queue.sent == []
    assert runtime.get_document_status("manuals", artifact_id)["stage"] == "dispatch_wait"
    assert state.document_dispatcher.run_once() == 0
    assert runtime.get_document_status("manuals", artifact_id)["stage"] == "dispatch_retry"
    assert state.document_dispatcher.run_once() == 0
    with state.engine.connect() as conn:
        assert conn.scalar(select(document_tasks.c.artifact_id)) == artifact_id
        assert conn.scalar(select(document_tasks.c.content)) == b"original"
        delivery = conn.execute(select(document_task_outbox)).mappings().one()
        assert delivery["pending"] is True
        assert delivery["attempts"] == 1
        assert delivery["retry_at"] > state.uploads._now(conn)
        assert delivery["owner"] is None and delivery["lease_until"] == 0
        assert delivery["last_error"] == "ConnectionError"
    assert submit(service, idempotency_key="retry") == artifact_id
    assert queue.sent == []
    queue.error = False
    expire_delivery(state, artifact_id)
    assert state.document_dispatcher.run_once() == 1
    assert queue.sent == [artifact_id]
    assert submit(service, idempotency_key="retry") == artifact_id
    assert queue.sent == [artifact_id]
    assert runtime.get_document_status("manuals", artifact_id)["stage"] == "queued"
    with state.engine.connect() as conn:
        delivery = conn.execute(select(document_task_outbox)).mappings().one()
        assert delivery["pending"] is False and delivery["attempts"] == 2
        assert delivery["owner"] is None and delivery["lease_until"] == 0
        assert delivery["last_error"] is None
    with pytest.raises(UploadConflict):
        runtime.add_document("manuals", "manual.txt", b"different", idempotency_key="retry")


def test_relay_resumes_after_process_restart(service):
    state, queue, ingester, _runtime = service
    artifact_id = state.document_tasks.submit("manuals", "manual.txt", b"original", "retry")
    restarted = StateStores(state.settings)
    try:
        restarted.document_dispatcher.transport = queue
        restarted.document_dispatcher.run_once()
        assert queue.sent == [artifact_id]
        assert DocumentWorker(Runtime(restarted, ingester)).process(artifact_id)
        assert restarted.document_tasks.status("manuals", artifact_id)["status"] == "succeeded"
    finally:
        restarted.close()


def test_duplicate_filename_and_cross_namespace_status(service):
    state, _queue, _ingester, runtime = service
    artifact_id = submit(service)
    with pytest.raises(UploadConflict):
        runtime.add_document("manuals", "MANUAL.txt", b"other")
    with pytest.raises(LookupError):
        state.document_tasks.status("other", artifact_id)
    with pytest.raises(LookupError):
        state.document_tasks.status("manuals", str(uuid4()))
    with pytest.raises(ValueError):
        state.document_tasks.status("manuals", "invalid")


def test_duplicate_claims_have_local_execution_ids_without_consumer_ownership(service):
    state, _queue, _ingester, runtime = service
    artifact_id = submit(service)
    first = state.document_tasks.claim(artifact_id)
    second = state.document_tasks.claim(artifact_id)
    assert first is not None and second is not None
    assert first["execution_id"] != second["execution_id"]
    for task in (first, second):
        assert len(task["execution_id"]) == 32
        assert UUID(hex=task["execution_id"]).version == 4
    with state.engine.connect() as conn:
        row = conn.execute(select(document_tasks)).mappings().one()
    assert row["status"] == "processing"
    assert row["owner"] is None and row["lease_until"] == 0 and row["attempts"] == 0
    runtime.process_document_task(first)
    assert state.document_tasks.claim(artifact_id) is None
    with state.engine.begin() as conn:
        with pytest.raises(UploadConflict):
            state.document_tasks.succeed(conn, second)
    assert state.document_tasks.terminal(artifact_id)


def test_recovery_does_not_republish_archived_delivery(service):
    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    assert state.document_dispatcher.run_once() == 1
    assert state.document_tasks.claim(artifact_id)
    with state.engine.begin() as conn:
        conn.execute(update(document_tasks).where(document_tasks.c.artifact_id == artifact_id).values(
            updated_at=0))
    DocumentWorker(runtime).recover()
    assert state.document_dispatcher.run_once() == 0
    assert queue.sent == [artifact_id]
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "processing"
    assert DocumentWorker(runtime).process(artifact_id)


def test_transient_failures_wait_for_broker_without_sql_retry_limit(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    assert state.document_dispatcher.run_once() == 1
    with state.engine.connect() as conn:
        archived_delivery = dict(conn.execute(select(document_task_outbox)).mappings().one())
    worker = DocumentWorker(runtime)
    ingester.fail = ConnectionError("private connection details")
    execution_ids = []
    original = runtime.process_document_task

    def record(task):
        execution_ids.append(task["execution_id"])
        return original(task)

    runtime.process_document_task = record
    for _ in range(5):
        assert not worker.process(artifact_id)
        status = runtime.get_document_status("manuals", artifact_id)
        assert status["status"] == "queued" and status["stage"] == "retry_wait"
        assert "private" not in (status["error"] or "")
        assert state.document_dispatcher.run_once() == 0
    assert len(set(execution_ids)) == 5
    assert queue.sent == [artifact_id]
    with state.engine.connect() as conn:
        row = conn.execute(select(document_tasks)).mappings().one()
        delivery = conn.execute(select(document_task_outbox)).mappings().one()
    assert row["content"] == b"original" and row["attempts"] == 0
    assert row["owner"] is None and row["lease_until"] == 0
    assert dict(delivery) == archived_delivery
    ingester.fail = None
    assert worker.process(artifact_id)
    assert len(set(execution_ids)) == 6
    from rag_mcp.state.schema import upload_operations
    with state.engine.connect() as conn:
        keys = list(conn.scalars(select(upload_operations.c.idempotency_key)))
    assert set(keys) == {f"document-task:{artifact_id}:{execution_id}" for execution_id in execution_ids}


def test_dead_letter_during_indexing_prevents_late_publication(service):
    state, _queue, ingester, runtime = service
    artifact_id = submit(service)
    original = ingester.ingest

    def dead_letter(*args, **kwargs):
        indexed = original(*args, **kwargs)
        assert state.document_tasks.dead_letter(artifact_id)
        return indexed

    ingester.ingest = dead_letter
    assert DocumentWorker(runtime).process(artifact_id)
    assert state.documents.list("manuals") == []
    assert state.blobs.get("manuals", "source", "manual.txt") is None
    status = runtime.get_document_status("manuals", artifact_id)
    assert status["status"] == "failed" and status["stage"] == "dead_letter"
    assert status["error"] == "Message retry limit reached"
    assert DocumentWorker(runtime).process(artifact_id)
    assert len(ingester.calls) == 1


def test_invalid_document_not_retried(service):
    from rag_mcp.service_knowledge import KnowledgeValidationError
    _state, _queue, ingester, runtime = service
    artifact_id = submit(service)
    ingester.fail = KnowledgeValidationError("invalid source")
    assert DocumentWorker(runtime).process(artifact_id)
    assert len(ingester.calls) == 1
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "failed"


def test_config_change_before_execution_is_rejected(service):
    state, _queue, ingester, runtime = service
    artifact_id = submit(service)
    state.knowledge_bases.update(state.knowledge_bases.get("manuals"))
    assert DocumentWorker(runtime).process(artifact_id)
    assert ingester.calls == []
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "failed"


def test_config_change_during_execution_rolls_back_publication(service):
    state, _queue, ingester, runtime = service
    artifact_id = submit(service)
    original = ingester.ingest

    def change_config(*args, **kwargs):
        indexed = original(*args, **kwargs)
        state.knowledge_bases.update(state.knowledge_bases.get("manuals"))
        return indexed

    ingester.ingest = change_config
    assert not DocumentWorker(runtime).process(artifact_id)
    assert state.documents.list("manuals") == []
    assert state.blobs.get("manuals", "source", "manual.txt") is None


def test_consumer_retry_during_send_does_not_reopen_outbox(service):
    state, _queue, _ingester, _runtime = service
    artifact_id = state.document_tasks.submit("manuals", "manual.txt", b"original")

    class RacingQueue:
        def send(self, incoming):
            task = state.document_tasks.claim(incoming)
            state.document_tasks.fail(task, retry=True, error="unused")

    assert state.document_tasks.dispatch(artifact_id, RacingQueue())
    with state.engine.connect() as conn:
        record = conn.execute(select(document_task_outbox)).mappings().one()
    assert not record["pending"] and record["generation"] == 1
    assert record["attempts"] == 1
    assert state.document_tasks.status("manuals", artifact_id)["stage"] == "retry_wait"


def test_redis_outage_falls_back_to_authoritative_sql(service, monkeypatch):
    state, _queue, _ingester, runtime = service
    artifact_id = submit(service)
    backend = state.cache_sync.backend
    backend.enabled = True

    def unavailable(*args, **kwargs):
        raise CacheUnavailable("down")

    monkeypatch.setattr(backend, "get", unavailable)
    monkeypatch.setattr(backend, "put", unavailable)
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "queued"
    assert DocumentWorker(runtime).process(artifact_id)
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_long_processing_needs_no_task_heartbeat_or_exclusive_claim(service, monkeypatch):
    from concurrent.futures import ThreadPoolExecutor

    state, _queue, ingester, runtime = service
    started = threading.Event()
    release = threading.Event()
    original_ingest = ingester.ingest

    def forbidden(*args, **kwargs):
        pytest.fail("Consumer execution must not renew a SQL task lease")

    monkeypatch.setattr(state.document_tasks, "heartbeat", forbidden, raising=False)

    def slow(*args, **kwargs):
        started.set()
        assert release.wait(5)
        return original_ingest(*args, **kwargs)

    ingester.ingest = slow
    artifact_id = submit(service)
    with ThreadPoolExecutor(max_workers=1) as pool:
        processing = pool.submit(DocumentWorker(runtime).process, artifact_id)
        try:
            assert started.wait(2)
            duplicate = state.document_tasks.claim(artifact_id)
            assert duplicate is not None
            assert runtime.get_document_status("manuals", artifact_id)["status"] == "processing"
            next_upload = runtime.add_document("manuals", "next.txt", b"next")
            assert runtime.get_document_status("manuals", next_upload["artifact_id"])["status"] == "queued"
        finally:
            release.set()
        assert processing.result(timeout=5)


def test_status_cache_refreshes_each_stage_and_hits_without_sql(service, monkeypatch):
    from sqlalchemy import event
    from test_state_cache import FakeRedis

    from rag_mcp.state.cache import RedisStateCache, StateCacheSettings

    state, _queue, _ingester, runtime = service
    old_backend = state.cache_sync.backend
    state.cache_sync.backend = RedisStateCache(StateCacheSettings(), client=FakeRedis())
    old_backend.close()
    artifact_id = submit(service)
    assert runtime.get_document_status("manuals", artifact_id)["stage"] == "dispatch_wait"
    queries = []

    def track(*_):
        queries.append(1)

    event.listen(state.engine, "before_cursor_execute", track)
    try:
        assert runtime.get_document_status("manuals", artifact_id)["status"] == "queued"
        assert queries == []
    finally:
        event.remove(state.engine, "before_cursor_execute", track)
    task = state.document_tasks.claim(artifact_id)
    state.document_tasks.progress(task, "embedding")
    assert runtime.get_document_status("manuals", artifact_id)["stage"] == "embedding"
    runtime.process_document_task(task)
    state.document_tasks.flush(artifact_id)
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"
    with pytest.raises(LookupError):
        state.document_tasks.status("other", artifact_id)


def test_publication_transaction_rolls_back_task_status_too(service, monkeypatch):
    state, _queue, _ingester, runtime = service
    artifact_id = submit(service)
    original = state.uploads.commit

    def abort(*args):
        original(*args)
        raise OSError("transaction aborted before commit")

    monkeypatch.setattr(state.uploads, "commit", abort)
    assert not DocumentWorker(runtime).process(artifact_id)
    assert state.documents.list("manuals") == []
    assert state.blobs.get("manuals", "source", "manual.txt") is None
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "queued"


def test_abandoned_upload_is_compensated_without_sql_republish(service):
    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    assert state.document_dispatcher.run_once() == 1
    old = state.document_tasks.claim(artifact_id)
    journal = UploadJournal(state, auto_heartbeat=False)
    from rag_mcp.chunking import _stable_id
    from rag_mcp.state.schema import document_leases

    try:
        operation = journal.begin("manuals", "manual.txt", _stable_id("doc", "manual.txt"), "test",
                                  on_begin=lambda conn, op: state.document_tasks.bind(conn, old, op))
        fresh = state.document_tasks.claim(artifact_id)
        assert fresh is not None and fresh["execution_id"] != old["execution_id"]
        with pytest.raises(UploadConflict):
            runtime.process_document_task(fresh)
        with state.engine.begin() as conn:
            conn.execute(update(document_leases).where(document_leases.c.operation_id == operation["id"])
                         .values(expires_at=0))
        DocumentWorker(runtime).recover()
        assert state.uploads.get(operation["id"])["status"] == "ROLLED_BACK"
        assert state.document_dispatcher.run_once() == 0
        assert queue.sent == [artifact_id]
        assert DocumentWorker(runtime).process(artifact_id)
    finally:
        journal.close()


@pytest.mark.parametrize("terminal", ["succeeded", "failed", "dead_letter"])
def test_first_terminal_wins_against_late_delivery_outcomes(service, terminal):
    state, _queue, _ingester, runtime = service
    artifact_id = submit(service)
    late = state.document_tasks.claim(artifact_id)
    winner = state.document_tasks.claim(artifact_id)
    if terminal == "succeeded":
        runtime.process_document_task(winner)
    elif terminal == "failed":
        state.document_tasks.fail(winner, retry=False, error="Invalid source")
    else:
        assert state.document_tasks.dead_letter(artifact_id)
    expected = state.document_tasks.status("manuals", artifact_id)
    assert state.document_tasks.dead_letter(artifact_id)
    with pytest.raises(UploadConflict):
        state.document_tasks.fail(late, retry=True, error="late retry")
    with pytest.raises(UploadConflict):
        state.document_tasks.fail(late, retry=False, error="late failure")
    with state.engine.begin() as conn:
        with pytest.raises(UploadConflict):
            state.document_tasks.succeed(conn, late)
    assert state.document_tasks.status("manuals", artifact_id) == expected
    assert state.document_tasks.claim(artifact_id) is None


def test_succeed_checks_configuration_inside_publication_transaction(service):
    state, _queue, _ingester, _runtime = service
    artifact_id = submit(service)
    task = state.document_tasks.claim(artifact_id)
    state.knowledge_bases.update(state.knowledge_bases.get("manuals"))
    with state.engine.begin() as conn:
        with pytest.raises(UploadConflict):
            state.document_tasks.succeed(conn, task)
    assert state.document_tasks.status("manuals", artifact_id)["status"] == "processing"
