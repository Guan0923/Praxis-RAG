"""Optional RocketMQ 5 gRPC transport for retrying state-cache refresh events."""

from __future__ import annotations

import json
import os
import re
import threading
from dataclasses import dataclass, field


@dataclass(frozen=True)
class RocketMQSettings:
    endpoint: str | None = None
    topic: str = "praxis_state_cache_refresh"
    group: str = "praxis_state_cache_refresh_workers"
    namespace: str = ""
    access_key: str = field(default="", repr=False)
    secret_key: str = field(default="", repr=False)
    tls: bool = False

    @classmethod
    def from_env(cls):
        return cls(
            endpoint=os.getenv("RAG_ROCKETMQ_ENDPOINT", "").strip() or None,
            topic=os.getenv("RAG_ROCKETMQ_TOPIC", "praxis_state_cache_refresh").strip(),
            group=os.getenv("RAG_ROCKETMQ_GROUP", "praxis_state_cache_refresh_workers").strip(),
            namespace=os.getenv("RAG_ROCKETMQ_NAMESPACE", "").strip(),
            access_key=os.getenv("RAG_ROCKETMQ_ACCESS_KEY", ""),
            secret_key=os.getenv("RAG_ROCKETMQ_SECRET_KEY", ""),
            tls=os.getenv("RAG_ROCKETMQ_TLS", "0").strip().lower() in {"1", "true", "yes"},
        )


class RocketMQCacheQueue:
    """Lazily start the SDK; missing brokers never prevent MySQL writes.

    Unacknowledged deliveries become visible after the invisibility timeout.
    MySQL outbox remains the durable source even after broker DLQ retention.
    """
    def __init__(self, settings, *, producer_factory=None, consumer_factory=None):
        self.settings = settings
        self._producer = None
        self._consumer = None
        self._producer_factory = producer_factory
        self._consumer_factory = consumer_factory
        self._producer_lock = threading.Lock()
        self._consumer_lock = threading.Lock()

    def _configuration(self):
        try:
            from rocketmq import ClientConfiguration, Credentials
        except ImportError:
            raise RuntimeError("RocketMQ retries require praxis-rag[cache]") from None
        return ClientConfiguration(
            self.settings.endpoint, Credentials(self.settings.access_key, self.settings.secret_key),
            namespace=self.settings.namespace, request_timeout=3)

    def _start_producer(self):
        if self._producer is None:
            if self._producer_factory:
                producer = self._producer_factory()
            else:
                from rocketmq import Producer
                producer = Producer(self._configuration(), topics=(self.settings.topic,), tls_enable=self.settings.tls)
            try:
                producer.startup()
            except Exception:
                producer.shutdown()
                raise
            self._producer = producer
        return self._producer

    def _start_consumer(self):
        if self._consumer is None:
            if self._consumer_factory:
                consumer = self._consumer_factory()
            else:
                from rocketmq import FilterExpression, SimpleConsumer
                consumer = SimpleConsumer(self._configuration(), self.settings.group,
                    {self.settings.topic: FilterExpression("*")}, await_duration=5, tls_enable=self.settings.tls)
            try:
                consumer.startup()
                # SDK 5.1.2 returns before SimpleConsumer telemetry is accepted.
                # Keep the version-specific handshake workaround inside this adapter.
                ready = getattr(consumer, "_init_settings_event", None)
                if ready is not None and not ready.wait(timeout=8):
                    raise RuntimeError("RocketMQ consumer settings handshake timed out")
            except Exception:
                consumer.shutdown()
                raise
            self._consumer = consumer
        return self._consumer

    def send(self, event_id):
        if not re.fullmatch(r"[0-9a-f]{32}", event_id):
            raise ValueError("Invalid cache event id")
        from rocketmq import Message
        message = Message()
        message.topic = self.settings.topic
        message.keys = event_id
        message.body = json.dumps({"event_id": event_id}, separators=(",", ":")).encode("utf-8")
        with self._producer_lock:
            self._start_producer().send(message)

    def consume_once(self, callback):
        with self._consumer_lock:
            consumer = self._start_consumer()
        for message in consumer.receive(1, 30):
            try:
                payload = json.loads(message.body)
                event_id = payload.get("event_id") if isinstance(payload, dict) else None
                if not isinstance(event_id, str) or not re.fullmatch(r"[0-9a-f]{32}", event_id):
                    continue  # Poison messages retry/DLQ; never execute arbitrary input.
                if callback(event_id):
                    consumer.ack(message)
            except Exception:
                # Do not ACK a failed refresh; broker visibility provides retries.
                continue

    def close(self):
        for client in (self._consumer, self._producer):
            if client is not None:
                client.shutdown()
        self._consumer = self._producer = None
