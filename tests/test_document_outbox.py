"""Transactional outbox: relay bridge, Debezium compatibility and maintenance.

Submission itself is covered in ``test_document_tasks``; this module focuses on
turning committed outbox rows into notifications (or deliberately not doing so
under the Debezium bridge) and on ``sweep``/``gc_outbox`` maintenance.
"""

import json
import time
from uuid import uuid4

import pytest
from redis_stub import MemoryRedis
from sqlalchemy import select, update

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.document_worker import DocumentWorker
from rag_mcp.state.cache import RedisStateCache, StateCacheSettings
from rag_mcp.state.document_jobs import (
    document_job_envelope,
    validate_document_envelope,
)
from rag_mcp.state.document_queue import document_bridge_mode
from rag_mcp.state.schema import document_job_outbox, document_jobs
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from test_document_tasks import Queue, build_service, submit, use_transport

ARTIFACT = "d1feec53-1036-4af0-8035-86ef6990fa6a"


@pytest.fixture
def service(tmp_path, monkeypatch):
    state, queue, ingester, runtime = build_service(tmp_path, monkeypatch)
    try:
        yield state, queue, ingester, runtime
    finally:
        state.close()


def build_state(tmp_path, monkeypatch, mode=None, name="outbox.sqlite3"):
    if mode is None:
        monkeypatch.delenv("RAG_OUTBOX_BRIDGE", raising=False)
    else:
        monkeypatch.setenv("RAG_OUTBOX_BRIDGE", mode)
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    state = StateStores(StateSettings(url=f"sqlite+pysqlite:///{tmp_path / name}"))
    state.cache_sync.backend = RedisStateCache(StateCacheSettings(), client=MemoryRedis())
    state.knowledge_bases.create({
        "namespace": "manuals", "collection": "test", "embedding_model": "test",
        "tokenizer": "test", **ChunkingConfig().metadata("test"),
    })
    return state


def outbox_rows(state):
    with state.engine.connect() as conn:
        return [dict(row) for row in conn.execute(select(document_job_outbox)).mappings()]


def age_outbox(state, artifact_id, seconds=8 * 24 * 3600):
    with state.engine.begin() as conn:
        conn.execute(update(document_job_outbox).where(
            document_job_outbox.c.artifact_id == artifact_id).values(
            created_at=time.time() - seconds))


def age_job(state, artifact_id, seconds=3600):
    with state.engine.begin() as conn:
        conn.execute(update(document_jobs).where(
            document_jobs.c.artifact_id == artifact_id).values(
            updated_at=time.time() - seconds))


def test_broker_failure_never_fails_a_committed_submission(service):
    """The durable transaction is the acceptance point, not the broker send."""

    state, queue, ingester, runtime = service
    queue.error = ConnectionError("private broker detail")
    artifact_id = submit(service, b"retry me", idempotency_key="retry")
    assert state.document_jobs.get(artifact_id)["status"] == "queued"
    rows = outbox_rows(state)
    assert len(rows) == 1 and rows[0]["status"] == "PENDING"
    assert rows[0]["last_error"] == "ConnectionError" and rows[0]["attempts"] == 1
    assert rows[0]["retry_at"] > time.time()
    queue.error = None
    relay = state.document_dispatcher.relay
    assert relay.publish_one(rows[0]["id"]) is True
    assert outbox_rows(state)[0]["status"] == "PUBLISHED"
    assert [message["artifact_id"] for message in queue.sent] == [artifact_id]
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    assert ingester.calls == [b"retry me"]


def test_relay_publish_pending_publishes_due_rows_and_marks_them(service):
    state, queue, _ingester, _runtime = service
    use_transport(state, None)
    first = submit(service, b"one", name="a.txt")
    second = submit(service, b"two", name="b.txt")
    use_transport(state, queue)
    with state.engine.begin() as conn:
        conn.execute(update(document_job_outbox).where(
            document_job_outbox.c.artifact_id == second).values(retry_at=time.time() + 300))
    assert state.document_dispatcher.relay.publish_pending() == 1
    assert [message["artifact_id"] for message in queue.sent] == [first]
    statuses = {row["artifact_id"]: row["status"] for row in outbox_rows(state)}
    assert statuses == {first: "PUBLISHED", second: "PENDING"}
    with state.engine.begin() as conn:
        conn.execute(update(document_job_outbox).values(retry_at=0.0))
    assert state.document_dispatcher.relay.publish_pending() == 1
    assert {message["artifact_id"] for message in queue.sent} == {first, second}


def test_relay_keeps_the_row_pending_and_records_a_bounded_error(service):
    state, queue, _ingester, _runtime = service
    use_transport(state, None)
    submit(service, b"pending")
    use_transport(state, queue)
    queue.error = TimeoutError("private broker credential")
    assert state.document_dispatcher.relay.publish_pending() == 0
    row = outbox_rows(state)[0]
    assert row["status"] == "PENDING"
    assert row["attempts"] == 1 and row["last_error"] == "TimeoutError"
    assert row["retry_at"] > time.time()
    assert queue.sent == []


def test_publish_one_ignores_missing_and_already_published_events(service):
    state, queue, _ingester, _runtime = service
    relay = state.document_dispatcher.relay
    assert relay.publish_one("missing") is False
    artifact_id = submit(service)
    rows = outbox_rows(state)
    assert rows[0]["status"] == "PUBLISHED" and rows[0]["artifact_id"] == artifact_id
    assert relay.publish_one(rows[0]["id"]) is False
    assert len(queue.sent) == 1


def test_relay_without_broker_keeps_the_notification_pending(service):
    state, _queue, _ingester, runtime = service
    use_transport(state, None)
    artifact_id = submit(service, b"no broker")
    assert state.document_dispatcher.relay.publish_pending() == 0
    assert state.document_dispatcher.relay.transport is None
    assert outbox_rows(state)[0]["status"] == "PENDING"
    assert state.document_jobs.get(artifact_id)["status"] == "queued"
    assert runtime.get_document_status("manuals", artifact_id)["status"] == "queued"


def test_relay_background_thread_publishes_pending_rows_and_stops(service):
    state, queue, _ingester, _runtime = service
    use_transport(state, None)
    artifact_id = submit(service, b"deferred")
    relay = state.document_dispatcher.relay
    relay.poll_seconds = 0.01
    use_transport(state, queue)
    relay.start()
    try:
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and not queue.sent:
            time.sleep(0.01)
        assert [message["artifact_id"] for message in queue.sent] == [artifact_id]
    finally:
        relay.stop()
    assert relay._thread is None
    relay.stop()  # idempotent


def test_default_bridge_mode_publishes_in_process(tmp_path, monkeypatch):
    state = build_state(tmp_path, monkeypatch, name="relay.sqlite3")
    try:
        assert document_bridge_mode() == "relay"
        dispatcher = state.document_dispatcher
        assert dispatcher.mode == "relay" and dispatcher.relay is not None
        queue = Queue()
        use_transport(state, queue)
        dispatcher.submit("manuals", "manual.txt", b"relay")
        assert [message["artifact_id"] for message in queue.sent] == [
            state.document_jobs.pending_outbox()[0]["artifact_id"]
            if state.document_jobs.pending_outbox() else queue.sent[0]["artifact_id"]]
        assert outbox_rows(state)[0]["status"] == "PUBLISHED"
    finally:
        state.close()


def test_debezium_bridge_never_publishes_from_the_application(tmp_path, monkeypatch):
    state = build_state(tmp_path, monkeypatch, mode="debezium", name="cdc.sqlite3")
    try:
        dispatcher = state.document_dispatcher
        assert dispatcher.mode == "debezium" and dispatcher.relay is None
        queue = Queue()
        use_transport(state, queue)
        artifact_id = dispatcher.submit("manuals", "manual.txt", b"cdc")["artifact_id"]
        assert queue.sent == []
        rows = outbox_rows(state)
        assert len(rows) == 1 and rows[0]["status"] == "PENDING"
        assert rows[0]["artifact_id"] == artifact_id
        assert state.document_jobs.get(artifact_id)["content"] == b"cdc"
    finally:
        state.close()


@pytest.mark.parametrize("mode", ["kafka", "polling", " "])
def test_invalid_bridge_mode_is_rejected(tmp_path, monkeypatch, mode):
    monkeypatch.setenv("RAG_OUTBOX_BRIDGE", mode)
    with pytest.raises(ValueError, match="relay or debezium"):
        document_bridge_mode()
    with pytest.raises(ValueError, match="relay or debezium"):
        StateStores(StateSettings(url=f"sqlite+pysqlite:///{tmp_path / 'bad.sqlite3'}"))


def test_notification_shape_is_content_free(service):
    state, queue, _ingester, _runtime = service
    content = b"FILEDATA" * 512
    artifact_id = submit(service, content)
    assert len(queue.sent) == 1
    message = queue.sent[0]
    assert set(message) == {"schema", "artifact_id", "namespace", "name", "created_at"}
    assert message["schema"] == 3 and message["artifact_id"] == artifact_id
    assert message["namespace"] == "manuals" and message["name"] == "manual.txt"
    body = json.dumps(message).encode()
    assert b"FILEDATA" not in body and b"content" not in body
    assert len(body) < 1024
    # The relay hands the transport exactly what the broker receives.
    assert document_job_envelope(state.document_jobs.get(artifact_id)) == message


def test_sweep_reemits_notification_for_a_stranded_queued_job(service):
    """A delivered-then-lost notification is regenerated as a new outbox row."""

    state, _queue, _ingester, _runtime = service
    artifact_id = submit(service, b"stranded")
    assert outbox_rows(state)[0]["status"] == "PUBLISHED"
    age_job(state, artifact_id)
    age_outbox(state, artifact_id)
    assert state.document_jobs.sweep() == 1
    pending = state.document_jobs.pending_outbox()
    assert [row["artifact_id"] for row in pending] == [artifact_id]
    assert len(outbox_rows(state)) == 2
    # A fresh notification row now exists, so a second pass has nothing to re-emit.
    assert state.document_jobs.sweep() == 0


def test_sweep_leaves_fresh_or_pending_jobs_alone(service):
    state, queue, _ingester, _runtime = service
    use_transport(state, None)
    fresh = submit(service, b"fresh")
    assert state.document_jobs.sweep() == 0
    # Even an old job with a due PENDING row is not re-emitted twice.
    age_job(state, fresh)
    assert state.document_jobs.sweep() == 0
    assert len(outbox_rows(state)) == 1
    use_transport(state, queue)
    assert state.document_dispatcher.relay.publish_pending() == 1
    assert outbox_rows(state)[0]["status"] == "PUBLISHED"


def test_sweep_requeues_a_processing_job_with_an_expired_lease(service):
    state, queue, ingester, runtime = service
    artifact_id = submit(service, b"expired")
    envelope = queue.sent[0]
    task = state.document_jobs.claim(envelope)
    assert task is not None and task["execution_id"]
    with state.engine.begin() as conn:
        conn.execute(update(document_jobs).where(
            document_jobs.c.artifact_id == artifact_id).values(lease_until=0.0))
    assert state.document_jobs.sweep() == 1
    row = state.document_jobs.get(artifact_id)
    assert (row["status"], row["stage"]) == ("queued", "retry_wait")
    assert row["owner"] is None and row["lease_until"] == 0.0
    assert [item["artifact_id"] for item in state.document_jobs.pending_outbox()] == [artifact_id]
    # The requeued job is then processable by a normal delivery.
    assert DocumentWorker(runtime).process(envelope) is True
    assert ingester.calls == [b"expired"]


def test_sweep_does_not_touch_terminal_jobs(service):
    state, queue, _ingester, runtime = service
    artifact_id = submit(service)
    assert DocumentWorker(runtime).process(queue.sent[0]) is True
    age_job(state, artifact_id)
    with state.engine.begin() as conn:
        conn.execute(update(document_job_outbox).values(status="PUBLISHED"))
    assert state.document_jobs.sweep() == 0
    assert state.document_jobs.get(artifact_id)["status"] == "succeeded"


def test_sweep_does_not_duplicate_a_recent_published_notification(service):
    state, _queue, _ingester, _runtime = service
    artifact_id = submit(service, b"recent")
    age_job(state, artifact_id)
    assert outbox_rows(state)[0]["status"] == "PUBLISHED"
    assert state.document_jobs.sweep() == 0


def test_gc_terminal_removes_only_old_terminal_jobs(service):
    state, queue, _ingester, runtime = service
    finished = submit(service, b"finished", name="finished.txt")
    assert DocumentWorker(runtime).process(queue.sent[-1]) is True
    queued = submit(service, b"waiting", name="waiting.txt")
    age_job(state, finished, seconds=40 * 24 * 3600)
    assert state.document_jobs.gc_terminal() == 1
    assert state.document_jobs.get(finished) is None
    assert state.document_jobs.get(queued) is not None
    assert state.document_jobs.gc_terminal(retain_seconds=0) == 0


def test_gc_outbox_respects_the_retention_window(service, monkeypatch):
    state, _queue, _ingester, _runtime = service
    old = submit(service, b"old", name="old.txt")
    keep = submit(service, b"keep", name="keep.txt")
    age_outbox(state, old)
    assert state.document_jobs.gc_outbox() == 1
    assert {row["artifact_id"] for row in outbox_rows(state)} == {keep}
    assert state.document_jobs.gc_outbox(max_age=0) == 1
    assert outbox_rows(state) == []
    # The job rows themselves are never garbage collected by outbox upkeep.
    assert state.document_jobs.get(old) is not None and state.document_jobs.get(keep) is not None


@pytest.mark.parametrize("seconds", ["0", "-1", "not-a-number"])
def test_retention_and_lease_settings_must_be_positive(monkeypatch, seconds):
    monkeypatch.setenv("RAG_DOCUMENT_OUTBOX_RETENTION_SECONDS", seconds)
    monkeypatch.setenv("RAG_DOCUMENT_JOB_LEASE_SECONDS", seconds)
    from rag_mcp.state.document_jobs import job_lease_seconds, outbox_retention_seconds

    with pytest.raises(ValueError):
        outbox_retention_seconds()
    if seconds == "not-a-number":
        with pytest.raises(ValueError):
            job_lease_seconds()
    else:
        with pytest.raises(ValueError, match="must be positive"):
            job_lease_seconds()


def test_native_envelope_round_trip_and_debezium_compatibility():
    payload = {"schema": 3, "artifact_id": ARTIFACT, "namespace": "manuals",
               "name": "manual.txt", "created_at": 12.5}
    assert validate_document_envelope(payload) == payload
    created = validate_document_envelope({"before": None, "op": "c", "after": {
        "artifact_id": ARTIFACT, "namespace": "manuals", "name": "manual.txt",
        "created_at": 3}})
    assert created == {"schema": 3, "artifact_id": ARTIFACT, "namespace": "manuals",
                       "name": "manual.txt", "created_at": 3.0}
    for op in ("r", "u"):
        assert validate_document_envelope({
            "op": op, "after": {"artifact_id": ARTIFACT, "namespace": "manuals",
                                "name": "manual.txt", "created_at": 0}}) == {
            **created, "created_at": 0.0}


@pytest.mark.parametrize("payload", [
    None, [], "notification", 3,
    {"artifact_id": ARTIFACT},
    {"schema": 2, "artifact_id": ARTIFACT, "namespace": "manuals", "name": "manual.txt"},
    {"schema": 3, "artifact_id": str(uuid4()).replace("-", ""), "namespace": "manuals",
     "name": "manual.txt"},
    {"schema": 3, "artifact_id": "not-a-uuid", "namespace": "manuals", "name": "manual.txt"},
    {"schema": 3, "artifact_id": ARTIFACT, "namespace": "../other", "name": "manual.txt"},
    {"schema": 3, "artifact_id": ARTIFACT, "namespace": "manuals", "name": "../a.txt"},
    {"schema": 3, "artifact_id": ARTIFACT, "namespace": "manuals", "name": "a.exe"},
    {"schema": 3, "artifact_id": ARTIFACT, "namespace": "manuals", "name": "manual.txt",
     "created_at": -1},
    {"schema": 3, "artifact_id": ARTIFACT, "namespace": "manuals", "name": "manual.txt",
     "created_at": "now"},
    {"op": "d", "after": {"artifact_id": ARTIFACT, "namespace": "manuals",
                          "name": "manual.txt"}},
    {"op": "c", "after": {"artifact_id": ARTIFACT, "namespace": "manuals"}},
])
def test_malformed_notifications_are_rejected(payload):
    with pytest.raises((ValueError, TypeError)):
        validate_document_envelope(payload)


def test_job_lease_seconds_default_and_override(monkeypatch):
    from rag_mcp.state.document_jobs import heartbeat_seconds, job_lease_seconds, sweep_after_seconds

    monkeypatch.delenv("RAG_DOCUMENT_JOB_LEASE_SECONDS", raising=False)
    monkeypatch.delenv("RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS", raising=False)
    monkeypatch.delenv("RAG_DOCUMENT_JOB_SWEEP_SECONDS", raising=False)
    assert job_lease_seconds() == 300.0 and heartbeat_seconds() == 20.0
    assert sweep_after_seconds() == 120.0
    monkeypatch.setenv("RAG_DOCUMENT_JOB_LEASE_SECONDS", "1.5")
    assert job_lease_seconds() == 1.5
