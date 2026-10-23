"""Durable MCP admission, cross-process task ownership and outbox recovery."""

import threading
from types import SimpleNamespace
from uuid import uuid4

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
from rag_mcp.state.document_tasks import DocumentTaskSettings
from rag_mcp.state.schema import document_task_outbox, document_tasks
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from rag_mcp.state.uploads import LeaseLost, UploadConflict, UploadJournal


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
    state.document_tasks.settings = DocumentTaskSettings(retry_seconds=0.01)
    queue = Queue()
    state.document_dispatcher.transport = queue
    ingester = Ingester()
    runtime = Runtime(state, ingester)
    yield state, queue, ingester, runtime
    state.close()


def submit(service, **kwargs):
    _state, _queue, _ingester, runtime = service
    return runtime.add_document("manuals", "manual.txt", b"original", **kwargs)["artifact_id"]


def expire(state, artifact_id):
    with state.engine.begin() as conn:
        conn.execute(update(document_tasks).where(document_tasks.c.artifact_id == artifact_id).values(
            lease_until=0, updated_at=0))
        conn.execute(update(document_task_outbox).where(
            document_task_outbox.c.artifact_id == artifact_id).values(retry_at=0))


def test_admission_is_durable_and_does_not_index(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    assert queue.sent == [artifact_id]
    assert ingester.calls == []
    assert state.documents.list("manuals") == []
    assert runtime.get_document_status("manuals", artifact_id) == {
        "artifact_id": artifact_id, "status": "queued", "stage": "queued", "error": None}
    with state.engine.connect() as conn:
        assert conn.scalar(select(document_tasks.c.content)) == b"original"
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
    from rag_mcp.service_knowledge import KnowledgeError
    with pytest.raises(KnowledgeError, match="not confirmed"):
        submit(service, idempotency_key="retry")
    with state.engine.connect() as conn:
        artifact_id = conn.scalar(select(document_tasks.c.artifact_id))
        assert conn.scalar(select(document_task_outbox.c.pending))
    queue.error = False
    assert submit(service, idempotency_key="retry") == artifact_id
    assert queue.sent == [artifact_id]
    assert submit(service, idempotency_key="retry") == artifact_id
    assert queue.sent == [artifact_id]
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


def test_only_one_live_owner_and_old_owner_is_fenced(service):
    state, _queue, _ingester, runtime = service
    artifact_id = submit(service)
    old = state.document_tasks.claim(artifact_id)
    assert old is not None
    assert state.document_tasks.claim(artifact_id) is None
    assert not DocumentWorker(runtime).process(artifact_id)
    expire(state, artifact_id)
    fresh = state.document_tasks.claim(artifact_id)
    assert fresh["owner"] != old["owner"]
    with pytest.raises(LeaseLost):
        state.document_tasks.heartbeat(old)
    with pytest.raises(LeaseLost):
        runtime.process_document_task(old)
    assert state.documents.list("manuals") == []
    runtime.process_document_task(fresh)
    assert state.document_tasks.terminal(artifact_id)


def test_expired_work_is_redriven_even_if_message_is_lost(service):
    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    state.document_tasks.claim(artifact_id)
    expire(state, artifact_id)
    state.document_dispatcher.run_once()
    assert queue.sent == [artifact_id, artifact_id]
    assert DocumentWorker(runtime).process(artifact_id)


def test_transient_retry_is_bounded_and_original_bytes_retained(service):
    state, _queue, ingester, runtime = service
    artifact_id = submit(service)
    worker = DocumentWorker(runtime)
    ingester.fail = ConnectionError("private connection details")
    for attempt in range(3):
        complete = worker.process(artifact_id)
        if attempt < 2:
            assert not complete
            status = runtime.get_document_status("manuals", artifact_id)
            assert status["status"] == "queued" and status["stage"] == "retry_wait"
            expire(state, artifact_id)
    assert complete
    status = runtime.get_document_status("manuals", artifact_id)
    assert status["status"] == "failed"
    assert "private" not in status["error"]
    assert state.documents.list("manuals") == []
    with state.engine.connect() as conn:
        assert conn.scalar(select(document_tasks.c.content)) == b"original"


def test_worker_losing_task_lease_cannot_publish_late(service):
    state, _queue, ingester, runtime = service
    artifact_id = submit(service)
    original = ingester.ingest

    def lose_lease(*args, **kwargs):
        indexed = original(*args, **kwargs)
        expire(state, artifact_id)
        return indexed

    ingester.ingest = lose_lease
    assert not DocumentWorker(runtime).process(artifact_id)
    assert state.documents.list("manuals") == []
    assert state.blobs.get("manuals", "source", "manual.txt") is None
    ingester.ingest = original
    assert DocumentWorker(runtime).process(artifact_id)
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


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


def test_outbox_send_cannot_clear_newer_retry(service):
    state, _queue, _ingester, _runtime = service
    artifact_id = state.document_tasks.submit("manuals", "manual.txt", b"original")

    class RacingQueue:
        def send(self, incoming):
            task = state.document_tasks.claim(incoming)
            state.document_tasks.fail(task, retry=True, error="unused")

    state.document_tasks.dispatch(artifact_id, RacingQueue())
    with state.engine.connect() as conn:
        assert conn.scalar(select(document_task_outbox.c.pending))


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


def test_long_processing_renews_task_lease_and_does_not_block_admission(service):
    from concurrent.futures import ThreadPoolExecutor
    from dataclasses import replace

    state, _queue, ingester, runtime = service
    state.document_tasks.settings = replace(state.document_tasks.settings, lease_seconds=0.3)
    started = threading.Event()
    release = threading.Event()
    renewed = threading.Event()
    original_ingest = ingester.ingest
    original_heartbeat = state.document_tasks.heartbeat

    def heartbeat(task):
        original_heartbeat(task)
        renewed.set()

    def slow(*args, **kwargs):
        started.set()
        assert release.wait(5)
        return original_ingest(*args, **kwargs)

    state.document_tasks.heartbeat = heartbeat
    ingester.ingest = slow
    artifact_id = submit(service)
    with ThreadPoolExecutor(max_workers=1) as pool:
        processing = pool.submit(DocumentWorker(runtime).process, artifact_id)
        try:
            assert started.wait(2)
            assert renewed.wait(2)
            assert state.document_tasks.claim(artifact_id) is None
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


def test_abandoned_execution_compensated_before_new_attempt(service):
    state, _queue, _ingester, runtime = service
    artifact_id = submit(service)
    old = state.document_tasks.claim(artifact_id)
    journal = UploadJournal(state, auto_heartbeat=False)
    from rag_mcp.chunking import _stable_id
    operation = journal.begin("manuals", "manual.txt", _stable_id("doc", "manual.txt"), "test",
                              on_begin=lambda conn, op: state.document_tasks.bind(conn, old, op))
    expire(state, artifact_id)
    assert state.document_tasks.claim(artifact_id) is None
    with state.engine.begin() as conn:
        from rag_mcp.state.schema import document_leases
        conn.execute(update(document_leases).where(document_leases.c.operation_id == operation["id"])
                     .values(expires_at=0))
    DocumentWorker(runtime).recover()
    assert state.uploads.get(operation["id"])["status"] == "ROLLED_BACK"
    assert DocumentWorker(runtime).process(artifact_id)
    journal.close()
