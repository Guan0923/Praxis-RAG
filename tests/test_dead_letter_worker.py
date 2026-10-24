"""DLQ acknowledgement follows committed SQL state, never inference or re-enqueue."""

import json
import subprocess
import sys
from types import SimpleNamespace
from uuid import uuid4

import pytest
from sqlalchemy import select, update

from rag_mcp.dead_letter_worker import DeadLetterWorker, dead_letter_queue_settings
from rag_mcp.state.document_queue import RocketMQDocumentQueue
from rag_mcp.state.schema import document_tasks, search_tasks
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores


class Consumer:
    def __init__(self, bodies):
        self.messages = [SimpleNamespace(body=body) for body in bodies]
        self.acks = []
        self.closed = False

    def startup(self):
        pass

    def receive(self, *_):
        return self.messages

    def ack(self, message):
        self.acks.append(message)

    def shutdown(self):
        self.closed = True


@pytest.fixture
def state(monkeypatch):
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    monkeypatch.setenv("RAG_REDIS_ENABLED", "0")
    result = StateStores(StateSettings(url="sqlite+pysqlite:///:memory:"))
    result.knowledge_bases.create({"namespace": "business_namespace", "collection": "test"})
    yield result
    result.close()


def submit(state, kind):
    if kind == "document":
        return state.document_tasks.submit("business_namespace", "manual.txt", b"content")
    return state.search_tasks.submit("business_namespace", {"query": "manual"})


def deliver(state, kind, task_id):
    key = "artifact_id" if kind == "document" else "result_id"
    consumer = Consumer([json.dumps({key: task_id}).encode()])
    queue = RocketMQDocumentQueue(dead_letter_queue_settings(kind), consumer_factory=lambda: consumer)
    queue.message_key = key
    return consumer, queue, DeadLetterWorker(state, kind)


@pytest.mark.parametrize("kind", ["document", "search"])
@pytest.mark.parametrize("status", ["queued", "processing", "succeeded"])
def test_dead_letters_fail_pending_preserve_succeeded_before_ack(state, kind, status):
    task_id = submit(state, kind)
    table = document_tasks if kind == "document" else search_tasks
    column = table.c.artifact_id if kind == "document" else table.c.result_id
    with state.engine.begin() as conn:
        conn.execute(update(table).where(column == task_id).values(status=status, stage="test"))
    consumer, queue, worker = deliver(state, kind, task_id)
    original = consumer.ack

    def ack(message):
        with state.engine.connect() as conn:
            row = conn.execute(select(table).where(column == task_id)).mappings().one()
        assert row["status"] == ("succeeded" if status == "succeeded" else "failed")
        assert row["stage"] == ("test" if status == "succeeded" else "dead_letter")
        original(message)

    consumer.ack = ack
    try:
        queue.consume_once(worker.process)
        queue.consume_once(worker.process)
        assert len(consumer.acks) == 2
    finally:
        queue.close()
    assert consumer.closed


@pytest.mark.parametrize("kind", ["document", "search"])
def test_database_exception_does_not_ack(state, kind, monkeypatch):
    task_id = submit(state, kind)
    consumer, queue, worker = deliver(state, kind, task_id)

    def fail(_):
        raise OSError("sensitive database credentials")

    monkeypatch.setattr(worker.tasks, "dead_letter", fail)
    try:
        with pytest.raises(OSError):
            queue.consume_once(worker.process)
        assert consumer.acks == []
    finally:
        queue.close()


@pytest.mark.parametrize("kind", ["document", "search"])
def test_unknown_canonical_ids_ack_after_authoritative_lookup(state, kind, caplog):
    consumer, queue, worker = deliver(state, kind, str(uuid4()))
    try:
        with caplog.at_level("INFO"):
            queue.consume_once(worker.process)
        assert len(consumer.acks) == 1
        assert "unknown" in caplog.text.lower()
    finally:
        queue.close()


@pytest.mark.parametrize("kind", ["document", "search"])
def test_poison_payload_never_reaches_sql_or_acks(kind, caplog):
    key = "artifact_id" if kind == "document" else "result_id"
    consumer = Consumer([b"not json", b"[]", b"null", b"\xff", b"{}",
                         json.dumps({key: "arbitrary-id-with-private-secret"}).encode(),
                         json.dumps({key: uuid4().hex}).encode()])
    queue = RocketMQDocumentQueue(dead_letter_queue_settings(kind), consumer_factory=lambda: consumer)
    queue.message_key = key
    try:
        queue.consume_once(lambda _: pytest.fail("poison reached callback"))
        assert consumer.acks == []
        assert "malformed" in caplog.text
        assert "private-secret" not in caplog.text
    finally:
        queue.close()


def test_dlq_settings_preserve_credentials_and_use_physical_not_business_namespace(monkeypatch):
    values = {"RAG_ROCKETMQ_ENDPOINT": "broker:8081", "RAG_ROCKETMQ_ACCESS_KEY": "access",
              "RAG_ROCKETMQ_SECRET_KEY": "secret", "RAG_ROCKETMQ_TLS": "1",
              "RAG_ROCKETMQ_NAMESPACE": "mq_metadata", "RAG_NAMESPACE": "business_namespace",
              "RAG_DOCUMENT_GROUP": "physical_ns%source", "RAG_DOCUMENT_DLQ_GROUP": "document_dlq",
              "RAG_SEARCH_GROUP": "source_search", "RAG_SEARCH_DLQ_GROUP": "search_dlq"}
    for name, value in values.items():
        monkeypatch.setenv(name, value)
    settings = dead_letter_queue_settings("document")
    assert settings.topic == "%DLQ%physical_ns%source"
    assert settings.group == "document_dlq"
    assert settings.namespace == "mq_metadata"
    assert (settings.endpoint, settings.access_key, settings.secret_key, settings.tls) == (
        "broker:8081", "access", "secret", True)
    assert dead_letter_queue_settings("search").topic == "%DLQ%source_search"


@pytest.mark.parametrize("group", ["", "bad group", "praxis_document_ingest_workers",
                                    "praxis_search_workers", "praxis_state_cache_refresh_workers",
                                    "praxis_search_dead_letters"])
def test_dlq_groups_must_be_independent_and_valid(monkeypatch, group):
    monkeypatch.setenv("RAG_DOCUMENT_DLQ_GROUP", group)
    with pytest.raises(ValueError, match="independent"):
        dead_letter_queue_settings("document")


def test_cli_import_does_not_import_inference_runtime():
    code = "import sys; import rag_mcp.dead_letter_worker; assert 'rag_mcp.service_knowledge' not in sys.modules; assert 'rag_mcp.http_service' not in sys.modules; assert 'torch' not in sys.modules"
    subprocess.run([sys.executable, "-c", code], check=True, capture_output=True, text=True)


@pytest.mark.parametrize("kind", ["document", "search"])
def test_main_loads_shared_config_and_closes_every_client(monkeypatch, kind):
    import rag_mcp.dead_letter_worker as module
    import rag_mcp.startup_config as startup

    events = []
    stop = SimpleNamespace(stopped=False)
    stop.is_set = lambda: stop.stopped
    stop.set = lambda: setattr(stop, "stopped", True)
    tasks = SimpleNamespace(dead_letter=lambda _: True)
    state = SimpleNamespace(document_tasks=tasks, search_tasks=tasks, close=lambda: events.append("state_close"))
    monkeypatch.setattr(startup, "load_service_config", lambda: events.append("config"))
    monkeypatch.setattr(StateStores, "create", lambda _: state)
    monkeypatch.setattr(module.threading, "Event", lambda: stop)
    monkeypatch.setattr(module.signal, "signal", lambda *_: None)
    monkeypatch.setenv("RAG_ROCKETMQ_ENDPOINT", "unused:8081")

    class Transport:
        def __init__(self, settings):
            assert settings.topic.startswith("%DLQ%")

        def consume_once(self, callback):
            assert self.message_key == ("artifact_id" if kind == "document" else "result_id")
            assert callback(str(uuid4())) is True
            stop.set()

        def close(self):
            events.append("transport_close")

    monkeypatch.setattr(module, "RocketMQDocumentQueue", Transport)
    assert module.main(["--kind", kind]) == 0
    assert events == ["config", "transport_close", "state_close"]


@pytest.mark.parametrize("kind", ["document", "search"])
def test_uncommitted_handler_result_never_acks(kind):
    tasks = SimpleNamespace(dead_letter=lambda _: False)
    state = SimpleNamespace(document_tasks=tasks, search_tasks=tasks)
    consumer, queue, worker = deliver(state, kind, str(uuid4()))
    try:
        queue.consume_once(worker.process)
        assert consumer.acks == []
    finally:
        queue.close()
