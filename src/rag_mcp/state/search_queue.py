"""Dedicated full-request search transport with synchronous broker admission."""

from __future__ import annotations

import logging
import os
from dataclasses import replace

from .cache_queue import RocketMQSettings
from .document_queue import RocketMQDocumentQueue, document_queue_settings
from .search_tasks import validate_search_envelope

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
    validate_payload = staticmethod(validate_search_envelope)

    def send(self, payload):
        payload = validate_search_envelope(payload)
        return self._send_payload(payload, payload["result_id"])

    def consume_once(self, callback):
        return self._consume_payload_once(callback, validate_search_envelope)


class SearchTaskDispatcher:
    def __init__(self, state, transport=None):
        self.tasks = state.search_tasks
        settings = search_queue_settings()
        self.transport = transport if transport is not None else (
            RocketMQSearchQueue(settings) if settings.endpoint else None)

    def submit(self, namespace, arguments_dict):
        if self.transport is None:
            raise RuntimeError("Search broker unavailable")
        envelope = self.tasks.submit(namespace, arguments_dict)
        try:
            self.transport.send(envelope)
            # A broker send alone is not acceptance if its poll state is unavailable.
            if self.tasks._read(namespace, envelope["result_id"]) is None:
                raise RuntimeError("Search admission unavailable")
        except Exception as exc:  # noqa: BLE001 - unconfirmed admission must never return success
            logger.warning("Search admission unconfirmed result_id=%s category=%s",
                           envelope["result_id"], type(exc).__name__)
            try:
                self.tasks.fail(envelope, retry=False)
            except Exception as cleanup_error:  # noqa: BLE001 - Redis may also be unavailable
                logger.warning("Search admission cleanup unavailable result_id=%s category=%s",
                               envelope["result_id"], type(cleanup_error).__name__)
            raise RuntimeError("Search queue acceptance was not confirmed") from None
        logger.info("Search admission confirmed result_id=%s", envelope["result_id"])
        return {"result_id": envelope["result_id"]}

    def run_once(self):
        return 0

    def start(self):
        return None

    def stop(self):
        return None

    def close(self):
        if self.transport is not None:
            self.transport.close()
