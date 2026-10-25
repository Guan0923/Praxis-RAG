"""DLQ handling records Redis outcomes without inference or SQL task access."""

import subprocess
import sys
from types import SimpleNamespace
from uuid import uuid4

import pytest

from rag_mcp.dead_letter_worker import DeadLetterWorker, dead_letter_queue_settings
from rag_mcp.state.store import StateStores
from rag_mcp.state.transient import TransientUnavailable


@pytest.mark.parametrize("kind", ["document", "search"])
@pytest.mark.parametrize("complete", [True, False, None])
def test_ack_eligibility_follows_task_outcome(kind, complete, caplog):
    calls = []

    def dead_letter(task_id):
        calls.append(task_id)
        return complete

    tasks = SimpleNamespace(dead_letter=dead_letter)
    state = SimpleNamespace(document_tasks=tasks, search_tasks=tasks)
    key = "artifact_id" if kind == "document" else "result_id"
    task_id = str(uuid4())
    payload = {"schema": 2, key: task_id}
    with caplog.at_level("INFO"):
        result = DeadLetterWorker(state, kind).process(payload)
    assert result is (complete is True)
    assert calls == [payload]
    if complete is True:
        assert task_id in caplog.text


@pytest.mark.parametrize("kind", ["document", "search"])
def test_redis_failure_propagates_without_ack_eligibility(kind):
    def fail(_):
        raise TransientUnavailable("Redis task write unavailable")

    tasks = SimpleNamespace(dead_letter=fail)
    state = SimpleNamespace(document_tasks=tasks, search_tasks=tasks)
    key = "artifact_id" if kind == "document" else "result_id"
    with pytest.raises(TransientUnavailable):
        DeadLetterWorker(state, kind).process({"schema": 2, key: str(uuid4())})


@pytest.mark.parametrize("kind", ["document", "search"])
@pytest.mark.parametrize("payload", [None, [], "legacy-id", {}, {"artifact_id": "bad", "result_id": "bad"}])
def test_invalid_payload_never_reaches_task_store(kind, payload):
    tasks = SimpleNamespace(dead_letter=lambda _: pytest.fail("Invalid message reached task store"))
    state = SimpleNamespace(document_tasks=tasks, search_tasks=tasks)
    with pytest.raises((ValueError, TypeError)):
        DeadLetterWorker(state, kind).process(payload)


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
            key = "artifact_id" if kind == "document" else "result_id"
            assert callback({"schema": 2, key: str(uuid4())}) is True
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
    state = SimpleNamespace(document_tasks=tasks, search_tasks=tasks)
    key = "artifact_id" if kind == "document" else "result_id"
    assert DeadLetterWorker(state, kind).process({"schema": 2, key: str(uuid4())}) is False
