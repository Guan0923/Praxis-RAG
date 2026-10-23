"""RocketMQ adapter contract: event IDs only and ACK only successful refreshes."""

import json
from types import SimpleNamespace

import pytest

from rag_mcp.state.cache_queue import RocketMQCacheQueue, RocketMQSettings


class Producer:
    def startup(self):
        self.started = True

    def send(self, message):
        self.message = message

    def shutdown(self):
        self.closed = True


class Consumer(Producer):
    def __init__(self):
        self.messages = []
        self.acked = []

    def receive(self, maximum, invisible):
        assert maximum == 1 and invisible == 30
        return self.messages

    def ack(self, message):
        self.acked.append(message)


def test_send_transmits_only_valid_event_id(monkeypatch):
    # No SDK/network requirement for unit tests.
    import sys
    monkeypatch.setitem(sys.modules, "rocketmq", SimpleNamespace(Message=SimpleNamespace))
    producer = Producer()
    queue = RocketMQCacheQueue(RocketMQSettings(endpoint="localhost:8081"), producer_factory=lambda: producer)
    queue.send("a" * 32)
    assert producer.message.topic == "praxis_state_cache_refresh"
    assert json.loads(producer.message.body) == {"event_id": "a" * 32}
    with pytest.raises(ValueError):
        queue.send("not an event")
    queue.close()
    assert producer.closed


def test_ack_only_successful_refresh_and_retry_errors():
    consumer = Consumer()
    consumer.messages = [SimpleNamespace(body=json.dumps({"event_id": key * 32}).encode()) for key in "abc"]
    consumer.messages += [SimpleNamespace(body=b"invalid"), SimpleNamespace(body=b'{"event_id":"bad"}')]
    queue = RocketMQCacheQueue(RocketMQSettings(endpoint="localhost:8081"), consumer_factory=lambda: consumer)
    def refresh(event_id):
        if event_id.startswith("c"):
            raise OSError("redis down")
        return event_id.startswith("a")
    queue.consume_once(refresh)
    assert consumer.acked == consumer.messages[:1]
    queue.close()
    assert consumer.closed


def test_sdk_missing_does_not_fail_configuration():
    queue = RocketMQCacheQueue(RocketMQSettings(endpoint="localhost:8081"))
    assert queue._consumer is None and queue._producer is None
