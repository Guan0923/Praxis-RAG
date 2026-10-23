"""Dedicated RocketMQ document transport and lightweight durable outbox relay."""

from __future__ import annotations

import json
import logging
import os
import threading
from dataclasses import replace

from .cache_queue import RocketMQCacheQueue, RocketMQSettings
from .document_tasks import validate_artifact_id

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
    invisible_seconds = 60
    renew_interval_seconds = 20

    def send(self, artifact_id):
        validate_artifact_id(artifact_id)
        from rocketmq import Message

        message = Message()
        message.topic = self.settings.topic
        message.keys = artifact_id
        message.body = json.dumps({"artifact_id": artifact_id}, separators=(",", ":")).encode()
        with self._producer_lock:
            self._start_producer().send(message)

    def consume_once(self, callback):
        with self._consumer_lock:
            consumer = self._start_consumer()
        for message in consumer.receive(1, self.invisible_seconds):
            try:
                payload = json.loads(message.body)
                artifact_id = validate_artifact_id(payload.get("artifact_id") if isinstance(payload, dict) else None)
            except (ValueError, TypeError):
                logger.warning("Ignoring malformed document message; broker retry/DLQ applies")
                continue
            stop = threading.Event()
            renewal_failed = threading.Event()

            def renew(stop=stop, message=message, renewal_failed=renewal_failed):
                while not stop.wait(self.renew_interval_seconds):
                    try:
                        consumer.change_invisible_duration(message, self.invisible_seconds)
                    except Exception:  # noqa: BLE001 - failed visibility renewal forbids acknowledgement
                        renewal_failed.set()
                        return

            renewer = threading.Thread(target=renew, name="document-message-lease", daemon=True)
            renewer.start()
            try:
                complete = callback(artifact_id)
            finally:
                stop.set()
                renewer.join()
            if complete and not renewal_failed.is_set():
                consumer.ack(message)


class DocumentTaskDispatcher:
    """No inference in the API process; failures leave the SQL outbox pending."""

    def __init__(self, state, transport=None):
        self.tasks = state.document_tasks
        settings = document_queue_settings()
        self.transport = transport or (RocketMQDocumentQueue(settings) if settings.endpoint else None)
        self._stop = threading.Event()
        self._thread = None

    def submit(self, namespace, name, content, idempotency_key=None):
        if self.transport is None:
            raise RuntimeError("Configure RAG_ROCKETMQ_ENDPOINT before adding documents")
        artifact_id = self.tasks.submit(namespace, name, content, idempotency_key)
        self.tasks.dispatch(artifact_id, self.transport)
        return {"artifact_id": artifact_id}

    def run_once(self):
        self.tasks.redrive_expired()
        return self.tasks.dispatch_pending(self.transport)

    def start(self):
        if self.transport is None or self._thread is not None:
            return
        self._stop.clear()

        def run():
            while not self._stop.is_set():
                try:
                    self.run_once()
                except Exception as exc:  # noqa: BLE001 - SQL outbox remains authoritative for retries
                    logger.warning("Document dispatch pending (%s)", type(exc).__name__)
                self._stop.wait(2)

        self._thread = threading.Thread(target=run, name="document-outbox", daemon=True)
        self._thread.start()

    def stop(self):
        self._stop.set()
        if self._thread is not None:
            self._thread.join()
            self._thread = None

    def close(self):
        self.stop()
        if self.transport is not None:
            self.transport.close()
