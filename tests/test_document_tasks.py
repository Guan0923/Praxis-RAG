"""Redis upload lifecycle, full-file MQ reconstruction and journal-backed replay."""

import threading
from types import SimpleNamespace

import pytest
from redis_stub import MemoryRedis
from sqlalchemy import event

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.document_worker import DocumentWorker
from rag_mcp.service_knowledge import (
    IndexedDocument,
    KnowledgeDocumentStore,
    KnowledgeError,
    KnowledgeValidationError,
    RagServiceRuntime,
)
from rag_mcp.state.cache import RedisStateCache, StateCacheSettings
from rag_mcp.state.document_tasks import (
    CHUNK_BYTES,
    document_envelopes,
    validate_document_envelope,
)
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from rag_mcp.state.transient import TransientUnavailable
from rag_mcp.state.uploads import UploadConflict


class Queue:
    def __init__(self):
        self.sent = []
        self.error_after = None

    def send(self, task):
        for envelope in document_envelopes(task):
            if self.error_after is not None and len(self.sent) >= self.error_after:
                raise ConnectionError("private broker secret")
            self.sent.append(envelope)

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
def service(tmp_path, monkeypatch):
    import tempfile
    monkeypatch.setattr(tempfile, "tempdir", str(tmp_path))
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    state = StateStores(StateSettings(url=f"sqlite+pysqlite:///{tmp_path / 'tasks.sqlite3'}"))
    redis = MemoryRedis()
    state.cache_sync.backend = RedisStateCache(StateCacheSettings(), client=redis)
    settings = {"namespace": "manuals", "collection": "test", "embedding_model": "test",
                "tokenizer": "test", **ChunkingConfig().metadata("test")}
    state.knowledge_bases.create(settings)
    queue = Queue()
    state.document_dispatcher.transport = queue
    ingester = Ingester()
    runtime = Runtime(state, ingester)
    yield state, queue, ingester, runtime
    state.close()


def submit(service, content=b"original", **kwargs):
    return service[3].add_document("manuals", "manual.txt", content, **kwargs)["artifact_id"]


def test_admission_sends_all_payloads_and_never_reads_or_writes_sql_tasks(service):
    state, queue, ingester, runtime = service
    statements = []
    event.listen(state.engine, "before_cursor_execute", lambda c, cur, sql, *rest: statements.append(sql))
    artifact_id = submit(service)
    assert len(queue.sent) == 1 and ingester.calls == []
    assert runtime.get_document_status("manuals", artifact_id) == {
        "artifact_id": artifact_id, "status": "queued", "stage": "queued", "error": None}
    assert not any("document_tasks" in sql or "document_task_outbox" in sql for sql in statements)
    assert "content" not in state.document_tasks.transient.get("document", artifact_id)


def test_worker_publishes_once_and_update_delete_remain_sync(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    worker = DocumentWorker(runtime)
    assert worker.process(queue.sent[0])
    assert worker.process(queue.sent[0])
    assert len(ingester.calls) == 1
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"
    assert state.blobs.get("manuals", "source", "manual.txt") == b"original"
    assert runtime.write_document("manuals", "manual.txt", b"updated", overwrite=True)["status"] == "updated"
    assert runtime.delete("manuals", "manual.txt")["deleted"]["name"] == "manual.txt"


def test_partial_send_failure_surfaces_and_same_key_reuses_artifact(service):
    _state, queue, ingester, runtime = service
    content = b"x" * (CHUNK_BYTES + 1)
    queue.error_after = 1
    with pytest.raises(KnowledgeError, match="same idempotency_key"):
        submit(service, content, idempotency_key="retry")
    artifact_id = queue.sent[0]["artifact_id"]
    assert runtime.get_document_status("manuals", artifact_id)["stage"] == "sending"
    assert not DocumentWorker(runtime).process(queue.sent[0])
    queue.error_after = None
    assert submit(service, content, idempotency_key="retry") == artifact_id
    assert len(queue.sent) == 3
    assert DocumentWorker(runtime).process(queue.sent[-1])
    assert ingester.calls == [content]
    assert submit(service, content, idempotency_key="retry") == artifact_id
    assert len(queue.sent) == 3
    with pytest.raises(UploadConflict):
        submit(service, b"different", idempotency_key="retry")


def test_out_of_order_duplicates_and_maximum_upload(service):
    _state, queue, ingester, runtime = service
    content = b"y" * (10 * 1024 * 1024)
    submit(service, content)
    worker = DocumentWorker(runtime)
    assert len(queue.sent) == 20
    assert not worker.process(queue.sent[-1])
    assert not worker.process(queue.sent[-1])
    for envelope in reversed(queue.sent[1:-1]):
        assert not worker.process(envelope)
    assert worker.process(queue.sent[0])
    assert ingester.calls == [content]
    assert worker.process(queue.sent[-1])


def test_expired_chunks_are_not_silently_acknowledged(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service, b"x" * (CHUNK_BYTES + 1))
    worker = DocumentWorker(runtime)
    assert not worker.process(queue.sent[0])
    state.document_tasks.transient.delete("document_chunks", artifact_id)
    assert not worker.process(queue.sent[1])
    assert ingester.calls == []
    assert worker.process(queue.sent[0])


def test_redis_loss_after_admission_can_reconstruct_from_broker(service):
    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    state.document_tasks.transient.delete("document", artifact_id)
    assert DocumentWorker(runtime).process(queue.sent[0])
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_redis_failure_cannot_accept_upload_or_ack_processing(service):
    state, queue, _ingester, _runtime = service
    state.cache_sync.backend._connection().broken = True
    with pytest.raises(KnowledgeError):
        submit(service)
    assert queue.sent == []


def test_missing_queue_fails_without_accepting(service):
    state, queue, _ingester, _runtime = service
    state.document_dispatcher.transport = None
    with pytest.raises(KnowledgeError):
        submit(service)
    assert queue.sent == []


def test_name_conflict_and_cross_namespace_status(service):
    state, _queue, _ingester, runtime = service
    artifact_id = submit(service)
    with pytest.raises(UploadConflict):
        runtime.add_document("manuals", "MANUAL.txt", b"different")
    with pytest.raises(LookupError):
        state.document_tasks.status("other", artifact_id)


@pytest.mark.parametrize("name", ["../a.txt", "a.exe", "CON.txt", "a/b.txt", ""])
def test_filename_validation(service, name):
    with pytest.raises(KnowledgeValidationError):
        service[3].add_document("manuals", name, b"x")


def test_config_change_is_terminal_failure(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    state.knowledge_bases.update({**state.knowledge_bases.get("manuals"), "chunk_size": 256})
    assert DocumentWorker(runtime).process(queue.sent[0])
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "failed"
    assert ingester.calls == []


def test_metadata_and_checksum_validation(service):
    _state, queue, ingester, runtime = service
    submit(service, b"x" * (CHUNK_BYTES + 1))
    worker = DocumentWorker(runtime)
    assert not worker.process(queue.sent[0])
    with pytest.raises(UploadConflict, match="metadata"):
        worker.process({**queue.sent[1], "name": "other.txt"})
    with pytest.raises(ValueError, match="chunk size"):
        validate_document_envelope({**queue.sent[0], "data": "eA=="})
    with pytest.raises(ValueError, match="checksum"):
        worker.process({**queue.sent[1], "data": "eQ=="})
    assert ingester.calls == []


def test_retry_then_success(service):
    _state, queue, ingester, runtime = service
    artifact_id = submit(service)
    ingester.fail = OSError("private parser data")
    worker = DocumentWorker(runtime)
    assert not worker.process(queue.sent[0])
    assert runtime.get_document_status("manuals", artifact_id)["stage"] == "retry_wait"
    ingester.fail = None
    assert worker.process(queue.sent[0])
    assert len(ingester.calls) == 2


def test_committed_journal_reconciles_after_status_expiry(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    worker = DocumentWorker(runtime)
    assert worker.process(queue.sent[0])
    state.document_tasks.transient.delete("document", artifact_id)
    state.document_tasks.transient.delete("document_chunks", artifact_id)
    assert worker.process(queue.sent[0])
    assert len(ingester.calls) == 1
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_status_write_failure_after_sql_commit_is_replayable(service, monkeypatch):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    original = state.document_tasks.succeed
    def unavailable(*args):
        raise TransientUnavailable("status unavailable")
    monkeypatch.setattr(state.document_tasks, "succeed", unavailable)
    assert DocumentWorker(runtime).process(queue.sent[0])
    monkeypatch.setattr(state.document_tasks, "succeed", original)
    assert DocumentWorker(runtime).process(queue.sent[0])
    assert len(ingester.calls) == 1
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_durable_publication_precedes_success_status(service, monkeypatch):
    state, queue, _ingester, runtime = service
    submit(service)
    original = state.document_tasks.succeed
    def succeed(conn, task):
        assert conn is None
        assert state.uploads.get(task["operation_id"])["status"] == "COMMITTED"
        return original(conn, task)
    monkeypatch.setattr(state.document_tasks, "succeed", succeed)
    assert DocumentWorker(runtime).process(queue.sent[0])


def test_dead_letter_finalizes_without_overwriting_publication(service):
    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    assert DocumentWorker(runtime).process(queue.sent[0])
    state.document_tasks.transient.delete("document", artifact_id)
    assert state.document_tasks.dead_letter(queue.sent[0])
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_active_execution_lock_defers_duplicate(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    token = state.document_tasks.transient.lock("document_execution", artifact_id, 300)
    try:
        assert not DocumentWorker(runtime).process(queue.sent[0])
        assert ingester.calls == []
    finally:
        state.document_tasks.transient.unlock("document_execution", artifact_id, token)
    assert DocumentWorker(runtime).process(queue.sent[0])


@pytest.mark.parametrize("content", [b"", "not bytes", b"x" * (10 * 1024 * 1024 + 1)],
                         ids=["empty", "not-bytes", "over-limit"])
def test_admission_preserves_nonempty_ten_mib_limit(service, content):
    with pytest.raises(KnowledgeValidationError, match="10 MiB"):
        submit(service, content)
    assert service[1].sent == []


def test_status_requires_redis_even_after_publication(service):
    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    assert DocumentWorker(runtime).process(queue.sent[0])
    state.cache_sync.backend._connection().broken = True
    with pytest.raises(KnowledgeError, match="Document status is temporarily unavailable"):
        runtime.get_document_status("manuals", artifact_id)


def test_redis_disabled_fails_admission(service):
    state, queue, _ingester, _runtime = service
    state.cache_sync.backend.enabled = False
    with pytest.raises(KnowledgeError):
        submit(service)
    assert queue.sent == []


def test_terminal_replay_checks_full_request_configuration(service):
    _state, queue, ingester, runtime = service
    submit(service)
    assert DocumentWorker(runtime).process(queue.sent[0])
    with pytest.raises(UploadConflict):
        DocumentWorker(runtime).process({**queue.sent[0], "configuration_version": 999})
    assert len(ingester.calls) == 1



def test_status_missing_or_expired_is_a_safe_validation_error(service):
    state, _queue, _ingester, runtime = service
    artifact_id = submit(service)
    state.document_tasks.transient.delete("document", artifact_id)
    with pytest.raises(KnowledgeValidationError, match="Document upload not found"):
        runtime.get_document_status("manuals", artifact_id)


def test_status_redis_failure_does_not_expose_backend_details(service, monkeypatch):
    state, _queue, _ingester, runtime = service
    artifact_id = submit(service)
    def unavailable(*_args):
        raise TransientUnavailable("private credential data")
    monkeypatch.setattr(state.document_tasks, "status", unavailable)
    with pytest.raises(KnowledgeError) as caught:
        runtime.get_document_status("manuals", artifact_id)
    assert "private" not in str(caught.value)
