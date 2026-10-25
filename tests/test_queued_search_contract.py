"""Bounded MCP waits and immutable snapshot publication without retrieval repeats."""

import threading
import time
from types import SimpleNamespace
from uuid import uuid4

import anyio
import pytest
from test_search_tasks import service as service  # noqa: PLC0414 - pytest fixture registration

from rag_mcp.contracts import SearchValidationError
from rag_mcp.mcp_http import McpHttpEndpoint
from rag_mcp.server import QueuedSearchRequest, create_registry, search_tool_definition
from rag_mcp.state.result_snapshots import SnapshotUnavailable


@pytest.mark.parametrize("wait", [-1, 31, True, "5", None, float("nan"), float("inf")])
def test_wait_validation_precedes_submission(wait):
    with pytest.raises(SearchValidationError, match="wait_seconds"):
        QueuedSearchRequest.from_arguments({"query": "manual", "wait_seconds": wait})


def test_omitted_wait_defaults_to_ten_seconds():
    request = QueuedSearchRequest.from_arguments({"query": "manual"})
    assert request.wait_seconds == 10
    assert QueuedSearchRequest(request.search).wait_seconds == 10
    assert search_tool_definition(queued=True).input_schema["properties"]["wait_seconds"]["default"] == 10


@pytest.mark.parametrize("wait", [0, 0.05, 5, 30])
def test_wait_is_not_part_of_retrieval_arguments(wait):
    request = QueuedSearchRequest.from_arguments({"query": "manual", "wait_seconds": wait})
    assert request.wait_seconds == wait
    assert request.search.query == "manual"
    assert not hasattr(request.search, "wait_seconds")
    assert search_tool_definition(queued=True).input_schema["properties"]["wait_seconds"]["maximum"] == 30
    assert "wait_seconds" not in search_tool_definition().input_schema["properties"]


class Runtime:
    def __init__(self):
        self.result_id = str(uuid4())
        self.submissions = []
        self.reads = []
        self.ready = False
        self.error = None

    def submit_search(self, namespace, arguments):
        self.submissions.append((namespace, arguments))
        return {"result_id": self.result_id}

    def get_search_result(self, namespace, result_id, **_kwargs):
        self.reads.append((namespace, result_id))
        if self.error:
            raise SnapshotUnavailable(self.error)
        if self.ready:
            return {"result_id": result_id, "results": [{"text": "evidence"}]}
        return {"result_id": result_id, "status": "queued", "stage": "processing"}


def endpoint(runtime):
    # Exercise the real endpoint wait loop without transport startup or background consumers.
    return SimpleNamespace(runtime=runtime)


def test_zero_wait_returns_only_uuid_without_querying_result():
    runtime = Runtime()
    response = anyio.run(McpHttpEndpoint.search_queued, endpoint(runtime), "manuals",
                         QueuedSearchRequest.from_arguments({"query": "manual", "wait_seconds": 0}))
    assert response == {"result_id": runtime.result_id}
    assert len(runtime.submissions) == 1
    assert runtime.reads == []


def test_completion_within_wait_returns_normal_results_and_same_uuid():
    runtime = Runtime()

    async def scenario():
        async def complete():
            await anyio.sleep(0.02)
            runtime.ready = True

        async with anyio.create_task_group() as tasks:
            tasks.start_soon(complete)
            response = await McpHttpEndpoint.search_queued(
                endpoint(runtime), "manuals", QueuedSearchRequest.from_arguments({"query": "manual", "wait_seconds": 1}))
        assert response == {"result_id": runtime.result_id, "results": [{"text": "evidence"}]}
        assert len(runtime.submissions) == 1

    anyio.run(scenario)


def test_wait_expiry_does_not_fail_or_resubmit_task():
    runtime = Runtime()
    started = time.monotonic()
    response = anyio.run(McpHttpEndpoint.search_queued, endpoint(runtime), "manuals",
                         QueuedSearchRequest.from_arguments({"query": "manual", "wait_seconds": 0.03}))
    assert response == {"result_id": runtime.result_id}
    assert time.monotonic() - started < 0.8
    assert len(runtime.submissions) == 1
    runtime.ready = True
    assert runtime.get_search_result("manuals", runtime.result_id)["results"]


def test_slow_database_read_cannot_extend_mcp_wait_indefinitely():
    runtime = Runtime()
    release = threading.Event()
    finished = threading.Event()
    original = runtime.get_search_result

    def slow(*args, **kwargs):
        try:
            release.wait(2)
            return original(*args, **kwargs)
        finally:
            finished.set()

    runtime.get_search_result = slow
    try:
        started = time.monotonic()
        response = anyio.run(McpHttpEndpoint.search_queued, endpoint(runtime), "manuals",
                             QueuedSearchRequest.from_arguments({"query": "manual", "wait_seconds": 0.03}))
        assert response == {"result_id": runtime.result_id}
        assert time.monotonic() - started < 0.8
    finally:
        release.set()
        assert finished.wait(2)


def test_cancelling_client_wait_does_not_submit_another_task():
    runtime = Runtime()

    async def scenario():
        with anyio.move_on_after(0.03):
            await McpHttpEndpoint.search_queued(
                endpoint(runtime), "manuals", QueuedSearchRequest.from_arguments({"query": "manual", "wait_seconds": 5}))
        assert len(runtime.submissions) == 1
        runtime.ready = True
        assert runtime.get_search_result("manuals", runtime.result_id)["results"]

    anyio.run(scenario)


def test_failed_job_is_mcp_error_not_queued():
    runtime = Runtime()
    runtime.error = "Search task exceeded its execution deadline"

    async def queued(request):
        return await McpHttpEndpoint.search_queued(endpoint(runtime), "manuals", request)

    registry = create_registry(SimpleNamespace(search=None, search_queued=queued))
    result = anyio.run(registry.invoke_async, "search", {"query": "manual", "wait_seconds": 1})
    assert result.is_error
    assert "deadline" in result.content[0].text


def test_completion_publishes_bytes_and_status_before_returning_ack(service):
    from test_search_tasks import submit

    from rag_mcp.search_worker import SearchWorker

    state, _queue = service
    envelope = submit(service)
    worker = SearchWorker(SimpleNamespace(state=state), executor=lambda *_args: {"results": [{"text": "ready"}]})
    assert worker.process(envelope) is True
    row = state.search_tasks.transient.get("search-result", envelope["result_id"])
    assert row["status"] == "succeeded"
    assert row["payload"]["results"] == [{"text": "ready"}]
    assert row["payload"]["result_id"] == envelope["result_id"]
