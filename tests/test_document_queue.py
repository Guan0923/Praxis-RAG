"""Notification-only document protocol, broker confirmation and ACK boundaries.

The queue transports identifiers only; file bytes live in MySQL. These tests
pin the wire contract, the poison/DLQ boundary and the ACK rules.
"""

import json
import os
from types import SimpleNamespace
from uuid import uuid4

import pytest

from rag_mcp.state.cache_queue import RocketMQSettings
from rag_mcp.state.document_jobs import document_job_envelope
from rag_mcp.state.document_queue import RocketMQDocumentQueue, document_queue_settings

def _rocketmq_log_dir_usable():
    """The SDK configures a file handler under ~/logs at import time.

    Skip when that path is not writable (sandboxed shells, read-only CI homes)
    instead of failing with PermissionError from deep inside logging.config.
    """

    log_dir = os.path.join(os.path.expanduser("~"), "logs", "rocketmq_python")
    try:
        os.makedirs(log_dir, exist_ok=True)
        # The SDK opens the file itself, so probe the file rather than the directory.
        with open(os.path.join(log_dir, "rocketmq_client.log"), "a"):
            pass
        return True
    except OSError:
        return False


pytestmark = pytest.mark.skipif(not _rocketmq_log_dir_usable(),
                               reason="RocketMQ SDK cannot create ~/logs/rocketmq_python")


def job():
    return {"id": uuid4().hex, "artifact_id": str(uuid4()), "namespace": "manuals",
            "name": "manual.txt", "status": "PENDING", "created_at": 1000.0}


def valid_message(payload=None):
    envelope = document_job_envelope(payload or job())
    return SimpleNamespace(body=json.dumps(envelope).encode())


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
        incoming = job()
        queue.send(incoming)
        message = producer.messages[0]
        assert message.topic == "documents"
        # SDK 5.1.2 normalises Message.keys to a set.
        assert message.keys == {incoming["artifact_id"]}
        payload = json.loads(message.body)
        assert payload == document_job_envelope(incoming)
        assert set(payload) == {"schema", "artifact_id", "namespace", "name", "created_at"}
        assert "content" not in payload and "data" not in payload
        with pytest.raises((ValueError, TypeError)):
            queue.send(uuid4().hex)
        with pytest.raises(ValueError):
            queue.send({**incoming, "artifact_id": "not-a-uuid"})
    finally:
        queue.close()
    assert producer.closed


def test_consumer_ack_only_for_terminal_tasks():
    incoming = document_job_envelope(job())
    artifact_id = incoming["artifact_id"]
    message = SimpleNamespace(body=json.dumps(incoming).encode())
    consumer = Consumer([message])
    queue = RocketMQDocumentQueue(RocketMQSettings(), consumer_factory=lambda: consumer)
    try:
        queue.consume_once(lambda payload: payload["artifact_id"] != artifact_id)
        assert consumer.acks == []
        queue.consume_once(lambda payload: payload["artifact_id"] == artifact_id)
        assert consumer.acks == [message]
    finally:
        queue.close()


def test_callback_failure_and_poison_messages_are_not_acknowledged():
    consumer = Consumer([SimpleNamespace(body=b"not json"), SimpleNamespace(body=b'{"artifact_id":"bad"}')])
    queue = RocketMQDocumentQueue(RocketMQSettings(), consumer_factory=lambda: consumer)
    try:
        queue.consume_once(lambda _: pytest.fail("Poison message reached worker"))
        assert consumer.acks == []
        consumer.messages = [valid_message()]

        def fail(_):
            raise OSError("database unavailable")

        with pytest.raises(OSError):
            queue.consume_once(fail)
        assert consumer.acks == []
    finally:
        queue.close()


def test_debezium_change_event_reaches_the_worker_as_a_notification():
    incoming = document_job_envelope(job())
    change = {"before": None, "op": "c", "after": {
        **incoming, "id": uuid4().hex, "status": "PENDING"}}
    consumer = Consumer([SimpleNamespace(body=json.dumps(change).encode())])
    queue = RocketMQDocumentQueue(RocketMQSettings(), consumer_factory=lambda: consumer)
    seen = []
    try:
        queue.consume_once(lambda payload: seen.append(payload) or True)
        assert seen == [incoming]
        assert consumer.acks
    finally:
        queue.close()


def test_legacy_canonical_uuid_only_payload_is_rejected():
    consumer = Consumer([SimpleNamespace(body=json.dumps({"artifact_id": str(uuid4())}).encode())])
    queue = RocketMQDocumentQueue(RocketMQSettings(), consumer_factory=lambda: consumer)
    try:
        queue.consume_once(lambda _: pytest.fail("Legacy identifier-only payload reached worker"))
        assert consumer.acks == []
    finally:
        queue.close()


def test_long_task_renews_visibility_before_ack():
    import threading

    message = valid_message()
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

    message = valid_message()
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


def test_document_queue_settings_reject_blank_topic_or_group(monkeypatch):
    monkeypatch.setenv("RAG_DOCUMENT_TOPIC", " ")
    with pytest.raises(ValueError, match="must not be empty"):
        document_queue_settings()
    monkeypatch.setenv("RAG_DOCUMENT_TOPIC", "documents")
    monkeypatch.setenv("RAG_DOCUMENT_GROUP", "")
    with pytest.raises(ValueError, match="must not be empty"):
        document_queue_settings()


@pytest.mark.parametrize("terminal", [True, False, None])
def test_renewals_have_bounded_retries_without_vetoing_callback(terminal, caplog):
    import threading

    message = valid_message()
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
    message = valid_message()
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
    message = valid_message()
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


def test_maximum_upload_is_a_single_small_notification():
    """There is no 512 KiB chunking: 10 MiB and 1 byte both fit one message."""

    pytest.importorskip("rocketmq")
    producer = Producer()
    queue = RocketMQDocumentQueue(RocketMQSettings(topic="documents"), producer_factory=lambda: producer)
    try:
        queue.send(job())
        assert len(producer.messages) == 1
        message = producer.messages[0]
        assert len(message.body) < 1024
        payload = json.loads(message.body)
        assert "chunk_index" not in payload and "data" not in payload
        assert payload["schema"] == 3
    finally:
        queue.close()


def test_partial_send_failure_propagates_to_the_relay():
    pytest.importorskip("rocketmq")
    producer = Producer()

    def send(_message):
        raise TimeoutError("confirmation lost")

    producer.send = send
    queue = RocketMQDocumentQueue(RocketMQSettings(topic="documents"), producer_factory=lambda: producer)
    try:
        with pytest.raises(TimeoutError):
            queue.send(job())
    finally:
        queue.close()
