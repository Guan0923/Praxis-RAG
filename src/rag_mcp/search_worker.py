"""Sequential search consumer with retrieval isolated in an owned spawned process."""

from __future__ import annotations

import argparse
import logging
import multiprocessing
import queue
import signal
import threading
import time
from pathlib import Path
from types import SimpleNamespace

logger = logging.getLogger(__name__)


def _execute_search(connection, config, state_settings, data_root, namespace, arguments):
    """Spawn entry point: credentials travel through multiprocessing, not argv/files."""
    runtime = None
    state = None
    try:
        from .service_knowledge import RagServiceRuntime
        from .state.store import StateStores

        state = StateStores.create(Path(data_root), settings=state_settings, create_schema=False)
        runtime = RagServiceRuntime(config, Path(data_root), state=state)
        payload = runtime.search(namespace, arguments, save_snapshot=False)
        connection.send((True, payload))
    except Exception as exc:  # noqa: BLE001 - report a safe type, never provider exception text
        # Exception messages/tracebacks may include provider credentials or queries.
        connection.send((False, type(exc).__name__, isinstance(exc, ValueError)))
    finally:
        connection.close()
        if runtime is not None:
            runtime.close()
        elif state is not None:
            state.close()


class _ExecutionFailure(Exception):
    def __init__(self, retry=True):
        super().__init__("Search execution failed")
        self.retry = retry


class SearchWorker:
    """Only explicitly injected test executors run in a thread instead of a child.

    The production path always uses spawn, including on POSIX. No model, database
    connection, or inference thread is inherited from the consumer process.
    """

    def __init__(self, runtime, executor=None):
        self.runtime = runtime
        self.tasks = runtime.state.search_tasks
        self.executor = executor
        self._mutex = threading.Lock()
        self._child = None

    def _monitor(self, poll, deadline):
        while True:
            now = time.monotonic()
            if now >= deadline:
                raise TimeoutError("Search deadline expired")
            result = poll(min(0.1, deadline - now))
            if result is not None:
                if time.monotonic() >= deadline:
                    raise TimeoutError("Search deadline expired")
                if not result[0]:
                    raise _ExecutionFailure(retry=not result[2])
                return result[1]

    @staticmethod
    def _stop_child(child):
        # Never terminate by name or touch any process not created by this worker.
        if child.is_alive():
            child.terminate()
        child.join(timeout=2)
        if child.is_alive():
            child.kill()
            child.join(timeout=2)
        if child.is_alive():
            raise RuntimeError("Owned search subprocess did not stop")
        child.close()

    def _spawn(self, task, deadline):
        context = multiprocessing.get_context("spawn")
        receiver, sender = context.Pipe(duplex=False)
        child = context.Process(
            target=_execute_search,
            args=(sender, self.runtime.config, self.runtime.state.settings,
                  str(self.runtime.data_root), task["namespace"], task["arguments"]),
            name="rag-search-execution",
        )
        self._child = child
        started = False
        reader = None
        try:
            child.start()
            started = True
            sender.close()

            replies = queue.Queue(maxsize=1)

            def receive():
                # recv may block while a large payload crosses the pipe. Keep it
                # separate from the deadline monitor, never join before read.
                try:
                    replies.put(receiver.recv())
                except (EOFError, OSError):
                    replies.put((False, "ExecutionClosed", False))

            reader = threading.Thread(target=receive, name="search-result-pipe", daemon=True)
            reader.start()

            def poll(wait):
                try:
                    return replies.get(timeout=wait)
                except queue.Empty:
                    return None

            return self._monitor(poll, deadline)
        finally:
            sender.close()
            if started:
                self._stop_child(child)
                if reader is not None:
                    reader.join(timeout=2)
            else:
                child.close()
            receiver.close()
            self._child = None

    def _injected(self, task, deadline):
        # This path exists exclusively for deterministic local tests with lambdas.
        # Production never falls back here if spawn or runtime construction fails.
        replies = queue.Queue(maxsize=1)

        def execute():
            try:
                replies.put((True, self.executor(task["namespace"], task["arguments"])))
            except Exception as exc:  # noqa: BLE001 - test executors obey the same safe error protocol
                replies.put((False, type(exc).__name__, isinstance(exc, ValueError)))

        thread = threading.Thread(target=execute, name="search-test-executor", daemon=True)
        thread.start()

        def poll(wait):
            try:
                return replies.get(timeout=wait)
            except queue.Empty:
                return None

        return self._monitor(poll, deadline)

    def process(self, result_id):
        with self._mutex:
            task = self.tasks.claim(result_id)
            if task is None:
                return self.tasks.terminal(result_id)
            # Includes IPC/runtime startup; publication rechecks the SQL deadline.
            deadline = time.monotonic() + task["remaining_seconds"]
            try:
                execute = self._spawn if self.executor is None else self._injected
                payload = execute(task, deadline)
                self.tasks.complete(task, payload)
            except Exception as exc:  # noqa: BLE001 - all retrieval failures must leave a durable outcome
                # Completion could commit while its acknowledgement is lost.
                if self.tasks.terminal(result_id):
                    return True
                self.tasks.fail(task, retry=getattr(exc, "retry", True),
                                error="Search execution failed")
                logger.warning("Search deferred result_id=%s error_type=%s",
                               result_id, type(exc).__name__)
            return self.tasks.terminal(result_id)

    def close(self):
        # Normal CLI shutdown drains the current delivery until its persisted
        # deadline. This hook also cleans up a child after unexpected caller exit.
        if self._child is not None:
            self._stop_child(self._child)
            self._child = None


def main(argv=None):
    from .config import RagConfig
    from .http_service import RagHttpSettings
    from .startup_config import load_service_config
    from .state.search_queue import RocketMQSearchQueue, search_queue_settings
    from .state.store import StateStores

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dispatch-only", action="store_true", help="Relay SQL outbox without inference")
    args = parser.parse_args(argv)
    load_service_config()
    settings = search_queue_settings()
    if not settings.endpoint:
        parser.error("RAG_ROCKETMQ_ENDPOINT is required")
    logging.basicConfig(level=logging.INFO)
    stop = threading.Event()
    for name in ("SIGINT", "SIGTERM"):
        if hasattr(signal, name):
            signal.signal(getattr(signal, name), lambda *_: stop.set())
    data_root = RagHttpSettings.from_env().data_root
    state = StateStores.create(data_root)
    transport = None
    worker = None
    try:
        if args.dispatch_only:
            while not stop.is_set():
                try:
                    state.search_dispatcher.run_once()
                except Exception as exc:  # noqa: BLE001 - durable relay retries on the next pass
                    logger.warning("Search dispatch unavailable (%s)", type(exc).__name__)
                stop.wait(1)
        else:
            # The coordinator owns no Qdrant/model instance. Each execution child
            # constructs its own runtime, including when using local Qdrant paths.
            runtime = SimpleNamespace(config=RagConfig.from_env(), data_root=data_root, state=state)
            worker = SearchWorker(runtime)
            transport = RocketMQSearchQueue(settings)
            state.search_dispatcher.start()
            while not stop.is_set():
                try:
                    transport.consume_once(worker.process)
                except Exception as exc:  # noqa: BLE001 - leave failed delivery unacknowledged
                    logger.warning("Search consumer unavailable (%s)", type(exc).__name__)
                    stop.wait(1)
    finally:
        try:
            if worker is not None:
                worker.close()
        finally:
            try:
                if transport is not None:
                    transport.close()
            finally:
                state.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
