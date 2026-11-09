"""MySQL-as-truth asynchronous upload contract: durable job, outbox, notification.

The architecture under test: ``add_document`` commits the complete job (bytes
included) plus one outbox row in a single MySQL transaction; the queue carries
an identifier-only notification; the worker reads the file back from MySQL.
"""

import json
import threading
from types import SimpleNamespace
from uuid import uuid4

import pytest
from redis_stub import MemoryRedis
from sqlalchemy import event, select

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.document_worker import DocumentWorker
from rag_mcp.service_knowledge import (
    IndexedDocument,
    KnowledgeDocumentStore,
    KnowledgeValidationError,
    RagServiceRuntime,
)
from rag_mcp.state.cache import RedisStateCache, StateCacheSettings
from rag_mcp.state.document_jobs import MAX_FILE_BYTES
from rag_mcp.state.schema import document_job_outbox
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from rag_mcp.state.uploads import UploadConflict


class Queue:
    """Broker stand-in: records the published notification and can fail on demand."""

    def __init__(self):
        self.sent = []
        self.error = None

    def send(self, payload):
        if self.error is not None:
            raise self.error
        self.sent.append(json.loads(json.dumps(payload)))

    def close(self):
        pass


class Ingester:
    def __init__(self):
        self.calls = []
        self.fail = None

    def ingest(self, path, *, source_name, chunking=None):
        self.calls.append(path.read_bytes())
        if self.fail is not None:
            raise self.fail
        from rag_mcp.chunking import _stable_id

        return IndexedDocument(_stable_id("doc", source_name.casefold()), 1, "test")

    def delete(self, document_id):
        pass


class Runtime(RagServiceRuntime):
    """Real storage and journal; no model or Qdrant needed by the job contract."""

    def __init__(self, state, ingester):
        self.state = state
        self._inference_lock = threading.RLock()
        self.tenant = SimpleNamespace(documents=KnowledgeDocumentStore(state, "manuals"),
                                      ingester=ingester, lock=threading.RLock())

    def _tenant(self, namespace):
        return self.tenant

    def _recovery_store(self, operation):
        return None


def use_transport(state, transport):
    """Point the dispatcher (and therefore its lazily resolved relay) at a broker."""

    state.document_dispatcher.transport = transport


def build_service(tmp_path, monkeypatch):
    import tempfile

    monkeypatch.setattr(tempfile, "tempdir", str(tmp_path))
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    monkeypatch.delenv("RAG_OUTBOX_BRIDGE", raising=False)
    state = StateStores(StateSettings.from_env())
    state.cache_sync.backend = RedisStateCache(StateCacheSettings(), client=MemoryRedis())
    state.knowledge_bases.create({
        "namespace": "manuals", "collection": "test", "embedding_model": "test",
        "tokenizer": "test", **ChunkingConfig().metadata("test"),
    })
    queue = Queue()
    use_transport(state, queue)
    ingester = Ingester()
    runtime = Runtime(state, ingester)
    return state, queue, ingester, runtime


@pytest.fixture
def service(tmp_path, monkeypatch):
    state, queue, ingester, runtime = build_service(tmp_path, monkeypatch)
    try:
        yield state, queue, ingester, runtime
    finally:
        state.close()


def submit(service, content=b"original", name="manual.txt", **kwargs):
    return service[3].add_document("manuals", name, content, **kwargs)["artifact_id"]


def notification(service, artifact_id):
    return {"schema": 3, "artifact_id": artifact_id, "namespace": "manuals",
            "name": "manual.txt", "created_at": 0.0}


def outbox_rows(state):
    with state.engine.connect() as conn:
        return [dict(row) for row in conn.execute(select(document_job_outbox)).mappings()]


def test_submission_commits_job_and_pending_outbox_in_one_mysql_transaction(service):
    """An accepted upload is a complete MySQL job with its own outbox row."""

    state, queue, ingester, runtime = service
    use_transport(state, None)
    statements = []
    event.listen(state.engine, "before_cursor_execute",
                 lambda conn, cursor, sql, *rest: statements.append(sql))
    artifact_id = submit(service, b"original")
    assert any("document_jobs" in sql for sql in statements)
    assert any("document_job_outbox" in sql for sql in statements)
    # The removed per-chunk dispatcher table must never be read or written again.
    assert not any("document_task_outbox" in sql for sql in statements)
    job = state.document_jobs.get(artifact_id)
    assert (job["status"], job["stage"]) == ("queued", "queued")
    assert job["content"] == b"original" and job["size"] == len(b"original")
    rows = outbox_rows(state)
    assert len(rows) == 1
    assert rows[0]["artifact_id"] == artifact_id and rows[0]["status"] == "PENDING"
    assert queue.sent == [] and ingester.calls == []


def test_submission_path_has_no_redis_or_broker_dependency(service):
    """A committed upload survives a dead Redis and a missing transport."""

    state, queue, ingester, runtime = service

    class UnreachableCache:
        def close(self):
            pass

        def __getattr__(self, name):
            raise AssertionError(f"submission touched Redis: {name}")

    use_transport(state, None)
    state.cache_sync.backend = UnreachableCache()
    artifact_id = submit(service, b"offline")
    assert state.document_jobs.get(artifact_id)["content"] == b"offline"
    assert outbox_rows(state)[0]["status"] == "PENDING"
    assert ingester.calls == []


def test_notification_carries_identifiers_only(service):
    """The published message is a schema-3 notification with no file content."""

    _state, queue, _ingester, _runtime = service
    content = b"SECRET-BYTES-" + bytes(range(256)) * 64
    artifact_id = submit(service, content)
    assert len(queue.sent) == 1
    message = queue.sent[0]
    assert set(message) == {"schema", "artifact_id", "namespace", "name", "created_at"}
    assert (message["schema"], message["artifact_id"], message["namespace"], message["name"]) == (
        3, artifact_id, "manuals", "manual.txt")
    body = json.dumps(message).encode()
    assert len(body) < 1024
    assert b"SECRET-BYTES" not in body and b"content" not in body


def test_worker_reads_the_file_back_from_mysql_for_a_bare_notification(service):
    """A freshly constructed identifier-only notification is still processable."""

    state, queue, ingester, runtime = service
    artifact_id = submit(service, b"from database")
    bare = notification(service, artifact_id)
    assert bare != queue.sent[0]
    assert DocumentWorker(runtime).process(bare) is True
    assert ingester.calls == [b"from database"]
    assert state.blobs.get("manuals", "source", "manual.txt") == b"from database"
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_debezium_change_event_is_processed_from_mysql(service):
    """A raw Debezium outbox change event is accepted like a native notification."""

    state, _queue, ingester, runtime = service
    artifact_id = submit(service, b"cdc")
    envelope = {"before": None, "op": "c", "after": {
        "artifact_id": artifact_id, "namespace": "manuals", "name": "manual.txt",
        "created_at": 0.0}}
    assert DocumentWorker(runtime).process(envelope) is True
    assert ingester.calls == [b"cdc"]
    assert state.blobs.get("manuals", "source", "manual.txt") == b"cdc"


def test_redelivered_notification_indexes_once(service):
    """At-least-once delivery stays idempotent after the job is terminal."""

    state, queue, ingester, runtime = service
    artifact_id = submit(service, b"once")
    worker = DocumentWorker(runtime)
    assert worker.process(queue.sent[0]) is True
    assert worker.process(queue.sent[0]) is True
    assert worker.process({**queue.sent[0], "created_at": 123.0}) is True
    assert ingester.calls == [b"once"]
    assert [item["name"] for item in runtime.list_documents("manuals")] == ["manual.txt"]
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_notification_cannot_override_the_committed_document(service):
    """Message metadata is advisory: MySQL owns the filename and bytes."""

    state, queue, ingester, runtime = service
    artifact_id = submit(service, b"committed")
    forged = {**queue.sent[0], "name": "other.txt", "created_at": 42.0}
    assert DocumentWorker(runtime).process(forged) is True
    assert ingester.calls == [b"committed"]
    assert state.blobs.get("manuals", "source", "manual.txt") == b"committed"
    assert state.blobs.get("manuals", "source", "other.txt") is None
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_redelivery_reconciles_a_committed_journal_without_reingest(service, monkeypatch):
    """Crash between journal commit and terminal status is recovered on redelivery."""

    state, queue, ingester, runtime = service
    artifact_id = submit(service, b"crash")
    original = state.document_jobs._finish
    monkeypatch.setattr(state.document_jobs, "_finish", lambda *args, **kwargs: None)
    assert DocumentWorker(runtime).process(queue.sent[0]) is False
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "processing"
    assert len(ingester.calls) == 1
    monkeypatch.setattr(state.document_jobs, "_finish", original)
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"
    assert len(ingester.calls) == 1
    assert state.uploads.unfinished("manuals") == []


def test_terminal_write_failure_after_committed_publication_is_reconciled(service, monkeypatch):
    """An ambiguous terminal write cannot be acknowledged with a non-terminal row."""

    state, queue, ingester, runtime = service
    artifact_id = submit(service)

    def unavailable(*_args):
        raise RuntimeError("status write unavailable")

    monkeypatch.setattr(state.document_jobs, "succeed", unavailable)
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"
    assert len(ingester.calls) == 1


def test_durable_journal_publication_precedes_success_status(service, monkeypatch):
    """Success is only written after the upload journal committed its publication."""

    state, queue, _ingester, runtime = service
    submit(service)
    original = state.document_jobs.succeed

    def succeed(conn, task):
        assert conn is None
        assert state.uploads.get(task["operation_id"])["status"] == "COMMITTED"
        return original(conn, task)

    monkeypatch.setattr(state.document_jobs, "succeed", succeed)
    assert DocumentWorker(runtime).process(queue.sent[0]) is True


def test_deterministic_validation_failure_is_failed_and_acknowledged(service):
    """A changed knowledge-base configuration is terminal and safe to ACK."""

    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    state.knowledge_bases.update({**state.knowledge_bases.get("manuals"), "chunk_size": 256})
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    status = runtime.get_document_status("manuals", artifact_id)
    assert status == {"artifact_id": artifact_id, "status": "failed", "stage": "failed",
                      "error": "Invalid document or changed knowledge-base configuration"}
    assert ingester.calls == []


def test_deterministic_parse_failure_is_failed_and_acknowledged(service):
    """A rejected document is a terminal, ACKed failure rather than a retry loop."""

    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    ingester.fail = KnowledgeValidationError("unsupported document")
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    status = runtime.get_document_status("manuals", artifact_id)
    assert status["status"] == "failed" and status["stage"] == "failed"
    assert status["error"] == "Invalid document or changed knowledge-base configuration"
    assert runtime.list_documents("manuals") == []


def test_transient_failure_requeues_without_acknowledging(service):
    """An ambiguous parser/backend failure stays retryable and is not ACKed."""

    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    ingester.fail = OSError("private parser detail")
    worker = DocumentWorker(runtime)
    assert worker.process(queue.sent[0]) is False
    status = runtime.get_document_status("manuals", artifact_id)
    assert status["status"] == "queued" and status["stage"] == "retry_wait"
    ingester.fail = None
    assert worker.process(queue.sent[0]) is True
    assert len(ingester.calls) == 2
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_unknown_artifact_is_acknowledged(service):
    """Retrying cannot make an unknown or purged job processable, so ACK it."""

    _state, _queue, ingester, runtime = service
    unknown = {"schema": 3, "artifact_id": str(uuid4()), "namespace": "manuals",
               "name": "missing.txt", "created_at": 0.0}
    assert DocumentWorker(runtime).process(unknown) is True
    assert ingester.calls == []


def test_status_reads_mysql_only_and_has_no_redis_branch(service):
    """Status keeps working with Redis disabled or unreachable."""

    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    state.cache_sync.backend = RedisStateCache(StateCacheSettings(enabled=False))
    assert runtime.get_document_status("manuals", artifact_id) == {
        "artifact_id": artifact_id, "status": "queued", "stage": "queued", "error": None}
    broken = MemoryRedis()
    broken.broken = True
    state.cache_sync.backend = RedisStateCache(StateCacheSettings(), client=broken)
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "queued"
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    assert runtime.get_document_status("manuals", artifact_id) == {
        "artifact_id": artifact_id, "status": "succeeded", "stage": "completed", "error": None}


def test_status_is_namespace_scoped_and_validates_the_artifact_id(service):
    """Cross-namespace and malformed lookups are safe validation errors."""

    state, _queue, _ingester, runtime = service
    artifact_id = submit(service)
    with pytest.raises(LookupError, match="Document upload not found"):
        state.document_jobs.status("other", artifact_id)
    with pytest.raises(KnowledgeValidationError, match="Document upload not found"):
        runtime.get_document_status("other", artifact_id)
    with pytest.raises(KnowledgeValidationError, match="canonical UUID"):
        runtime.get_document_status("manuals", "not-a-uuid")
    with pytest.raises(KnowledgeValidationError, match="Document upload not found"):
        runtime.get_document_status("manuals", str(uuid4()))


def test_unknown_namespace_is_rejected_before_a_job_is_created(service):
    """No knowledge-base row means no committed job."""

    state, _queue, _ingester, runtime = service
    with pytest.raises(KnowledgeValidationError, match="Knowledge base is not configured"):
        runtime.add_document("other", "manual.txt", b"x")
    assert outbox_rows(state) == []


def test_terminated_job_acknowledges_and_keeps_first_terminal_status(service):
    """Terminal replay is an ACK; a later failure cannot overwrite success."""

    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    original = state.document_jobs.get(artifact_id)
    assert state.document_jobs.dead_letter(queue.sent[0]) is True
    row = state.document_jobs.get(artifact_id)
    assert row["status"] == "succeeded" and row["stage"] == original["stage"]
    assert state.document_jobs.fail(row, retry=False, error="late failure") is None
    assert state.document_jobs.get(artifact_id)["status"] == "succeeded"


def test_dead_letter_finalizes_a_non_terminal_job(service):
    """Broker retry exhaustion turns an unattempted job into a failed one."""

    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    second = submit(service, b"never started", name="second.txt")
    assert state.document_jobs.dead_letter(queue.sent[-1]) is True
    status = runtime.get_document_status("manuals", second)
    assert status["status"] == "failed" and status["stage"] == "failed"
    assert status["error"] == "Message retry limit reached"
    assert ingester.calls == [b"original"]
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


@pytest.mark.parametrize("name", ["../a.txt", "a.exe", "CON.txt", "a/b.txt", "", "x" * 201])
def test_filename_validation_happens_before_any_row_is_written(service, name):
    state, _queue, _ingester, runtime = service
    with pytest.raises(KnowledgeValidationError):
        runtime.add_document("manuals", name, b"x")
    assert outbox_rows(state) == []


@pytest.mark.parametrize("content", [b"", "not bytes", b"x" * (MAX_FILE_BYTES + 1)],
                         ids=["empty", "not-bytes", "over-limit"])
def test_submission_preserves_nonempty_ten_mib_limit(service, content):
    state, _queue, _ingester, runtime = service
    with pytest.raises(KnowledgeValidationError, match="10 MiB"):
        runtime.add_document("manuals", "manual.txt", content)
    assert outbox_rows(state) == []


@pytest.mark.parametrize("key", ["", "x" * 129, 1, True, [], {}])
def test_idempotency_key_bounds_are_validated_before_any_row_is_written(service, key):
    state, _queue, _ingester, runtime = service
    with pytest.raises(KnowledgeValidationError, match="idempotency_key"):
        runtime.add_document("manuals", "manual.txt", b"x", idempotency_key=key)
    assert outbox_rows(state) == []


def test_same_key_same_content_reuses_the_artifact_without_republishing(service):
    state, queue, _ingester, runtime = service
    first = submit(service, b"original", idempotency_key="retry")
    assert submit(service, b"original", idempotency_key="retry") == first
    assert len(queue.sent) == 1
    assert len(outbox_rows(state)) == 1
    with pytest.raises(UploadConflict, match="different file input"):
        submit(service, b"different", idempotency_key="retry")


def test_conflicting_filename_against_queued_job_and_published_document(service):
    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    with pytest.raises(UploadConflict, match="already queued"):
        runtime.add_document("manuals", "MANUAL.txt", b"different")
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    with pytest.raises(UploadConflict, match="already exists"):
        runtime.add_document("manuals", "manual.txt", b"different")
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"


def test_duplicate_content_under_another_name_is_allowed(service):
    state, _queue, _ingester, runtime = service
    submit(service, b"same", name="a.txt")
    submit(service, b"same", name="b.txt")
    assert {row["name"] for row in outbox_rows(state)} == {"a.txt", "b.txt"}


def test_sync_write_and_delete_paths_are_unaffected(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    assert runtime.write_document("manuals", "manual.txt", b"updated", overwrite=True)["status"] == "updated"
    assert state.blobs.get("manuals", "source", "manual.txt") == b"updated"
    assert runtime.delete("manuals", "manual.txt")["deleted"]["name"] == "manual.txt"
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "succeeded"
    assert runtime.list_documents("manuals") == []


def test_maximum_upload_is_one_small_notification(service):
    state, queue, ingester, runtime = service
    content = b"z" * MAX_FILE_BYTES
    artifact_id = submit(service, content, name="large.txt")
    assert len(queue.sent) == 1
    assert len(json.dumps(queue.sent[0]).encode()) < 1024
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    assert ingester.calls == [content]
    assert state.document_jobs.get(artifact_id)["size"] == MAX_FILE_BYTES
    assert len(state.blobs.get("manuals", "source", "large.txt")) == MAX_FILE_BYTES


def test_attempts_and_execution_identity_are_recorded_per_claim(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service)
    envelope = queue.sent[0]
    first = state.document_jobs.claim(envelope)
    assert first["status"] == "processing" and first["attempts"] == 1
    assert first["content"] == b"original"
    assert first["execution_id"] and first["lease_until"] > 0
    state.document_jobs.fail(first, retry=True, error="retry")
    second = state.document_jobs.claim(envelope)
    assert second is not None and second["attempts"] == 2
    assert second["execution_id"] != first["execution_id"]
    assert state.document_jobs.progress(second, "indexing") is None
    assert state.document_jobs.get(artifact_id)["stage"] == "indexing"
    assert state.document_jobs.heartbeat(second) is None
    assert ingester.calls == []


def test_outbox_accessors_expose_the_committed_notification(service):
    state, _queue, _ingester, _runtime = service
    use_transport(state, None)
    artifact_id = submit(service)
    rows = state.document_jobs.pending_outbox()
    assert [row["artifact_id"] for row in rows] == [artifact_id]
    event_id = rows[0]["id"]
    assert state.document_jobs.outbox_event(event_id)["artifact_id"] == artifact_id
    assert state.document_jobs.outbox_event("missing") is None
    state.document_jobs.mark_published(event_id)
    assert state.document_jobs.outbox_event(event_id)["status"] == "PUBLISHED"
    assert state.document_jobs.outbox_event(event_id)["published_at"] is not None
    assert state.document_jobs.pending_outbox() == []
