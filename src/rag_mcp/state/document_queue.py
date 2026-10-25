"""Bounded document payload transport with confirmed synchronous admission."""

from __future__ import annotations

import json
import logging
import os
import threading
from dataclasses import replace

from .cache_queue import RocketMQCacheQueue, RocketMQSettings
from .document_tasks import document_envelopes, validate_document_envelope

logger = logging.getLogger(__name__)


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


class RocketMQDocumentQueue(RocketMQCacheQueue):
    message_key = "artifact_id"
    invisible_seconds = 60
    renew_interval_seconds = 20
    renew_max_attempts = 3
    renew_retry_seconds = 1

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

    validate_payload = staticmethod(validate_document_envelope)

    def send(self, task):
        for payload in document_envelopes(task):
            self._send_payload(payload, task["artifact_id"])

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
                payload = json.loads(message.body)
                payload = validator(payload)
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


class DocumentTaskDispatcher:
    """Return an upload ID only after Redis and every MQ message are confirmed."""

    def __init__(self, state, transport=None):
        self.tasks = state.document_tasks
        settings = document_queue_settings()
        self.transport = transport or (RocketMQDocumentQueue(settings) if settings.endpoint else None)

    def submit(self, namespace, name, content, idempotency_key=None):
        if self.transport is None:
            raise RuntimeError("RocketMQ is required for document uploads")
        task = self.tasks.submit(namespace, name, content, idempotency_key)
        if not task.get("accepted"):
            try:
                self.transport.send(task)
                self.tasks.accepted(task)
            except Exception as exc:
                logger.warning("Document admission unconfirmed artifact_id=%s category=%s",
                               task["artifact_id"], type(exc).__name__)
                raise
        logger.info("Document admission confirmed artifact_id=%s", task["artifact_id"])
        return {"artifact_id": task["artifact_id"]}

    def run_once(self):
        return 0

    def start(self):
        """Compatibility hook; there is no SQL task relay."""

    def stop(self):
        """Compatibility hook; there is no background dispatcher."""

    def close(self):
        if self.transport is not None:
            self.transport.close()
