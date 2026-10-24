"""Durable admission and generation-fenced leased document outbox recovery."""

import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import replace

import pytest
from sqlalchemy import create_engine, inspect, select, text, update

from rag_mcp.service_knowledge import RagServiceRuntime
from rag_mcp.state.document_tasks import DocumentDispatchSettings
from rag_mcp.state.schema import document_task_outbox as outbox
from rag_mcp.state.schema import document_tasks as tasks
from rag_mcp.state.schema_migrations import migrate_document_outbox
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores


class Transport:
    def __init__(self):
        self.sent = []
        self.error = None

    def send(self, artifact_id):
        self.sent.append(artifact_id)
        if self.error:
            raise self.error

    def close(self):
        pass


@pytest.fixture
def state(tmp_path, monkeypatch):
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    instance = StateStores(StateSettings(url=f"sqlite+pysqlite:///{tmp_path / 'relay.sqlite3'}"))
    instance.knowledge_bases.create({"namespace": "ns", "collection": "test"})
    instance.document_tasks.dispatch_settings = DocumentDispatchSettings(retry_seconds=10, max_retry_seconds=60)
    yield instance
    instance.close()


def submit(state, name="a.txt"):
    return state.document_dispatcher.submit("ns", name, b"source", idempotency_key=name)["artifact_id"]


def due(state, artifact_id):
    with state.engine.begin() as conn:
        conn.execute(update(outbox).where(outbox.c.artifact_id == artifact_id).values(retry_at=0, lease_until=0))
    state.document_tasks._dispatch_pause_until = 0


def delivery(state, artifact_id):
    with state.engine.connect() as conn:
        return dict(conn.execute(select(outbox).where(outbox.c.artifact_id == artifact_id)).mappings().one())


def test_admission_never_calls_transport_or_redis_and_works_without_queue(state, monkeypatch):
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = state
    transport = Transport()
    transport.error = RuntimeError("broker must not be called")
    state.document_dispatcher.transport = transport

    def forbidden(*_args, **_kwargs):
        pytest.fail("Admission performed cache/network work")

    monkeypatch.setattr(state.document_tasks, "flush", forbidden)
    result = runtime.add_document("ns", "a.txt", b"source", idempotency_key="same")
    assert transport.sent == []
    state.document_dispatcher.transport = None
    assert runtime.add_document("ns", "a.txt", b"source", idempotency_key="same") == result
    with state.engine.connect() as conn:
        assert conn.scalar(select(tasks.c.content)) == b"source"
    assert delivery(state, result["artifact_id"])["pending"]


def test_failure_is_persisted_with_backoff_without_spending_processing_budget(state):
    transport = Transport()
    transport.error = ValueError("private secret must not be stored")
    artifact_id = submit(state)
    assert state.document_tasks.dispatch(artifact_id, transport) is False
    record = delivery(state, artifact_id)
    assert record["pending"] and record["owner"] is None and record["lease_until"] == 0
    assert record["attempts"] == 1 and record["retry_at"] > 0
    assert record["last_error"] == "ValueError"
    status = state.document_tasks.status("ns", artifact_id)
    assert status["status"] == "queued" and status["stage"] == "dispatch_retry"
    with state.engine.connect() as conn:
        assert conn.scalar(select(tasks.c.attempts)) == 0
    assert state.document_tasks.dispatch(artifact_id, transport) is False
    assert len(transport.sent) == 1
    transport.error = None
    due(state, artifact_id)
    assert state.document_tasks.dispatch(artifact_id, transport) is True
    assert not delivery(state, artifact_id)["pending"]
    assert state.document_tasks.status("ns", artifact_id)["stage"] == "queued"


def test_one_bad_message_does_not_starve_other_documents(state):
    first, second = submit(state), submit(state, "b.txt")

    class Selective(Transport):
        def send(self, artifact_id):
            self.sent.append(artifact_id)
            if artifact_id == first:
                raise ValueError("invalid payload")

    transport = Selective()
    state.document_dispatcher.transport = transport
    state.document_dispatcher.run_once()
    assert set(transport.sent) == {first, second}
    assert delivery(state, first)["pending"]
    assert not delivery(state, second)["pending"]


def test_broker_outage_pauses_batch_and_can_recover(state):
    ids = [submit(state, f"{index}.txt") for index in range(3)]
    transport = Transport()
    transport.error = ConnectionError("down")
    state.document_dispatcher.transport = transport
    state.document_dispatcher.run_once()
    assert len(transport.sent) == 1
    state.document_dispatcher.run_once()
    assert len(transport.sent) == 1
    transport.error = None
    for artifact_id in ids:
        due(state, artifact_id)
    state.document_dispatcher.run_once()
    assert all(not delivery(state, artifact_id)["pending"] for artifact_id in ids)


def test_only_one_relay_sends_while_lease_live_and_sql_not_held_during_send(state):
    artifact_id = submit(state)
    other = StateStores(state.settings)
    entered, release = threading.Event(), threading.Event()

    class Slow(Transport):
        def send(self, incoming):
            self.sent.append(incoming)
            entered.set()
            assert release.wait(5)

    transport = Slow()
    try:
        with ThreadPoolExecutor(max_workers=1) as pool:
            future = pool.submit(state.document_tasks.dispatch, artifact_id, transport)
            try:
                assert entered.wait(2)
                assert other.document_tasks.dispatch(artifact_id, transport) is False
                # An independent SQL writer can accept another document during MQ IO.
                assert other.document_tasks.submit("ns", "b.txt", b"second")
            finally:
                release.set()
            assert future.result(timeout=5)
        assert transport.sent == [artifact_id]
    finally:
        other.close()


def test_crashed_relay_is_reclaimed_and_old_confirmation_cannot_win(state):
    artifact_id = submit(state)
    old = state.document_tasks._delivery_claim(artifact_id)
    assert old is not None
    due(state, artifact_id)
    fresh = state.document_tasks._delivery_claim(artifact_id)
    assert fresh["owner"] != old["owner"]
    state.document_tasks._delivery_result(old)
    assert delivery(state, artifact_id)["owner"] == fresh["owner"]
    assert delivery(state, artifact_id)["pending"]
    state.document_tasks._delivery_result(fresh)
    assert not delivery(state, artifact_id)["pending"]


def test_expired_sender_cannot_clear_pending_confirmation(state):
    artifact_id = submit(state)
    sender = state.document_tasks._delivery_claim(artifact_id)
    due(state, artifact_id)
    state.document_tasks._delivery_result(sender)
    assert delivery(state, artifact_id)["pending"]
    assert state.document_tasks.dispatch(artifact_id, Transport())


def test_broker_sdk_outage_categories_are_recognized():
    pytest.importorskip("rocketmq")
    from rocketmq.v5.exception.client_exception import ProxyTimeoutException

    from rag_mcp.state.document_tasks import DocumentTasks

    assert DocumentTasks._broker_unavailable(ProxyTimeoutException("timeout"))
    assert not DocumentTasks._broker_unavailable(ValueError("invalid message"))


def test_consumer_retry_during_send_does_not_change_delivery_generation(state):
    artifact_id = submit(state)

    class RetryDuringSend(Transport):
        def send(self, incoming):
            task = state.document_tasks.claim(incoming)
            state.document_tasks.fail(task, retry=True, error="retry")

    state.document_tasks.dispatch(artifact_id, RetryDuringSend())
    record = delivery(state, artifact_id)
    assert record["generation"] == 1 and not record["pending"]
    assert record["owner"] is None and record["attempts"] == 1
    assert state.document_tasks.status("ns", artifact_id)["stage"] == "retry_wait"


def test_send_ack_lost_keeps_task_and_allows_duplicate_delivery(state):
    artifact_id = submit(state)
    transport = Transport()
    transport.error = TimeoutError("Broker accepted but acknowledgement was lost")
    state.document_tasks.dispatch(artifact_id, transport)
    assert delivery(state, artifact_id)["pending"]
    due(state, artifact_id)
    transport.error = None
    state.document_tasks.dispatch(artifact_id, transport)
    assert transport.sent == [artifact_id, artifact_id]


def test_dispatcher_only_dispatches_pending_without_consumer_recovery(state, monkeypatch):
    artifact_id = submit(state)
    transport = Transport()
    transport.error = ValueError("bad")
    state.document_dispatcher.transport = transport
    assert state.document_dispatcher.run_once() == 0
    original = delivery(state, artifact_id)

    def forbidden(*args, **kwargs):
        pytest.fail("Outbox relay must not schedule consumer execution")

    monkeypatch.setattr(state.document_tasks, "redrive_expired", forbidden, raising=False)
    assert state.document_dispatcher.run_once() == 0
    assert delivery(state, artifact_id) == original
    due(state, artifact_id)
    transport.error = None
    assert state.document_dispatcher.run_once() == 1
    task = state.document_tasks.claim(artifact_id)
    assert task is not None
    with state.engine.begin() as conn:
        conn.execute(update(tasks).where(tasks.c.artifact_id == artifact_id).values(updated_at=0))
    archived = delivery(state, artifact_id)
    assert state.document_dispatcher.run_once() == 0
    assert delivery(state, artifact_id) == archived
    assert transport.sent == [artifact_id, artifact_id]
    assert state.document_tasks.status("ns", artifact_id)["status"] == "processing"


def test_abandoned_processing_does_not_republish_acknowledged_delivery(state):
    artifact_id = submit(state)
    transport = Transport()
    state.document_dispatcher.transport = transport
    assert state.document_dispatcher.run_once() == 1
    assert state.document_tasks.claim(artifact_id)
    with state.engine.begin() as conn:
        conn.execute(update(tasks).where(tasks.c.artifact_id == artifact_id).values(updated_at=0))
    original = delivery(state, artifact_id)
    assert state.document_dispatcher.run_once() == 0
    assert delivery(state, artifact_id) == original
    assert not original["pending"] and original["generation"] == 1
    assert transport.sent == [artifact_id]
    assert state.document_tasks.claim(artifact_id) is not None


def test_dead_letter_during_send_keeps_terminal_state(state):
    artifact_id = submit(state)

    class DeadLetterDuringSend(Transport):
        def send(self, incoming):
            assert state.document_tasks.dead_letter(incoming)

    state.document_tasks.dispatch(artifact_id, DeadLetterDuringSend())
    status = state.document_tasks.status("ns", artifact_id)
    assert status["status"] == "failed" and status["stage"] == "dead_letter"
    assert status["error"] == "Message retry limit reached"
    assert not delivery(state, artifact_id)["pending"]
    assert state.document_tasks.claim(artifact_id) is None


def test_dispatch_retries_do_not_change_consumer_state_or_delete_file(state):
    artifact_id = submit(state)
    transport = Transport()
    transport.error = ValueError("bad config")
    for _ in range(5):
        due(state, artifact_id)
        state.document_tasks.dispatch(artifact_id, transport)
    assert delivery(state, artifact_id)["pending"]
    assert delivery(state, artifact_id)["attempts"] == 5
    with state.engine.connect() as conn:
        row = conn.execute(select(tasks.c.attempts, tasks.c.status, tasks.c.content)).one()
    assert row == (0, "queued", b"source")


def test_relay_restart_recovers_unconfirmed_send(state):
    artifact_id = submit(state)
    assert state.document_tasks._delivery_claim(artifact_id)
    due(state, artifact_id)
    other = StateStores(state.settings)
    transport = Transport()
    other.document_dispatcher.transport = transport
    try:
        other.document_dispatcher.run_once()
        assert transport.sent == [artifact_id]
    finally:
        other.close()


def test_existing_database_outbox_upgrade_is_additive_and_repeatable(tmp_path):
    engine = create_engine(f"sqlite+pysqlite:///{tmp_path / 'old.sqlite3'}")
    try:
        with engine.begin() as conn:
            conn.execute(text("CREATE TABLE document_task_outbox (artifact_id VARCHAR(36) PRIMARY KEY, pending BOOLEAN NOT NULL, generation BIGINT NOT NULL, retry_at DOUBLE NOT NULL)"))
            conn.execute(text("INSERT INTO document_task_outbox VALUES ('existing', 1, 7, 12)"))
        migrate_document_outbox(engine)
        migrate_document_outbox(engine)
        assert {"owner", "lease_until", "attempts", "last_error"} <= {
            column["name"] for column in inspect(engine).get_columns("document_task_outbox")}
        with engine.connect() as conn:
            row = conn.execute(text("SELECT * FROM document_task_outbox")).mappings().one()
        assert row["pending"] and row["generation"] == 7 and row["retry_at"] == 12
        assert row["owner"] is None and row["lease_until"] == 0 and row["attempts"] == 0
    finally:
        engine.dispose()


def test_state_store_automatically_upgrades_old_outbox(tmp_path):
    path = tmp_path / "old-state.sqlite3"
    engine = create_engine(f"sqlite+pysqlite:///{path}")
    with engine.begin() as conn:
        conn.execute(text("CREATE TABLE document_task_outbox (artifact_id VARCHAR(36) PRIMARY KEY, pending BOOLEAN NOT NULL, generation BIGINT NOT NULL, retry_at DOUBLE NOT NULL)"))
    engine.dispose()
    state = StateStores(StateSettings(url=f"sqlite+pysqlite:///{path}"))
    try:
        state.knowledge_bases.create({"namespace": "ns", "collection": "test"})
        artifact_id = submit(state)
        assert delivery(state, artifact_id)["lease_until"] == 0
    finally:
        state.close()


def test_backlog_warning_and_health(state, caplog):
    submit(state)
    state.document_tasks.dispatch_settings = replace(state.document_tasks.dispatch_settings, alert_seconds=0.01)
    with state.engine.begin() as conn:
        conn.execute(update(tasks).values(created_at=0))
    metrics = state.document_tasks.delivery_health()
    assert metrics["pending"] == 1 and metrics["oldest_seconds"] > 0
    assert "backlog" in caplog.text
