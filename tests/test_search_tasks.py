"""Full-request admission and Redis-only queued retrieval contracts."""

import copy
import json
import threading
from concurrent.futures import ThreadPoolExecutor
from types import SimpleNamespace
from unittest.mock import Mock
from uuid import uuid4

import pytest
from redis_stub import MemoryRedis

from rag_mcp.service_knowledge import KnowledgeError, RagServiceRuntime
from rag_mcp.state.cache import RedisStateCache, StateCacheSettings
from rag_mcp.state.result_snapshots import (
    ResultSnapshots,
    SnapshotSettings,
    SnapshotUnavailable,
)
from rag_mcp.state.search_queue import (
    RocketMQSearchQueue,
    SearchTaskDispatcher,
    search_queue_settings,
)
from rag_mcp.state.search_tasks import (
    SearchTasks,
    SearchTaskSettings,
    validate_search_envelope,
)


class Queue:
    def __init__(self):
        self.sent = []
        self.broken = False

    def send(self, envelope):
        if self.broken:
            raise ConnectionError("private broker details")
        self.sent.append(copy.deepcopy(envelope))

    def close(self):
        pass


@pytest.fixture
def service(monkeypatch):
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    client = MemoryRedis()
    engine = Mock()
    engine.begin.side_effect = AssertionError("Search must not access SQL")
    engine.connect.side_effect = AssertionError("Search must not access SQL")
    state = SimpleNamespace(engine=engine, cache_sync=SimpleNamespace(
        backend=RedisStateCache(StateCacheSettings(prefix="search-test"), client=client)))
    state.results = ResultSnapshots(state)
    state.search_tasks = SearchTasks(state)
    queue = Queue()
    state.search_dispatcher = SearchTaskDispatcher(state, queue)
    yield state, queue
    engine.begin.assert_not_called()
    engine.connect.assert_not_called()


def submit(service):
    state, queue = service
    result_id = state.search_dispatcher.submit("manuals", {"query": "private manual"})["result_id"]
    assert queue.sent[-1]["result_id"] == result_id
    return queue.sent[-1]


def complete(state, envelope, text="evidence"):
    task = state.search_tasks.claim(envelope)
    assert task is not None
    state.search_tasks.complete(task, {"results": [{"text": text}]})
    return task


def test_admission_sends_full_envelope_and_redis_retains_no_invocation(service):
    state, queue = service
    envelope = submit(service)
    assert envelope["schema"] == 2 and envelope["arguments"] == {"query": "private manual"}
    record = state.search_tasks.transient.get("search-result", envelope["result_id"])
    assert "private manual" not in json.dumps(record) and "arguments" not in record
    assert state.search_tasks.get("manuals", envelope["result_id"])["status"] == "queued"
    assert state.search_dispatcher.run_once() == 0
    state.search_dispatcher.start()
    state.search_dispatcher.stop()
    assert len(queue.sent) == 1
    complete(state, envelope)
    result = state.search_tasks.get("manuals", envelope["result_id"])
    assert result["result_id"] == envelope["result_id"] and result["results"] == [{"text": "evidence"}]


def test_broker_failure_never_reports_success_or_leaves_active_job(service):
    state, queue = service
    queue.broken = True
    with pytest.raises(RuntimeError, match="acceptance") as error:
        state.search_dispatcher.submit("manuals", {"query": "private"})
    assert "private" not in str(error.value)
    client = state.cache_sync.backend._connection()
    assert all(json.loads(value)["status"] == "failed" for value in client.values.values())
    assert queue.sent == []


def test_missing_broker_rejects_before_creating_state(service):
    state, queue = service
    state.search_dispatcher.transport = None
    with pytest.raises(RuntimeError, match="broker"):
        state.search_dispatcher.submit("manuals", {"query": "manual"})
    assert state.cache_sync.backend._connection().values == {}
    assert queue.sent == []


@pytest.mark.parametrize("disabled", [False, True])
def test_redis_unavailable_rejects_submission_before_broker_send(service, disabled):
    state, queue = service
    if disabled:
        state.cache_sync.backend = RedisStateCache(StateCacheSettings(enabled=False))
    else:
        state.cache_sync.backend._connection().broken = True
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = state
    with pytest.raises(KnowledgeError, match="not confirmed"):
        runtime.submit_search("manuals", {"query": "manual"})
    assert queue.sent == []


def test_redis_failure_after_send_is_not_successful_acceptance(service):
    state, queue = service
    send = queue.send

    def break_after_send(envelope):
        send(envelope)
        state.cache_sync.backend._connection().broken = True

    queue.send = break_after_send
    with pytest.raises(RuntimeError, match="not confirmed"):
        state.search_dispatcher.submit("manuals", {"query": "manual"})
    assert len(queue.sent) == 1


def test_wrong_namespace_missing_and_expired_jobs_never_execute(service):
    state, _queue = service
    envelope = submit(service)
    assert state.search_tasks.get("other", envelope["result_id"]) is None
    assert state.search_tasks.get("manuals", str(uuid4())) is None
    assert state.search_tasks.claim({**envelope, "namespace": "other"}) is None
    state.search_tasks.clock = lambda: envelope["deadline"]
    assert state.search_tasks.claim(envelope) is None
    assert state.search_tasks.terminal(envelope)
    with pytest.raises(SnapshotUnavailable, match="timed out"):
        state.search_tasks.get("manuals", envelope["result_id"])


def test_retry_and_dead_letter_are_transient_and_safe(service):
    state, _queue = service
    envelope = submit(service)
    task = state.search_tasks.claim(envelope)
    state.search_tasks.fail(task, error="private credential")
    assert state.search_tasks.get("manuals", envelope["result_id"])["stage"] == "retry_wait"
    assert not state.search_tasks.terminal(envelope)
    assert state.search_tasks.dead_letter(envelope["result_id"])
    with pytest.raises(SnapshotUnavailable, match="failed"):
        state.search_tasks.get("manuals", envelope["result_id"])
    assert "private credential" not in str(state.cache_sync.backend._connection().values)


def test_duplicate_completion_is_atomic_and_first_terminal_wins(service):
    state, _queue = service
    envelope = submit(service)
    first = state.search_tasks.claim(envelope)
    second = state.search_tasks.claim(envelope)
    barrier = threading.Barrier(2)

    def finish(task, text):
        barrier.wait(timeout=5)
        return state.search_tasks.complete(task, {"results": [{"text": text}]})

    with ThreadPoolExecutor(max_workers=2) as pool:
        futures = [pool.submit(finish, task, text) for task, text in ((first, "first"), (second, "second"))]
        [future.result(timeout=5) for future in futures]
    saved = state.search_tasks.get("manuals", envelope["result_id"])
    state.search_tasks.fail(second, retry=False)
    state.search_tasks.complete(second, {"results": [{"text": "late"}]})
    assert state.search_tasks.get("manuals", envelope["result_id"]) == saved
    assert state.search_tasks.claim(envelope) is None


def test_deadline_rechecked_during_atomic_publication(service, monkeypatch):
    state, _queue = service
    envelope = submit(service)
    task = state.search_tasks.claim(envelope)
    prepare = state.results.prepare

    def cross_deadline(*args, **kwargs):
        value = prepare(*args, **kwargs)
        state.search_tasks.clock = lambda: envelope["deadline"]
        return value

    monkeypatch.setattr(state.results, "prepare", cross_deadline)
    assert state.search_tasks.complete(task, {"results": []}) is None
    row = state.search_tasks.transient.get("search-result", envelope["result_id"])
    assert row["status"] == "timed_out" and "payload" not in row


def test_result_loss_or_redis_failure_never_falls_back_to_sql(service):
    state, _queue = service
    envelope = submit(service)
    complete(state, envelope)
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = state
    state.cache_sync.backend._connection().broken = True
    with pytest.raises(SnapshotUnavailable):
        runtime.get_search_result("manuals", envelope["result_id"])
    state.cache_sync.backend._connection().broken = False
    state.search_tasks.transient.delete("search-result", envelope["result_id"])
    with pytest.raises(SnapshotUnavailable):
        runtime.get_search_result("manuals", envelope["result_id"])


@pytest.mark.parametrize("change", [
    {"schema": 1}, {"schema": True}, {"result_id": "not-uuid"}, {"namespace": ""},
    {"namespace": "x" * 65}, {"arguments": []}, {"arguments": {"query": "x" * (1024 * 1024)}},
    {"created_at": float("nan")}, {"deadline": float("inf")}, {"expires_at": 0},
])
def test_envelope_validation_is_bounded(service, change):
    envelope = submit(service)
    with pytest.raises(ValueError, match="envelope"):
        validate_search_envelope({**envelope, **change})


def test_transport_overrides_use_full_payload_helpers(service, monkeypatch):
    envelope = submit(service)
    queue = RocketMQSearchQueue(search_queue_settings())
    send = Mock()
    consume = Mock()
    monkeypatch.setattr(queue, "_send_payload", send)
    monkeypatch.setattr(queue, "_consume_payload_once", consume)
    queue.send(envelope)
    send.assert_called_once_with(envelope, envelope["result_id"])
    callback = Mock()
    queue.consume_once(callback)
    consume.assert_called_once_with(callback, validate_search_envelope)
    with pytest.raises(ValueError):
        queue.send({"result_id": envelope["result_id"]})
    with pytest.raises(ValueError):
        queue.send(envelope["result_id"])


@pytest.mark.parametrize("value", [0, -1, True, float("nan"), float("inf")])
def test_invalid_timeout(value):
    with pytest.raises(ValueError):
        SearchTaskSettings(timeout_seconds=value)

@pytest.mark.parametrize("terminal", [False, True])
def test_real_transport_callback_contract_acks_only_after_result_bytes(service, terminal):
    state, _queue = service
    envelope = submit(service)
    message = SimpleNamespace(body=json.dumps(envelope).encode(), topic="broker-topic")
    acknowledgements = []

    def ack(received):
        assert received is message
        if terminal:
            assert state.results.get("manuals", envelope["result_id"])["results"] == [{"text": "ready"}]
        acknowledgements.append(received)

    consumer = SimpleNamespace(receive=lambda *_args: [message], ack=ack,
                               startup=lambda: None, shutdown=lambda: None)
    transport = RocketMQSearchQueue(search_queue_settings(), consumer_factory=lambda: consumer)

    def process(received):
        assert received == envelope
        task = state.search_tasks.claim(received)
        if terminal:
            state.search_tasks.complete(task, {"results": [{"text": "ready"}]})
        else:
            state.search_tasks.fail(task, retry=True)
        return state.search_tasks.terminal(received)

    try:
        transport.consume_once(process)
        assert len(acknowledgements) == int(terminal)
        assert message.topic == transport.settings.topic
    finally:
        transport.close()


def test_transport_rejects_legacy_uuid_and_oversized_messages_without_execute(service):
    envelope = submit(service)
    messages = [SimpleNamespace(body=json.dumps(payload).encode(), topic="topic") for payload in (
        {"result_id": envelope["result_id"]}, envelope["result_id"],
        {**envelope, "arguments": {"query": "x" * (1024 * 1024)}})]
    consumer = SimpleNamespace(receive=lambda *_args: messages, ack=Mock(),
                               startup=lambda: None, shutdown=lambda: None)
    callback = Mock()
    transport = RocketMQSearchQueue(search_queue_settings(), consumer_factory=lambda: consumer)
    try:
        transport.consume_once(callback)
        callback.assert_not_called()
        consumer.ack.assert_not_called()
    finally:
        transport.close()


def test_safe_admission_and_terminal_logging_excludes_query(service, caplog):
    caplog.set_level("INFO")
    state, _queue = service
    envelope = submit(service)
    complete(state, envelope)
    assert envelope["result_id"] in caplog.text
    assert "admission confirmed" in caplog.text and "succeeded" in caplog.text
    assert "private manual" not in caplog.text and "evidence" not in caplog.text


def test_ttl_caps_execution_deadline_and_expiry_never_recreates_evicted_work(service):
    state, _queue = service
    state.results.settings = SnapshotSettings(ttl_seconds=2)
    state.search_tasks.settings = SearchTaskSettings(timeout_seconds=100)
    envelope = submit(service)
    assert envelope["deadline"] == envelope["expires_at"]
    state.search_tasks.transient.delete("search-result", envelope["result_id"])
    assert state.search_tasks.claim(envelope) is None
    assert state.search_tasks.terminal(envelope)
    assert state.search_tasks.transient.get("search-result", envelope["result_id"]) is None

def test_timeout_environment_and_result_ttl_are_independent(monkeypatch):
    monkeypatch.setenv("RAG_SEARCH_TIMEOUT_SECONDS", "30")
    monkeypatch.setenv("RAG_RESULT_TTL_SECONDS", "75")
    assert SearchTaskSettings.from_env() == SearchTaskSettings(timeout_seconds=30)
    assert SnapshotSettings.from_env().ttl_seconds == 75
    assert set(SearchTaskSettings.__dataclass_fields__) == {"timeout_seconds"}

@pytest.mark.parametrize("field,value", [("arguments", {"query": "injected"}), ("created_at", 1)])
def test_conflicting_envelope_never_mutates_admitted_request(service, field, value):
    state, _queue = service
    envelope = submit(service)
    store = state.search_tasks.transient
    before = store.get("search-result", envelope["result_id"])
    assert state.search_tasks.claim({**envelope, field: value}) is None
    assert store.get("search-result", envelope["result_id"]) == before
    assert state.search_tasks.claim(envelope)["arguments"] == envelope["arguments"]


def test_uuid_collision_cannot_replace_another_tenant(service, monkeypatch):
    import rag_mcp.state.search_tasks as module

    state, queue = service
    envelope = submit(service)
    before = state.search_tasks.transient.get("search-result", envelope["result_id"])
    monkeypatch.setattr(module, "uuid4", lambda: envelope["result_id"])
    with pytest.raises(RuntimeError, match="admission"):
        state.search_dispatcher.submit("other", {"query": "injected"})
    assert len(queue.sent) == 1
    assert state.search_tasks.transient.get("search-result", envelope["result_id"]) == before


def test_unicode_wire_expansion_is_bounded_before_admission(service):
    state, queue = service
    # Raw UTF-8 fits 1 MiB; MQ JSON ASCII escaping would exceed the envelope bound.
    with pytest.raises(ValueError, match="envelope"):
        state.search_dispatcher.submit("manuals", {"query": "中" * 200_000})
    assert queue.sent == []
    assert state.cache_sync.backend._connection().values == {}


def test_uncertain_broker_send_cannot_execute_failed_attempt_on_redelivery(service):
    from rag_mcp.search_worker import SearchWorker

    state, queue = service
    send = queue.send

    def lose_receipt(envelope):
        send(envelope)
        raise TimeoutError("private receipt lost")

    queue.send = lose_receipt
    with pytest.raises(RuntimeError, match="not confirmed"):
        state.search_dispatcher.submit("manuals", {"query": "manual"})
    original = queue.sent[0]
    worker = SearchWorker(SimpleNamespace(state=state), executor=lambda *_args: pytest.fail("failed admission"))
    assert worker.process(original)
    queue.send = send
    retried_id = state.search_dispatcher.submit("manuals", {"query": "manual"})["result_id"]
    # Search has no client idempotency key: each new submission is a new invocation.
    assert retried_id != original["result_id"]


@pytest.mark.parametrize("missing", [False, True])
def test_redelivery_missing_or_expired_state_is_acknowledged_without_execution(service, missing):
    from rag_mcp.search_worker import SearchWorker

    state, _queue = service
    envelope = submit(service)
    if missing:
        state.search_tasks.transient.delete("search-result", envelope["result_id"])
    else:
        state.search_tasks.clock = lambda: envelope["expires_at"]
    worker = SearchWorker(SimpleNamespace(state=state), executor=lambda *_args: pytest.fail("expired work"))
    assert worker.process(envelope)
    row = state.search_tasks.transient.get("search-result", envelope["result_id"])
    assert row is None if missing else row["status"] == "timed_out"


def test_transport_redis_read_failure_never_acknowledges(service):
    from rag_mcp.search_worker import SearchWorker

    state, _queue = service
    envelope = submit(service)
    state.cache_sync.backend._connection().broken = True
    message = SimpleNamespace(body=json.dumps(envelope).encode(), topic="topic")
    consumer = SimpleNamespace(receive=lambda *_args: [message], ack=Mock(),
                               startup=lambda: None, shutdown=lambda: None)
    worker = SearchWorker(SimpleNamespace(state=state), executor=lambda *_args: pytest.fail("Redis unavailable"))
    transport = RocketMQSearchQueue(search_queue_settings(), consumer_factory=lambda: consumer)
    try:
        with pytest.raises(RuntimeError, match="Redis"):
            transport.consume_once(worker.process)
        consumer.ack.assert_not_called()
    finally:
        transport.close()

@pytest.mark.parametrize("change", [{"namespace": "other"}, {"arguments": {"query": "injected"}}])
def test_conflicting_dead_letter_envelope_cannot_fail_admitted_job(service, change):
    state, _queue = service
    envelope = submit(service)
    before = state.search_tasks.transient.get("search-result", envelope["result_id"])
    assert state.search_tasks.dead_letter({**envelope, **change})
    assert state.search_tasks.transient.get("search-result", envelope["result_id"]) == before
    complete(state, envelope)
    assert state.search_tasks.dead_letter(envelope)
    assert state.search_tasks.get("manuals", envelope["result_id"])["results"] == [{"text": "evidence"}]
