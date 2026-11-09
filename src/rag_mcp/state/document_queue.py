"""Notification-only RocketMQ transport plus the transactional outbox bridge.

Two bridges turn one committed MySQL outbox row into a broker notification:

* ``RAG_OUTBOX_BRIDGE=debezium`` -- Debezium tails the binlog of
  ``document_job_outbox`` and publishes each INSERT. The application never
  talks to the broker on the submission path, so a broker outage cannot lose
  or block an accepted upload.
* ``RAG_OUTBOX_BRIDGE=relay`` (default) -- an in-process relay publishes
  pending rows and marks them ``PUBLISHED``. Used for any deployment without
  a CDC pipeline.

Either way the message carries identifiers only. File bytes stay in MySQL and
the worker reads them back by ``artifact_id``.
"""

from __future__ import annotations

import json
import logging
import os
import threading
import time
from dataclasses import replace

from .cache_queue import RocketMQCacheQueue, RocketMQSettings
from .document_jobs import document_job_envelope, validate_document_envelope

logger = logging.getLogger(__name__)

BRIDGE_MODES = ("relay", "debezium")


def document_queue_settings():
    settings = replace(
        RocketMQSettings.from_env(),
        topic=os.getenv("RAG_DOCUMENT_TOPIC", "praxis_document_ingest").strip(),
        group=os.getenv("RAG_DOCUMENT_GROUP", "praxis_document_ingest_workers").strip(),
    )
    if not settings.topic or not settings.group:
        raise ValueError("Document topic and consumer group must not be empty")
    cache = RocketMQSettings.from_env()
    if settings.topic == cache.topic or settings.group == cache.group:
        raise ValueError("Document topic/group must be separate from cache refresh")
    return settings


def document_bridge_mode():
    value = os.getenv("RAG_OUTBOX_BRIDGE", "relay").strip().lower()
    if value not in BRIDGE_MODES:
        raise ValueError("RAG_OUTBOX_BRIDGE must be relay or debezium")
    return value


class RocketMQDocumentQueue(RocketMQCacheQueue):
    message_key = "artifact_id"
    invisible_seconds = 60
    renew_interval_seconds = 20
    renew_max_attempts = 3
    renew_retry_seconds = 1
    validate_payload = staticmethod(validate_document_envelope)

    def _configuration(self):
        import rocketmq  # noqa: F401 - SDK import installs its own logging handlers

        # SDK 5.1.2 logs raw RPC exceptions internally, including server text.
        # Only the safe adapter categories are allowed into application logs.
        logging.getLogger("rocketmq-python-client").disabled = True
        return super()._configuration()

    def _start_consumer(self):
        if self._consumer_factory is None:
            def create_consumer():
                from rocketmq import FilterExpression, SimpleConsumer

                class PhysicalTopicConsumer(SimpleConsumer):
                    def _receive_req(self, topic, *args, **kwargs):
                        request = super()._receive_req(topic, *args, **kwargs)
                        # 5.3.3 routes split ns%topic in the response, while
                        # ReceiveMessage still resolves the physical name only.
                        request.message_queue.topic.name = topic
                        return request

                return PhysicalTopicConsumer(
                    self._configuration(), self.settings.group,
                    {self.settings.topic: FilterExpression("*")}, await_duration=5,
                    tls_enable=self.settings.tls)

            self._consumer_factory = create_consumer
        return super()._start_consumer()

    @staticmethod
    def _failure_category(exc):
        if isinstance(exc, TimeoutError):
            return "timeout"
        if isinstance(exc, OSError):
            return "connection"
        return "sdk_error"

    def close(self):
        error = None
        for name in ("_consumer", "_producer"):
            client = getattr(self, name)
            setattr(self, name, None)
            if client is not None:
                try:
                    client.shutdown()
                except Exception as exc:  # noqa: BLE001 - attempt both client shutdowns
                    logger.warning("Message client shutdown failed category=%s", self._failure_category(exc))
                    error = error or exc
        if error is not None:
            raise error

    def send(self, payload):
        """Publish one notification built from a job or outbox row.

        Accepts either a stored job/outbox row or an already-built schema-3
        envelope; only the identifier fields ever reach the broker.
        """

        payload = document_job_envelope(payload)
        self._send_payload(payload, payload["artifact_id"])

    def _send_payload(self, payload, key):
        from rocketmq import Message

        message = Message()
        message.topic = self.settings.topic
        message.keys = key
        message.body = json.dumps(payload, separators=(",", ":"), allow_nan=False).encode()
        with self._producer_lock:
            self._start_producer().send(message)

    def consume_once(self, callback):
        return self._consume_payload_once(callback, self.validate_payload)

    def _consume_payload_once(self, callback, validator):
        with self._consumer_lock:
            consumer = self._start_consumer()
        for message in consumer.receive(1, self.invisible_seconds):
            # Broker 5.3.3 strips namespace prefixes in responses, but ACK/renew
            # address Resource.name. This consumer has exactly one subscription.
            message.topic = self.settings.topic
            try:
                payload = validator(json.loads(message.body))
            except (ValueError, TypeError):
                logger.warning("Ignoring malformed document message; broker retry/DLQ applies")
                continue
            stop = threading.Event()

            def renew(stop=stop, message=message):
                while not stop.wait(self.renew_interval_seconds):
                    for attempt in range(max(1, self.renew_max_attempts)):
                        try:
                            consumer.change_invisible_duration(message, self.invisible_seconds)
                            break
                        except Exception as exc:  # noqa: BLE001 - SDK errors must not undo committed work
                            logger.warning("Message visibility renewal failed category=%s attempt=%d",
                                           self._failure_category(exc), attempt + 1)
                            if attempt + 1 >= self.renew_max_attempts:
                                return
                            if stop.wait(self.renew_retry_seconds):
                                return

            renewer = threading.Thread(target=renew, name="document-message-lease", daemon=True)
            renewer.start()
            try:
                complete = callback(payload)
            finally:
                stop.set()
                renewer.join()
            if complete is True:
                try:
                    consumer.ack(message)
                except Exception as exc:
                    logger.warning("Message acknowledgement failed category=%s", self._failure_category(exc))
                    raise


class DocumentOutboxRelay:
    """Publish committed outbox rows when no external CDC bridge is configured."""

    def __init__(self, state, transport, *, poll_seconds=2.0):
        self.state = state
        self.jobs = state.document_jobs
        self._transport = transport
        self.poll_seconds = poll_seconds
        self._stop = threading.Event()
        self._thread = None

    @property
    def transport(self):
        # Resolved lazily: the dispatcher may swap its transport after construction.
        return self._transport() if callable(self._transport) else self._transport

    def publish_one(self, event_id):
        if self.transport is None:
            return False
        event = self.jobs.outbox_event(event_id)
        if event is None or event["status"] != "PENDING":
            return False
        try:
            self.transport.send(document_job_envelope(event))
        except Exception as exc:  # noqa: BLE001 - the row stays pending for the next pass
            logger.warning("Document outbox publish pending event_id=%s error_type=%s",
                           event_id, type(exc).__name__)
            self.jobs.mark_published(event_id, error=type(exc).__name__)
            return False
        self.jobs.mark_published(event_id)
        return True

    def publish_pending(self, limit=50):
        if self.transport is None:
            return 0
        published = 0
        for event in self.jobs.pending_outbox(limit=limit):
            if self.publish_one(event["id"]):
                published += 1
        return published

    def start(self):
        if self._thread is not None:
            return
        self._stop.clear()

        def run():
            while not self._stop.wait(self.poll_seconds):
                try:
                    self.publish_pending()
                except Exception as exc:  # noqa: BLE001 - never kill the relay thread
                    logger.warning("Document outbox relay pass failed error_type=%s", type(exc).__name__)

        self._thread = threading.Thread(target=run, name="document-outbox-relay", daemon=True)
        self._thread.start()

    def stop(self):
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=15)
            if not self._thread.is_alive():
                self._thread = None


class DocumentJobDispatcher:
    """Accept an upload by committing MySQL; delivery is a separate concern."""

    def __init__(self, state, transport=None):
        self.state = state
        self.jobs = state.document_jobs
        settings = document_queue_settings()
        self.transport = transport if transport is not None else (
            RocketMQDocumentQueue(settings) if settings.endpoint else None)
        self.mode = document_bridge_mode()
        self.relay = DocumentOutboxRelay(state, lambda: self.transport) if self.mode == "relay" else None
        if self.mode == "relay" and self.transport is None:
            logger.error("Document outbox relay has no broker; uploads stay queued until one is configured")

    def submit(self, namespace, name, content, idempotency_key=None):
        # The MySQL transaction is the durable acceptance point. Nothing below
        # may turn a committed job into a client-visible failure.
        job = self.jobs.submit(namespace, name, content, idempotency_key)
        if self.relay is not None and not job.get("reused"):
            try:
                self.relay.publish_one(job["event_id"])
            except Exception as exc:  # noqa: BLE001 - delivery retries from the outbox
                logger.warning("Document notification deferred error_type=%s", type(exc).__name__)
        return {"artifact_id": job["artifact_id"]}

    def run_once(self):
        return 0

    def start(self):
        if self.relay is not None:
            self.relay.start()

    def stop(self):
        if self.relay is not None:
            self.relay.stop()

    def close(self):
        self.stop()
        if self.transport is not None:
            self.transport.close()
