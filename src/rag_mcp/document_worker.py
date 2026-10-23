"""Bounded document consumer; scale with independent worker processes."""

from __future__ import annotations

import argparse
import logging
import signal
import threading
from contextlib import contextmanager

from .service_knowledge import KnowledgeValidationError
from .state.upload_coordinator import UploadCoordinator
from .state.uploads import LeaseLost

logger = logging.getLogger(__name__)


class DocumentWorker:
    def __init__(self, runtime):
        self.runtime = runtime
        self.tasks = runtime.state.document_tasks
        self._mutex = threading.Lock()

    @contextmanager
    def _heartbeat(self, task):
        stop = threading.Event()
        errors = []

        def renew():
            while not stop.wait(self.tasks.settings.lease_seconds / 3):
                try:
                    self.tasks.heartbeat(task)
                except Exception as exc:  # noqa: BLE001 - ownership must fail closed on any renewal error
                    errors.append(exc)
                    return

        thread = threading.Thread(target=renew, name="document-task-lease", daemon=True)
        thread.start()
        try:
            yield
            if errors:
                raise LeaseLost("Document heartbeat failed") from errors[0]
        finally:
            stop.set()
            thread.join()

    def process(self, artifact_id):
        with self._mutex:
            task = self.tasks.claim(artifact_id)
            if task is None:
                return self.tasks.terminal(artifact_id)
            try:
                with self._heartbeat(task):
                    self.runtime.process_document_task(task)
            except Exception as exc:  # noqa: BLE001 - persist safe outcomes for all parser/backend failures
                if self.tasks.terminal(artifact_id):
                    return True  # Publication committed even if its acknowledgement was lost.
                try:
                    error = ("Invalid document or changed knowledge-base configuration"
                             if isinstance(exc, KnowledgeValidationError)
                             else f"Document processing failed ({type(exc).__name__})")
                    self.tasks.fail(task, retry=not isinstance(exc, KnowledgeValidationError), error=error)
                except LeaseLost:
                    pass
                logger.warning("Document processing deferred artifact_id=%s error_type=%s",
                               artifact_id, type(exc).__name__)
            finally:
                self.tasks.flush(artifact_id)
            return self.tasks.terminal(artifact_id)

    def recover(self):
        UploadCoordinator(self.runtime.state).recover(self.runtime._recovery_store)
        self.tasks.redrive_expired()


def main(argv=None):
    from .config import RagConfig
    from .http_service import RagHttpSettings
    from .service_knowledge import RagServiceRuntime
    from .startup_config import load_service_config
    from .state.document_queue import RocketMQDocumentQueue, document_queue_settings
    from .state.store import StateStores

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dispatch-only", action="store_true", help="Relay SQL outbox without inference")
    args = parser.parse_args(argv)
    load_service_config()
    settings = document_queue_settings()
    if not settings.endpoint:
        parser.error("RAG_ROCKETMQ_ENDPOINT is required")
    logging.basicConfig(level=logging.INFO)
    stop = threading.Event()
    for name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, name):
            signal.signal(getattr(signal, name), lambda *_: stop.set())
    state = StateStores.create(RagHttpSettings.from_env().data_root)
    runtime = None
    transport = None
    try:
        if args.dispatch_only:
            while not stop.is_set():
                try:
                    state.document_dispatcher.run_once()
                except Exception as exc:  # noqa: BLE001 - durable outbox retries across transport failures
                    logger.warning("Outbox dispatch unavailable (%s)", type(exc).__name__)
                stop.wait(2)
        else:
            runtime = RagServiceRuntime(RagConfig.from_env(), RagHttpSettings.from_env().data_root, state=state)
            worker = DocumentWorker(runtime)
            transport = RocketMQDocumentQueue(settings)
            state.document_dispatcher.start()
            while not stop.is_set():
                try:
                    worker.recover()
                    transport.consume_once(worker.process)
                except Exception as exc:  # noqa: BLE001 - keep consumer alive; do not acknowledge failed work
                    logger.warning("Document consumer unavailable (%s)", type(exc).__name__)
                    stop.wait(2)
    finally:
        if transport is not None:
            transport.close()
        if runtime is not None:
            runtime.close()
        else:
            state.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
