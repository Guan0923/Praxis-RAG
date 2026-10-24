"""Consume broker dead letters into terminal SQL state without inference or re-enqueueing."""

from __future__ import annotations

import argparse
import logging
import os
import re
import signal
import threading
from dataclasses import replace
from pathlib import Path

from .state.cache_queue import RocketMQSettings
from .state.document_queue import RocketMQDocumentQueue, document_queue_settings
from .state.document_tasks import validate_artifact_id

logger = logging.getLogger(__name__)


def dead_letter_queue_settings(kind):
    """Use the physical source group, not a business namespace or namespace metadata.

    Apache Broker 5.3.3 addresses Resource.name directly. Its DLQ name is
    %DLQ%<configured source group>, including any explicit ns% prefix. SDK
    resource_namespace is retained as metadata, not prepended a second time.
    """
    from .state.search_queue import search_queue_settings

    sources = {"document": document_queue_settings(), "search": search_queue_settings()}
    if kind not in sources:
        raise ValueError("Dead-letter kind must be document or search")
    groups = {
        name: os.getenv(f"RAG_{name.upper()}_DLQ_GROUP", f"praxis_{name}_dead_letters").strip()
        for name in sources
    }
    reserved = {source.group for source in sources.values()} | {RocketMQSettings.from_env().group}
    if (any(not re.fullmatch(r"[%A-Za-z0-9_-]+", group) for group in groups.values())
            or len(set(groups.values())) != len(groups) or reserved.intersection(groups.values())):
        raise ValueError("Dead-letter consumer groups must be valid, independent groups")
    source = sources[kind]
    if not re.fullmatch(r"[%A-Za-z0-9_-]+", source.group):
        raise ValueError("Invalid dead-letter source consumer group")
    return replace(source, topic=f"%DLQ%{source.group}", group=groups[kind])


class DeadLetterWorker:
    """ACK eligibility is returned only by the committed authoritative transaction."""

    def __init__(self, state, kind):
        if kind not in {"document", "search"}:
            raise ValueError("Dead-letter kind must be document or search")
        self.tasks = getattr(state, f"{kind}_tasks")

    def process(self, task_id):
        validate_artifact_id(task_id)
        complete = self.tasks.dead_letter(task_id)
        if complete is True:
            logger.info("Dead-letter transaction committed or task already absent/terminal")
        return complete is True


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kind", choices=("document", "search"), required=True)
    args = parser.parse_args(argv)

    from .startup_config import load_service_config
    from .state.store import StateStores

    load_service_config()
    settings = dead_letter_queue_settings(args.kind)
    if not settings.endpoint:
        parser.error("RAG_ROCKETMQ_ENDPOINT is required")
    logging.basicConfig(level=logging.INFO)
    stop = threading.Event()
    for name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, name):
            signal.signal(getattr(signal, name), lambda *_: stop.set())
    data_root = Path(os.getenv("RAG_SERVICE_DATA_ROOT", str(Path.home() / ".praxis-rag" / "data"))).expanduser()
    state = StateStores.create(data_root)
    transport = None
    try:
        worker = DeadLetterWorker(state, args.kind)
        transport = RocketMQDocumentQueue(settings)
        transport.message_key = "artifact_id" if args.kind == "document" else "result_id"
        while not stop.is_set():
            try:
                transport.consume_once(worker.process)
            except Exception as exc:  # noqa: BLE001 - keep failed SQL transactions and ACKs retryable
                logger.warning("Dead-letter consumer unavailable category=%s",
                               transport._failure_category(exc))
                stop.wait(2)
    finally:
        try:
            if transport is not None:
                transport.close()
        finally:
            state.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
