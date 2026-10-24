"""Dedicated UUID-only search transport and independent durable outbox relay."""

from __future__ import annotations

import logging
import os
import threading
from dataclasses import replace

from .cache_queue import RocketMQSettings
from .document_queue import RocketMQDocumentQueue, document_queue_settings

logger = logging.getLogger(__name__)


def search_queue_settings():
    cache = RocketMQSettings.from_env()
    document = document_queue_settings()
    settings = replace(
        cache,
        topic=os.getenv("RAG_SEARCH_TOPIC", "praxis_search").strip(),
        group=os.getenv("RAG_SEARCH_GROUP", "praxis_search_workers").strip(),
    )
    if not settings.topic or not settings.group:
        raise ValueError("Search topic and consumer group must not be empty")
    if settings.topic in (cache.topic, document.topic) or settings.group in (cache.group, document.group):
        raise ValueError("Search topic/group must be separate from cache and document queues")
    return settings


class RocketMQSearchQueue(RocketMQDocumentQueue):
    message_key = "result_id"


class SearchTaskDispatcher:
    """Admission does not connect to the broker or execute retrieval in the API."""

    def __init__(self, state, transport=None):
        self.tasks = state.search_tasks
        settings = search_queue_settings()
        self.transport = transport if transport is not None else (
            RocketMQSearchQueue(settings) if settings.endpoint else None)
        self._stop = threading.Event()
        self._wake = threading.Event()
        self._thread = None
        self._lifecycle_lock = threading.Lock()

    def submit(self, namespace, arguments_dict):
        result_id = self.tasks.submit(namespace, arguments_dict)
        self._wake.set()
        return {"result_id": result_id}

    def run_once(self):
        self.tasks.expire()
        return self.tasks.dispatch_pending(self.transport)

    def start(self):
        with self._lifecycle_lock:
            if self.transport is None or self._thread is not None:
                return
            self._stop.clear()

            def relay():
                while not self._stop.is_set():
                    self._wake.clear()
                    try:
                        self.run_once()
                    except Exception as exc:  # noqa: BLE001 - retry durable outbox after SQL/broker failures
                        logger.warning("Search outbox unavailable (%s)", type(exc).__name__)
                    self._wake.wait(1)

            self._thread = threading.Thread(target=relay, name="search-outbox", daemon=True)
            self._thread.start()

    def stop(self):
        with self._lifecycle_lock:
            self._stop.set()
            self._wake.set()
            if self._thread is not None:
                self._thread.join()
                self._thread = None

    def close(self):
        self.stop()
        if self.transport is not None:
            self.transport.close()
