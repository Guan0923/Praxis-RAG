"""Dedicated document protocol, broker confirmation and ACK boundaries."""

import json
from types import SimpleNamespace
from uuid import uuid4

import pytest

from rag_mcp.state.cache_queue import RocketMQSettings
from rag_mcp.state.document_queue import RocketMQDocumentQueue, document_queue_settings


class Producer:
    def __init__(self):
        self.messages = []
        self.closed = False

    def startup(self):
        pass

    def send(self, message):
        self.messages.append(message)

    def shutdown(self):
        self.closed = True


class Consumer(Producer):
    def __init__(self, messages):
        super().__init__()
        self.messages = messages
        self.acks = []
        self.renewals = []

    def receive(self, count, duration):
        assert count == 1 and duration == 60
        return self.messages

    def ack(self, message):
        self.acks.append(message)

    def change_invisible_duration(self, message, duration):
        self.renewals.append((message, duration))


def test_document_protocol_uses_canonical_uuid_and_dedicated_topic():
    pytest.importorskip("rocketmq")
    producer = Producer()
    queue = RocketMQDocumentQueue(RocketMQSettings(topic="documents"), producer_factory=lambda: producer)
    try:
        artifact_id = str(uuid4())
        queue.send(artifact_id)
        message = producer.messages[0]
        assert message.topic == "documents"
        assert json.loads(message.body) == {"artifact_id": artifact_id}
        with pytest.raises(ValueError):
            queue.send(uuid4().hex)
    finally:
        queue.close()
    assert producer.closed


def test_consumer_ack_only_for_terminal_tasks():
    artifact_id = str(uuid4())
    message = SimpleNamespace(body=json.dumps({"artifact_id": artifact_id}).encode())
    consumer = Consumer([message])
    queue = RocketMQDocumentQueue(RocketMQSettings(), consumer_factory=lambda: consumer)
    try:
        queue.consume_once(lambda incoming: incoming != artifact_id)
        assert consumer.acks == []
        queue.consume_once(lambda incoming: incoming == artifact_id)
        assert consumer.acks == [message]
    finally:
        queue.close()


def test_callback_failure_and_poison_messages_are_not_acknowledged():
    consumer = Consumer([SimpleNamespace(body=b"not json"), SimpleNamespace(body=b'{"artifact_id":"bad"}')])
    queue = RocketMQDocumentQueue(RocketMQSettings(), consumer_factory=lambda: consumer)
    try:
        queue.consume_once(lambda _: pytest.fail("Poison message reached worker"))
        assert consumer.acks == []
        consumer.messages = [SimpleNamespace(body=json.dumps({"artifact_id": str(uuid4())}).encode())]

        def fail(_):
            raise OSError("database unavailable")

        with pytest.raises(OSError):
            queue.consume_once(fail)
        assert consumer.acks == []
    finally:
        queue.close()


def test_long_task_renews_visibility_before_ack():
    import threading

    message = SimpleNamespace(body=json.dumps({"artifact_id": str(uuid4())}).encode())
    consumer = Consumer([message])
    renewed = threading.Event()
    original = consumer.change_invisible_duration

    def renew(*args):
        original(*args)
        renewed.set()

    consumer.change_invisible_duration = renew
    queue = RocketMQDocumentQueue(RocketMQSettings(), consumer_factory=lambda: consumer)
    queue.renew_interval_seconds = 0.01
    try:
        def complete(_):
            assert renewed.wait(2)
            return True

        queue.consume_once(complete)
        assert consumer.renewals and consumer.acks == [message]
    finally:
        queue.close()


def test_visibility_renewal_failure_still_acks_committed_terminal(caplog):
    import threading

    message = SimpleNamespace(body=json.dumps({"artifact_id": str(uuid4())}).encode())
    consumer = Consumer([message])
    failed = threading.Event()

    def renew(*_):
        failed.set()
        raise OSError("secret-credential-value")

    consumer.change_invisible_duration = renew
    queue = RocketMQDocumentQueue(RocketMQSettings(), consumer_factory=lambda: consumer)
    queue.renew_interval_seconds = 0.01
    try:
        def complete(_):
            assert failed.wait(2)
            return True

        queue.consume_once(complete)
        assert consumer.acks == [message]
        assert "secret-credential-value" not in caplog.text
        assert "category=connection" in caplog.text
    finally:
        queue.close()


def test_cache_and_document_topics_and_groups_must_be_separate(monkeypatch):
    monkeypatch.setenv("RAG_DOCUMENT_TOPIC", "praxis_state_cache_refresh")
    with pytest.raises(ValueError, match="separate"):
        document_queue_settings()
    monkeypatch.setenv("RAG_DOCUMENT_TOPIC", "documents")
    monkeypatch.setenv("RAG_DOCUMENT_GROUP", "praxis_state_cache_refresh_workers")
    with pytest.raises(ValueError, match="separate"):
        document_queue_settings()


@pytest.mark.parametrize("terminal", [True, False, None])
def test_renewals_have_bounded_retries_without_vetoing_callback(terminal, caplog):
    import threading

    message = SimpleNamespace(body=json.dumps({"artifact_id": str(uuid4())}).encode())
    consumer = Consumer([message])
    exhausted = threading.Event()
    calls = []

    def renew(*_):
        calls.append(1)
        if len(calls) == 3:
            exhausted.set()
        raise RuntimeError("private broker credentials")

    consumer.change_invisible_duration = renew
    queue = RocketMQDocumentQueue(RocketMQSettings(), consumer_factory=lambda: consumer)
    queue.renew_interval_seconds = queue.renew_retry_seconds = 0.001
    try:
        def complete(_):
            assert exhausted.wait(2)
            return terminal

        queue.consume_once(complete)
        assert len(calls) == 3
        assert consumer.acks == ([message] if terminal is True else [])
        assert "private broker credentials" not in caplog.text
        assert caplog.text.count("visibility renewal failed") == 3
    finally:
        queue.close()


def test_failed_ack_propagates_after_terminal_commit(caplog):
    message = SimpleNamespace(body=json.dumps({"artifact_id": str(uuid4())}).encode())
    consumer = Consumer([message])
    committed = []

    def ack(_):
        assert committed == [True]
        raise TimeoutError("private ACK credentials")

    consumer.ack = ack
    queue = RocketMQDocumentQueue(RocketMQSettings(), consumer_factory=lambda: consumer)
    try:
        def complete(_):
            committed.append(True)
            return True

        with pytest.raises(TimeoutError):
            queue.consume_once(complete)
        assert committed == [True]
        assert "private ACK credentials" not in caplog.text
        assert "acknowledgement failed category=timeout" in caplog.text
    finally:
        queue.close()


def test_ack_and_renew_use_physical_subscription_topic():
    message = SimpleNamespace(topic="%DLQ%source", body=json.dumps({"artifact_id": str(uuid4())}).encode())
    consumer = Consumer([message])
    queue = RocketMQDocumentQueue(
        RocketMQSettings(topic="%DLQ%mq_namespace%source", namespace="mq_namespace"),
        consumer_factory=lambda: consumer)
    try:
        queue.consume_once(lambda _: True)
        assert consumer.acks[0].topic == "%DLQ%mq_namespace%source"
    finally:
        queue.close()


def test_close_attempts_both_clients_even_when_consumer_shutdown_fails(caplog):
    producer = Producer()
    consumer = Consumer([])

    def fail():
        raise OSError("private close credentials")

    consumer.shutdown = fail
    queue = RocketMQDocumentQueue(RocketMQSettings())
    queue._consumer, queue._producer = consumer, producer
    with pytest.raises(OSError):
        queue.close()
    assert producer.closed
    assert queue._consumer is queue._producer is None
    assert "private close credentials" not in caplog.text
    queue.close()


def test_sdk_receive_request_keeps_physical_topic_name(monkeypatch):
    pytest.importorskip("rocketmq")
    from rocketmq import SimpleConsumer
    from rocketmq.grpc_protocol import ReceiveMessageRequest

    captured = []
    monkeypatch.setattr(SimpleConsumer, "startup", lambda self: self._init_settings_event.set())
    monkeypatch.setattr(SimpleConsumer, "shutdown", lambda _: None)
    monkeypatch.setattr(SimpleConsumer, "_receive_req", lambda *_args, **_kwargs: ReceiveMessageRequest())
    queue = RocketMQDocumentQueue(RocketMQSettings(endpoint="127.0.0.1:8081", topic="%DLQ%ns%source"))
    try:
        consumer = queue._start_consumer()
        captured.append(consumer._receive_req(queue.settings.topic))
        assert captured[0].message_queue.topic.name == "%DLQ%ns%source"
    finally:
        queue.close()
