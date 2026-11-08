"""DLQ handling records durable terminal outcomes without inference or re-enqueue.

Document dead letters are normalised from the native schema-3 notification (or a
raw Debezium change event) and finalised in the ``document_jobs`` table.
"""

import subprocess
import sys
from types import SimpleNamespace
from uuid import uuid4

import pytest

from rag_mcp.dead_letter_worker import DeadLetterWorker, dead_letter_queue_settings
from rag_mcp.state.store import StateStores
from rag_mcp.state.transient import TransientUnavailable


def payload(kind, task_id=None):
    task_id = task_id or str(uuid4())
    if kind == "document":
        return {"schema": 3, "artifact_id": task_id, "namespace": "manuals",
                "name": "manual.txt", "created_at": 0.0}
    return {"schema": 3, "result_id": task_id}


@pytest.mark.parametrize("kind", ["document", "search"])
@pytest.mark.parametrize("complete", [True, False, None])
def test_ack_eligibility_follows_task_outcome(kind, complete, caplog):
    calls = []

    def dead_letter(message):
        calls.append(message)
        return complete

    tasks = SimpleNamespace(dead_letter=dead_letter)
    state = SimpleNamespace(document_jobs=tasks, search_tasks=tasks)
    key = "artifact_id" if kind == "document" else "result_id"
    incoming = payload(kind)
    task_id = incoming[key]
    with caplog.at_level("INFO"):
        result = DeadLetterWorker(state, kind).process(incoming)
    assert result is (complete is True)
    assert calls == [incoming]
    if complete is True:
        assert task_id in caplog.text


@pytest.mark.parametrize("kind", ["document", "search"])
def test_storage_failure_propagates_without_ack_eligibility(kind):
    def fail(_):
        raise TransientUnavailable("State write unavailable")

    tasks = SimpleNamespace(dead_letter=fail)
    state = SimpleNamespace(document_jobs=tasks, search_tasks=tasks)
    with pytest.raises(TransientUnavailable):
        DeadLetterWorker(state, kind).process(payload(kind))


@pytest.mark.parametrize("kind", ["document", "search"])
@pytest.mark.parametrize("incoming", [
    None, [], "legacy-id", {}, {"artifact_id": "bad", "result_id": "bad"},
])
def test_invalid_payload_never_reaches_task_store(kind, incoming):
    tasks = SimpleNamespace(dead_letter=lambda _: pytest.fail("Invalid message reached task store"))
    state = SimpleNamespace(document_jobs=tasks, search_tasks=tasks)
    with pytest.raises((ValueError, TypeError)):
        DeadLetterWorker(state, kind).process(incoming)


def test_document_dead_letter_rejects_legacy_schema_two():
    """The document kind requires the schema-3 notification, not the old protocol."""

    tasks = SimpleNamespace(dead_letter=lambda _: pytest.fail("Legacy message reached task store"))
    state = SimpleNamespace(document_jobs=tasks)
    legacy = {"schema": 2, "artifact_id": str(uuid4())}
    with pytest.raises(ValueError):
        DeadLetterWorker(state, "document").process(legacy)


def test_document_dead_letter_accepts_a_debezium_change_event():
    received = []
    tasks = SimpleNamespace(dead_letter=lambda message: received.append(message) or True)
    state = SimpleNamespace(document_jobs=tasks)
    artifact_id = str(uuid4())
    change = {"before": None, "op": "c", "after": {
        "artifact_id": artifact_id, "namespace": "manuals", "name": "manual.txt",
        "created_at": 0.0}}
    assert DeadLetterWorker(state, "document").process(change) is True
    assert received == [{"schema": 3, "artifact_id": artifact_id, "namespace": "manuals",
                         "name": "manual.txt", "created_at": 0.0}]


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
    state = SimpleNamespace(document_jobs=tasks, search_tasks=tasks, close=lambda: events.append("state_close"))
    monkeypatch.setattr(startup, "load_service_config", lambda: events.append("config"))
    monkeypatch.setattr(StateStores, "create", lambda _: state)
    monkeypatch.setattr(module.threading, "Event", lambda: stop)
    monkeypatch.setattr(module.signal, "signal", lambda *_: None)
    monkeypatch.setenv("RAG_ROCKETMQ_ENDPOINT", "unused:8081")

    class Transport:
        def __init__(self, settings):
            assert settings.topic.startswith("%DLQ%")

        def consume_once(self, callback):
            assert callback(payload(kind)) is True
            stop.set()

        def close(self):
            events.append("transport_close")

    from rag_mcp.state import search_queue

    monkeypatch.setattr(module, "RocketMQDocumentQueue", Transport)
    monkeypatch.setattr(search_queue, "RocketMQSearchQueue", Transport)
    assert module.main(["--kind", kind]) == 0
    assert events == ["config", "transport_close", "state_close"]


@pytest.mark.parametrize("kind", ["document", "search"])
def test_nonterminal_handler_result_never_eligible_for_ack(kind):
    tasks = SimpleNamespace(dead_letter=lambda _: False)
    state = SimpleNamespace(document_jobs=tasks, search_tasks=tasks)
    assert DeadLetterWorker(state, kind).process(payload(kind)) is False
