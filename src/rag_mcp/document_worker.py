"""Durable document consumer: load the job from MySQL, index, then ACK.

The broker message carries an identifier only. Everything the worker needs --
bytes, checksum, configuration version -- is read back from the durable job
row, so a redelivered or Debezium-replayed notification can never disagree with
the committed request.
"""

from __future__ import annotations

import argparse
import logging
import signal
import threading

from .service_knowledge import KnowledgeValidationError
from .state.document_jobs import (
    TERMINAL_STATUSES,
    heartbeat_seconds,
    validate_document_envelope,
)
from .state.upload_coordinator import UploadCoordinator
from .state.uploads import UploadConflict

logger = logging.getLogger(__name__)


class DocumentWorker:
    def __init__(self, runtime):
        self.runtime = runtime
        self.jobs = runtime.state.document_jobs
        self._mutex = threading.Lock()

    def _start_heartbeat(self, task):
        """Keep the SQL lease alive across long parses and model calls."""

        stop = threading.Event()

        def beat():
            while not stop.wait(heartbeat_seconds()):
                try:
                    self.jobs.heartbeat(task)
                except Exception as exc:  # noqa: BLE001 - the lease expiry path recovers
                    logger.warning("Document lease heartbeat failed artifact_id=%s error_type=%s",
                                   task["artifact_id"], type(exc).__name__)
                    return

        thread = threading.Thread(target=beat, name="document-job-heartbeat", daemon=True)
        thread.start()
        return stop, thread

    def process(self, envelope):
        envelope = validate_document_envelope(envelope)
        artifact_id = envelope["artifact_id"]
        with self._mutex:
            job = self.jobs.get(artifact_id, envelope["namespace"])
            if job is None:
                # Deliberately acknowledged: an unknown or purged job cannot be
                # made processable by retrying the delivery.
                logger.info("Document job unknown; acknowledging artifact_id=%s", artifact_id)
                return True
            if job["status"] in TERMINAL_STATUSES:
                return True
            if self.jobs.reconcile(job):
                return True
            task = self.jobs.claim(envelope)
            if task is None:
                # Another live worker holds the lease; leave the delivery for retry.
                return self.jobs.terminal(envelope)
            stop, heartbeat = self._start_heartbeat(task)
            try:
                if self.jobs.reconcile(task):
                    return True
                if self.jobs.execution_busy(task):
                    return False
                try:
                    self.runtime.process_document_task(task)
                    if not self.jobs.reconcile(task):
                        return False
                except Exception as exc:  # noqa: BLE001 - classify all parser/backend failures safely
                    if self.jobs.reconcile(task):
                        return True
                    try:
                        error = ("Invalid document or changed knowledge-base configuration"
                                 if isinstance(exc, KnowledgeValidationError)
                                 else f"Document processing failed ({type(exc).__name__})")
                        self.jobs.fail(task, retry=not isinstance(exc, KnowledgeValidationError),
                                       error=error)
                    except UploadConflict:
                        pass
                    logger.warning("Document processing deferred artifact_id=%s error_type=%s",
                                   artifact_id, type(exc).__name__)
                return self.jobs.terminal(artifact_id)
            finally:
                stop.set()
                heartbeat.join(timeout=heartbeat_seconds())

    def recover(self):
        try:
            self.jobs.sweep()
            self.jobs.gc_outbox()
            self.jobs.gc_terminal()
        except Exception as exc:  # noqa: BLE001 - maintenance never blocks consumption
            logger.warning("Document job maintenance failed (%s)", type(exc).__name__)
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
