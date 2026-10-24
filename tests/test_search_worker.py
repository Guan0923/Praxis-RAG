"""Owned search subprocess termination and independent-task resource bounds."""

import multiprocessing
import time
from types import SimpleNamespace

import pytest
from sqlalchemy import select

from rag_mcp.config import RagConfig
from rag_mcp.search_worker import SearchWorker
from rag_mcp.state.schema import search_result_snapshots, search_tasks
from rag_mcp.state.search_tasks import SearchTaskSettings
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores


def sleeping_search(connection, _config, _settings, _root, _namespace, _arguments):
    time.sleep(30)


def large_search(connection, _config, _settings, _root, _namespace, _arguments):
    # Same child-to-parent envelope as the production target.
    connection.send((True, {"results": [{"text": "x" * (2 * 1024 * 1024)}]}))
    connection.close()


@pytest.fixture
def worker_state(tmp_path, monkeypatch):
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    state = StateStores(StateSettings(url=f"sqlite+pysqlite:///{tmp_path / 'worker.sqlite3'}"))
    state.knowledge_bases.create({"namespace": "manuals", "collection": "test"})
    runtime = SimpleNamespace(state=state, config=RagConfig(backend="qdrant"), data_root=tmp_path)
    yield runtime
    state.close()


def test_task_deadline_terminates_only_owned_inference_process(worker_state, monkeypatch):
    import rag_mcp.search_worker as module

    runtime = worker_state
    state = runtime.state
    state.search_tasks.settings = SearchTaskSettings(timeout_seconds=1)
    task_id = state.search_tasks.submit("manuals", {"query": "slow"})
    # submit's durable API may return the UUID or its response mapping.
    result_id = task_id["result_id"] if isinstance(task_id, dict) else task_id
    monkeypatch.setattr(module, "_execute_search", sleeping_search)

    def forbidden(_task):
        pytest.fail("Spawned search must not renew a SQL execution lease")

    monkeypatch.setattr(state.search_tasks, "heartbeat", forbidden, raising=False)
    before = {process.pid for process in multiprocessing.active_children()}
    worker = SearchWorker(runtime)
    try:
        started = time.monotonic()
        worker.process(result_id)
        assert time.monotonic() - started < 8
        assert {process.pid for process in multiprocessing.active_children()} <= before
        with state.engine.connect() as conn:
            status = conn.scalar(select(search_tasks.c.status).where(search_tasks.c.result_id == result_id))
            assert status == "timed_out"
            assert conn.scalar(select(search_result_snapshots.c.id)) is None
    finally:
        worker.close()


def test_injected_executor_never_heartbeats_and_ignores_legacy_leases(worker_state, monkeypatch):
    from sqlalchemy import update

    runtime = worker_state
    state = runtime.state
    result_id = state.search_tasks.submit("manuals", {"query": "manual"})
    with state.engine.begin() as conn:
        conn.execute(update(search_tasks).where(search_tasks.c.result_id == result_id).values(
            owner="old-worker", lease_until=10**12, attempts=1000, retry_at=10**12))

    def forbidden(_task):
        pytest.fail("Search worker must not renew a SQL execution lease")

    monkeypatch.setattr(state.search_tasks, "heartbeat", forbidden, raising=False)
    calls = []

    def execute(namespace, arguments):
        calls.append((namespace, arguments))
        return {"results": [{"text": "answer"}]}

    worker = SearchWorker(runtime, executor=execute)
    try:
        assert worker.process(result_id)
        assert worker.process(result_id)
        assert calls == [("manuals", {"query": "manual"})]
        assert state.search_tasks.get("manuals", result_id)["results"][0]["text"] == "answer"
    finally:
        worker.close()


def test_large_child_response_is_drained_before_join(worker_state, monkeypatch):
    import rag_mcp.search_worker as module

    runtime = worker_state
    state = runtime.state
    task_id = state.search_tasks.submit("manuals", {"query": "large"})
    result_id = task_id["result_id"] if isinstance(task_id, dict) else task_id
    monkeypatch.setattr(module, "_execute_search", large_search)
    worker = SearchWorker(runtime)
    try:
        assert worker.process(result_id)
        result = state.search_tasks.get("manuals", result_id)
        assert len(result["results"][0]["text"]) == 2 * 1024 * 1024
    finally:
        worker.close()


@pytest.mark.parametrize("permanent", [False, True])
def test_execution_failure_is_safe_and_only_broker_retries(worker_state, permanent):
    from sqlalchemy import update

    from rag_mcp.state.schema import search_task_outbox

    state = worker_state.state
    result_id = state.search_tasks.submit("manuals", {"query": "manual"})
    with state.engine.begin() as conn:
        conn.execute(update(search_task_outbox).values(pending=False))

    def execute(_namespace, _arguments):
        error = ValueError if permanent else ConnectionError
        raise error("private-provider-credential")

    worker = SearchWorker(worker_state, executor=execute)
    try:
        assert worker.process(result_id) is permanent
        with state.engine.connect() as conn:
            row = conn.execute(select(search_tasks)).mappings().one()
            assert row["status"] == ("failed" if permanent else "queued")
            assert row["stage"] == ("failed" if permanent else "retry_wait")
            assert "private-provider-credential" not in str(row)
            assert not conn.scalar(select(search_task_outbox.c.pending))
            assert conn.scalar(select(search_result_snapshots.c.id)) is None
    finally:
        worker.close()


@pytest.mark.parametrize("loser_fails", [False, True])
def test_duplicate_workers_compute_but_first_terminal_wins(worker_state, loser_fails):
    import threading
    from concurrent.futures import ThreadPoolExecutor

    runtime = worker_state
    result_id = runtime.state.search_tasks.submit("manuals", {"query": "duplicate"})
    barrier = threading.Barrier(2)
    winner_committed = threading.Event()

    def winner(_namespace, _arguments):
        barrier.wait(timeout=5)
        return {"results": [{"text": "winner"}]}

    def loser(_namespace, _arguments):
        barrier.wait(timeout=5)
        assert winner_committed.wait(5)
        if loser_fails:
            raise ConnectionError("private credential")
        return {"results": [{"text": "loser"}]}

    first = SearchWorker(runtime, executor=winner)
    second = SearchWorker(runtime, executor=loser)

    def process_first():
        try:
            return first.process(result_id)
        finally:
            winner_committed.set()

    try:
        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(process_first), pool.submit(second.process, result_id)]
            assert all(future.result(timeout=10) for future in futures)
        result = runtime.state.search_tasks.get("manuals", result_id)
        assert result["results"][0]["text"] == "winner"
    finally:
        first.close()
        second.close()


def test_worker_sql_failure_propagates_instead_of_acknowledging(worker_state, monkeypatch):
    state = worker_state.state
    result_id = state.search_tasks.submit("manuals", {"query": "manual"})

    def unavailable(*_args, **_kwargs):
        raise ConnectionError("SQL unavailable")

    monkeypatch.setattr(state.search_tasks, "complete", unavailable)
    monkeypatch.setattr(state.search_tasks, "fail", unavailable)
    worker = SearchWorker(worker_state, executor=lambda *_args: {"results": []})
    try:
        with pytest.raises(ConnectionError, match="SQL unavailable"):
            worker.process(result_id)
        with state.engine.connect() as conn:
            assert conn.scalar(select(search_result_snapshots.c.id)) is None
    finally:
        worker.close()
