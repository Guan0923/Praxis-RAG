"""Bounded document consumer; scale with independent worker processes."""

from __future__ import annotations

import argparse
import logging
import signal
import threading

from .service_knowledge import KnowledgeValidationError
from .state.upload_coordinator import UploadCoordinator
from .state.uploads import UploadConflict

logger = logging.getLogger(__name__)


class DocumentWorker:
    def __init__(self, runtime):
        self.runtime = runtime
        self.tasks = runtime.state.document_tasks
        self._mutex = threading.Lock()

    def process(self, envelope):
        from .state.document_tasks import validate_document_envelope

        envelope = validate_document_envelope(envelope)
        artifact_id = envelope["artifact_id"]
        with self._mutex:
            if self.tasks.reconcile(envelope) or self.tasks.terminal(envelope):
                return True
            task = self.tasks.assemble(envelope)
            if task is None:
                # Never ACK partial payloads: Redis TTL/loss must not lose the file.
                return False
            token = self.tasks.transient.lock("document_execution", artifact_id, 300)
            if token is None:
                return False
            try:
                if self.tasks.reconcile(task):
                    return True
                if self.tasks.execution_busy(task):
                    return False
                task = self.tasks.claim(task)
                if task is None:
                    return self.tasks.terminal(artifact_id)
                try:
                    self.runtime.process_document_task(task)
                    if not self.tasks.reconcile(task):
                        return False
                except Exception as exc:  # noqa: BLE001 - classify all parser/backend failures safely
                    if self.tasks.reconcile(task):
                        return True
                    try:
                        error = ("Invalid document or changed knowledge-base configuration"
                                 if isinstance(exc, KnowledgeValidationError)
                                 else f"Document processing failed ({type(exc).__name__})")
                        self.tasks.fail(task, retry=not isinstance(exc, KnowledgeValidationError), error=error)
                    except UploadConflict:
                        pass
                    logger.warning("Document processing deferred artifact_id=%s error_type=%s",
                                   artifact_id, type(exc).__name__)
                return self.tasks.terminal(artifact_id)
            finally:
                self.tasks.transient.unlock("document_execution", artifact_id, token)

    def recover(self):
        UploadCoordinator(self.runtime.state).recover(self.runtime._recovery_store)


def main(argv=None):
    from .config import RagConfig
    from .http_service import RagHttpSettings
    from .service_knowledge import RagServiceRuntime
    from .startup_config import load_service_config
    from .state.document_queue import RocketMQDocumentQueue, document_queue_settings
    from .state.store import StateStores

    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args(argv)
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
        runtime = RagServiceRuntime(RagConfig.from_env(), RagHttpSettings.from_env().data_root, state=state)
        worker = DocumentWorker(runtime)
        transport = RocketMQDocumentQueue(settings)
        while not stop.is_set():
            try:
                worker.recover()
                transport.consume_once(worker.process)
            except Exception as exc:  # noqa: BLE001 - retain broker delivery on transient failure
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
