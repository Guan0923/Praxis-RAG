"""Owned spawned retrieval, bounded deadlines and publish-before-ACK behavior."""

import multiprocessing
import time
from concurrent.futures import ThreadPoolExecutor
from threading import Barrier, Event
from types import SimpleNamespace

import pytest
from test_search_tasks import service as service  # noqa: PLC0414 - pytest fixture registration
from test_search_tasks import submit

from rag_mcp.config import RagConfig
from rag_mcp.search_worker import SearchWorker
from rag_mcp.state.search_tasks import SearchTaskSettings


def sleeping_search(connection, _config, _settings, _root, _namespace, _arguments):
    time.sleep(30)


def large_search(connection, _config, _settings, _root, _namespace, _arguments):
    connection.send((True, {"results": [{"text": "x" * (2 * 1024 * 1024)}]}))
    connection.close()


@pytest.fixture
def worker_state(service, tmp_path):
    state, _queue = service
    state.settings = None
    return SimpleNamespace(state=state, config=RagConfig(backend="qdrant"), data_root=tmp_path)


def test_task_deadline_terminates_only_owned_inference_process(service, worker_state, monkeypatch):
    import rag_mcp.search_worker as module

    state = worker_state.state
    state.search_tasks.settings = SearchTaskSettings(timeout_seconds=1)
    envelope = submit(service)
    monkeypatch.setattr(module, "_execute_search", sleeping_search)
    before = {process.pid for process in multiprocessing.active_children()}
    worker = SearchWorker(worker_state)
    try:
        started = time.monotonic()
        assert worker.process(envelope)
        assert time.monotonic() - started < 8
        assert {process.pid for process in multiprocessing.active_children()} <= before
        row = state.search_tasks.transient.get("search-result", envelope["result_id"])
        assert row["status"] == "timed_out" and "payload" not in row
    finally:
        worker.close()


def test_large_child_response_is_drained_before_join(service, worker_state, monkeypatch):
    import rag_mcp.search_worker as module

    envelope = submit(service)
    monkeypatch.setattr(module, "_execute_search", large_search)
    worker = SearchWorker(worker_state)
    try:
        assert worker.process(envelope)
        result = worker_state.state.search_tasks.get("manuals", envelope["result_id"])
        assert len(result["results"][0]["text"]) == 2 * 1024 * 1024
    finally:
        worker.close()


@pytest.mark.parametrize("permanent", [False, True])
def test_failure_is_safe_and_only_broker_retries(service, worker_state, permanent, caplog):
    envelope = submit(service)

    def execute(*_args):
        raise (ValueError if permanent else ConnectionError)("private-provider-credential")

    worker = SearchWorker(worker_state, executor=execute)
    assert worker.process(envelope) is permanent
    row = worker_state.state.search_tasks.transient.get("search-result", envelope["result_id"])
    assert row["status"] == ("failed" if permanent else "queued")
    assert row["stage"] == ("failed" if permanent else "retry_wait")
    assert "private-provider-credential" not in str(row) + caplog.text


def test_results_exist_before_ack_and_duplicate_delivery_does_not_reexecute(service, worker_state):
    envelope = submit(service)
    calls = []

    def execute(namespace, arguments):
        calls.append((namespace, arguments))
        return {"results": [{"text": "answer"}]}

    worker = SearchWorker(worker_state, executor=execute)
    assert worker.process(envelope) is True
    assert worker_state.state.results.get("manuals", envelope["result_id"])["results"] == [{"text": "answer"}]
    assert worker.process(envelope) is True
    assert len(calls) == 1


def test_wrong_namespace_and_expired_delivery_never_invoke_executor(service, worker_state):
    envelope = submit(service)
    worker = SearchWorker(worker_state, executor=lambda *_args: pytest.fail("must not execute"))
    assert worker.process({**envelope, "namespace": "other"})
    worker_state.state.search_tasks.clock = lambda: envelope["expires_at"]
    assert worker.process(envelope)
    with pytest.raises(ValueError, match="envelope"):
        worker.process(envelope["result_id"])


@pytest.mark.parametrize("loser_fails", [False, True])
def test_duplicate_workers_first_terminal_wins(service, worker_state, loser_fails):
    envelope = submit(service)
    barrier = Barrier(2)
    published = Event()

    def winner(*_args):
        barrier.wait(timeout=5)
        return {"results": [{"text": "winner"}]}

    def loser(*_args):
        barrier.wait(timeout=5)
        assert published.wait(5)
        if loser_fails:
            raise ConnectionError("private")
        return {"results": [{"text": "loser"}]}

    first = SearchWorker(worker_state, executor=winner)
    second = SearchWorker(worker_state, executor=loser)

    def process_first():
        try:
            return first.process(envelope)
        finally:
            published.set()

    with ThreadPoolExecutor(max_workers=2) as pool:
        futures = [pool.submit(process_first), pool.submit(second.process, envelope)]
        assert all(future.result(timeout=10) for future in futures)
    assert worker_state.state.results.get("manuals", envelope["result_id"])["results"] == [{"text": "winner"}]


def test_redis_publication_failure_does_not_ack(service, worker_state):
    envelope = submit(service)

    def execute(*_args):
        worker_state.state.cache_sync.backend._connection().broken = True
        return {"results": []}

    worker = SearchWorker(worker_state, executor=execute)
    with pytest.raises(RuntimeError, match="Redis"):
        worker.process(envelope)


def test_lost_completion_response_does_not_overwrite_saved_result(service, worker_state, monkeypatch):
    envelope = submit(service)
    tasks = worker_state.state.search_tasks
    complete = tasks.complete

    def lose_response(*args):
        complete(*args)
        raise ConnectionError("private lost response")

    monkeypatch.setattr(tasks, "complete", lose_response)
    worker = SearchWorker(worker_state, executor=lambda *_args: {"results": [{"text": "saved"}]})
    assert worker.process(envelope)
    assert tasks.get("manuals", envelope["result_id"])["results"] == [{"text": "saved"}]
