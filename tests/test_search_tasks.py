"""Reliable queued retrieval, deadlines, result identity and Redis publication."""

import json
import threading
from concurrent.futures import ThreadPoolExecutor
from uuid import uuid4

import pytest
from sqlalchemy import select, update

from rag_mcp.contracts import SearchRequest
from rag_mcp.service_knowledge import RagServiceRuntime
from rag_mcp.state.result_snapshots import SnapshotUnavailable
from rag_mcp.state.schema import search_result_snapshots
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores


class Queue:
    def __init__(self):
        self.sent = []
        self.broken = False

    def send(self, result_id):
        if self.broken:
            raise ConnectionError("private broker details")
        self.sent.append(result_id)

    def close(self):
        pass


@pytest.fixture
def service(tmp_path, monkeypatch):
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    state = StateStores(StateSettings(url=f"sqlite+pysqlite:///{tmp_path / 'searches.sqlite3'}"))
    state.knowledge_bases.create({"namespace": "manuals", "collection": "test"})
    queue = Queue()
    state.search_dispatcher.transport = queue
    yield state, queue
    state.close()


def submit(service):
    state, _queue = service
    return state.search_dispatcher.submit("manuals", {"query": "manual"})["result_id"]


def complete(state, result_id, text="evidence"):
    task = state.search_tasks.claim(result_id)
    assert task is not None
    state.search_tasks.complete(task, {"query": "manual", "results": [{"text": text, "metadata": {}}]})
    return task


def test_admission_is_durable_uuid_only_and_poll_never_executes_search(service):
    state, queue = service
    result_id = submit(service)
    assert queue.sent == []
    state.search_dispatcher.run_once()
    assert queue.sent == [result_id]
    pending = state.search_tasks.get("manuals", result_id)
    assert pending["result_id"] == result_id and pending["status"] == "queued"
    with state.engine.connect() as conn:
        assert conn.scalar(select(search_result_snapshots.c.id)) is None
    complete(state, result_id)
    result = state.search_tasks.get("manuals", result_id)
    assert result["result_id"] == result_id
    assert result["results"][0]["text"] == "evidence"
    assert state.search_tasks.get("manuals", result_id) == result


def test_broker_failure_preserves_outbox_for_relay(service):
    state, queue = service
    queue.broken = True
    result_id = state.search_dispatcher.submit("manuals", {"query": "manual"})["result_id"]
    state.search_dispatcher.run_once()
    assert queue.sent == []
    assert state.search_tasks.get("manuals", result_id)["status"] == "queued"
    queue.broken = False
    state.search_dispatcher.run_once()
    assert queue.sent == [result_id]
    assert state.search_tasks.get("manuals", queue.sent[0])["status"] == "queued"


def test_duplicate_concurrent_consumers_can_claim(service):
    state, _queue = service
    result_id = submit(service)
    other = StateStores(state.settings)
    barrier = threading.Barrier(2)

    def claim(tasks):
        barrier.wait()
        return tasks.claim(result_id)

    try:
        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(claim, instance.search_tasks) for instance in (state, other)]
            claimed = [future.result(timeout=5) for future in futures]
        assert all(task is not None for task in claimed)
        assert all(task["status"] == "processing" for task in claimed)
        assert claimed[0]["arguments"] == claimed[1]["arguments"]
    finally:
        other.close()


def test_result_is_tenant_scoped_and_snapshot_expiry_is_an_error(service):
    state, _queue = service
    result_id = submit(service)
    assert state.search_tasks.get("other", result_id) is None
    assert state.search_tasks.get("manuals", str(uuid4())) is None
    complete(state, result_id)
    assert state.search_tasks.get("other", result_id) is None
    with state.engine.begin() as conn:
        conn.execute(update(search_result_snapshots).where(
            search_result_snapshots.c.id == result_id).values(expires_at=0))
    with pytest.raises(SnapshotUnavailable):
        state.search_tasks.get("manuals", result_id)


def test_failed_job_returns_safe_error_instead_of_queued(service):
    state, _queue = service
    result_id = submit(service)
    task = state.search_tasks.claim(result_id)
    state.search_tasks.fail(task, retry=False, error="Search processing failed")
    with pytest.raises(SnapshotUnavailable, match="failed"):
        state.search_tasks.get("manuals", result_id)
    assert state.search_tasks.terminal(result_id)


def test_atomic_snapshot_failure_does_not_mark_task_successful(service, monkeypatch):
    state, _queue = service
    result_id = submit(service)
    task = state.search_tasks.claim(result_id)
    original = state.results.save

    def abort(*args, **kwargs):
        original(*args, **kwargs)
        raise RuntimeError("abort before commit")

    monkeypatch.setattr(state.results, "save", abort)
    with pytest.raises(RuntimeError, match="abort"):
        state.search_tasks.complete(task, {"results": []})
    assert state.search_tasks.get("manuals", result_id)["status"] == "queued"
    with state.engine.connect() as conn:
        assert conn.scalar(select(search_result_snapshots.c.id)) is None


def test_completed_results_use_redis_and_redis_outage_falls_back_to_sql(service, monkeypatch):
    import time

    from rag_mcp.state.cache import RedisStateCache, StateCacheSettings

    class Redis:
        def __init__(self):
            self.values = {}
            self.deadlines = {}

        def set(self, key, value, ex=None, nx=False):
            self.values[key] = value
            self.deadlines[key] = time.time() + (ex or 60)

        def get(self, key):
            return self.values.get(key) if self.deadlines.get(key, 0) > time.time() else None

        def close(self):
            pass

    state, _queue = service
    state.cache_sync.backend.close()
    backend = RedisStateCache(StateCacheSettings(), client=Redis())
    state.cache_sync.backend = backend
    result_id = submit(service)
    complete(state, result_id)
    key = state.results._key("manuals", result_id)
    raw = backend._connection().get(key)
    assert json.loads(raw)["payload"]["result_id"] == result_id
    assert state.search_tasks.get("manuals", result_id)["results"][0]["text"] == "evidence"

    def fail(*_args, **_kwargs):
        raise ConnectionError("down")

    monkeypatch.setattr(backend._connection(), "get", fail)
    assert state.search_tasks.get("manuals", result_id)["results"][0]["text"] == "evidence"


def test_query_validation_precedes_submission(service):
    state, queue = service
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = state
    with pytest.raises(ValueError):
        runtime.submit_search("manuals", {"query": "manual", "wait_seconds": 2})
    assert queue.sent == []
    assert SearchRequest.from_arguments({"query": "manual"}).query == "manual"


def test_task_deadline_includes_queue_time_and_blocks_late_success(service):
    from rag_mcp.state.schema import search_tasks

    state, _queue = service
    result_id = submit(service)
    with state.engine.begin() as conn:
        conn.execute(update(search_tasks).where(search_tasks.c.result_id == result_id).values(deadline=0))
    with pytest.raises(SnapshotUnavailable, match="deadline|timed out"):
        state.search_tasks.get("manuals", result_id)
    assert state.search_tasks.claim(result_id) is None
    assert state.search_tasks.terminal(result_id)
    result_id = submit(service)
    task = state.search_tasks.claim(result_id)
    with state.engine.begin() as conn:
        conn.execute(update(search_tasks).where(search_tasks.c.result_id == result_id).values(deadline=0))
    assert state.search_tasks.complete(task, {"results": []}) is None
    assert state.search_tasks.terminal(result_id)
    with state.engine.connect() as conn:
        assert conn.scalar(select(search_result_snapshots.c.id).where(
            search_result_snapshots.c.id == result_id)) is None


def test_old_computation_can_publish_after_duplicate_claim_and_retry(service):
    from rag_mcp.state.schema import search_tasks

    state, _queue = service
    result_id = submit(service)
    old = state.search_tasks.claim(result_id)
    with state.engine.begin() as conn:
        conn.execute(update(search_tasks).where(search_tasks.c.result_id == result_id).values(
            owner="legacy", lease_until=10**12, attempts=1000, retry_at=10**12))
    fresh = state.search_tasks.claim(result_id)
    assert fresh is not None
    state.search_tasks.fail(fresh, retry=True)
    assert state.search_tasks.get("manuals", result_id)["stage"] == "retry_wait"
    assert state.search_tasks.complete(old, {"results": [{"text": "first"}]}) is not None
    assert state.search_tasks.complete(fresh, {"results": [{"text": "second"}]}) is None
    assert state.search_tasks.get("manuals", result_id)["results"][0]["text"] == "first"


@pytest.mark.parametrize("dispatch_first", [True, False])
def test_retries_never_reopen_or_change_initial_outbox(service, dispatch_first):
    from rag_mcp.state.schema import search_task_outbox, search_tasks

    state, queue = service
    result_id = submit(service)
    if dispatch_first:
        state.search_dispatcher.run_once()
    with state.engine.connect() as conn:
        initial = dict(conn.execute(select(search_task_outbox)).mappings().one())
    for _ in range(5):
        task = state.search_tasks.claim(result_id)
        state.search_tasks.fail(task, retry=True, error="token=private-provider-credential")
        assert state.search_tasks.get("manuals", result_id) == {
            "result_id": result_id, "status": "queued", "stage": "retry_wait"}
    with state.engine.connect() as conn:
        assert dict(conn.execute(select(search_task_outbox)).mappings().one()) == initial
        row = conn.execute(select(search_tasks)).mappings().one()
        assert json.loads(row["arguments"]) == {"query": "manual"}
        assert "private-provider-credential" not in str(row)
        assert row["attempts"] == 0
    state.search_dispatcher.run_once()
    state.search_dispatcher.run_once()
    assert queue.sent == [result_id]
    assert not state.search_tasks.terminal(result_id)


def test_expiry_sweep_is_bounded_and_clears_private_arguments(service):
    from rag_mcp.state.schema import search_task_outbox, search_tasks

    state, _queue = service
    ids = [submit(service) for _ in range(3)]
    with state.engine.begin() as conn:
        conn.execute(update(search_tasks).where(search_tasks.c.result_id.in_(ids)).values(deadline=0))
    assert state.search_tasks.expire(limit=2) == 2
    assert state.search_tasks.expire(limit=2) == 1
    with state.engine.connect() as conn:
        rows = conn.execute(select(search_tasks).where(search_tasks.c.result_id.in_(ids))).mappings().all()
        assert all(row["status"] == "timed_out" and not json.loads(row["arguments"]) for row in rows)
        assert all(conn.scalars(select(search_task_outbox.c.pending).where(search_task_outbox.c.result_id.in_(ids))))


def test_outbox_send_confirmation_cannot_clear_newer_generation(service):
    from rag_mcp.state.schema import search_task_outbox

    state, _queue = service
    result_id = submit(service)

    class RacingQueue:
        def send(self, incoming):
            with state.engine.begin() as conn:
                conn.execute(update(search_task_outbox).where(
                    search_task_outbox.c.result_id == incoming).values(
                        generation=search_task_outbox.c.generation + 1))

    state.search_tasks.dispatch(result_id, RacingQueue())
    with state.engine.connect() as conn:
        assert conn.scalar(select(search_task_outbox.c.pending).where(
            search_task_outbox.c.result_id == result_id))


def test_separate_state_instance_reads_same_job_after_restart(service):
    state, _queue = service
    result_id = submit(service)
    restarted = StateStores(state.settings)
    try:
        assert restarted.search_tasks.get("manuals", result_id)["status"] == "queued"
        complete(restarted, result_id)
        assert state.search_tasks.get("manuals", result_id)["result_id"] == result_id
    finally:
        restarted.close()


@pytest.mark.parametrize("competitor", ["complete", "fail", "dead_letter"])
def test_concurrent_terminal_outcomes_are_atomic_and_first_wins(service, competitor, monkeypatch):
    from rag_mcp.state.schema import search_tasks

    state, _queue = service
    result_id = submit(service)
    first = state.search_tasks.claim(result_id)
    second = state.search_tasks.claim(result_id)
    other = StateStores(state.settings)
    barrier = threading.Barrier(2)
    saves = []
    original = state.results.save

    def save(*args, **kwargs):
        saves.append(kwargs["result_id"])
        return original(*args, **kwargs)

    monkeypatch.setattr(state.results, "save", save)
    monkeypatch.setattr(other.results, "save", save)

    def publish():
        barrier.wait()
        return state.search_tasks.complete(first, {"results": [{"text": "first"}]})

    def compete():
        barrier.wait()
        if competitor == "complete":
            return other.search_tasks.complete(second, {"results": [{"text": "second"}]})
        if competitor == "fail":
            return other.search_tasks.fail(second, retry=False)
        return other.search_tasks.dead_letter(result_id)

    try:
        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(publish), pool.submit(compete)]
            outcomes = [future.result(timeout=10) for future in futures]
        with state.engine.connect() as conn:
            row = conn.execute(select(search_tasks)).mappings().one()
            snapshot = conn.execute(select(search_result_snapshots)).mappings().first()
        if row["status"] == "succeeded":
            assert snapshot is not None
            assert saves == [result_id]
            winner = state.search_tasks.get("manuals", result_id)
            assert winner["results"][0]["text"] in ("first", "second")
        else:
            assert row["status"] == "failed"
            assert snapshot is None
            assert saves == []
        if competitor == "complete":
            assert sum(outcome is not None for outcome in outcomes) == 1
        assert json.loads(row["arguments"]) == {}
        assert state.search_tasks.complete(first, {"results": []}) is None
        state.search_tasks.fail(second, retry=True)
        with state.engine.connect() as conn:
            assert dict(conn.execute(select(search_tasks)).mappings().one()) == dict(row)
    finally:
        other.close()


def test_deadline_during_snapshot_serialization_rolls_back_snapshot(service, monkeypatch):
    import time

    from rag_mcp.state.schema import search_tasks
    from rag_mcp.state.search_tasks import SearchTaskSettings

    state, _queue = service
    state.search_tasks.settings = SearchTaskSettings(timeout_seconds=0.2)
    result_id = submit(service)
    task = state.search_tasks.claim(result_id)
    original = state.results.save

    def delayed_save(*args, **kwargs):
        result = original(*args, **kwargs)
        time.sleep(0.3)
        return result

    monkeypatch.setattr(state.results, "save", delayed_save)
    assert state.search_tasks.complete(task, {"results": []}) is None
    with state.engine.connect() as conn:
        row = conn.execute(select(search_tasks)).mappings().one()
        assert row["status"] == "timed_out"
        assert json.loads(row["arguments"]) == {}
        assert conn.scalar(select(search_result_snapshots.c.id)) is None


@pytest.mark.parametrize("existing", ["queued", "processing", "succeeded", "failed", "timed_out", "unknown"])
def test_dead_letter_is_idempotent_and_respects_first_terminal(service, existing):
    from rag_mcp.state.schema import search_task_outbox, search_tasks

    state, _queue = service
    result_id = str(uuid4()) if existing == "unknown" else submit(service)
    if existing == "processing":
        state.search_tasks.claim(result_id)
    elif existing == "succeeded":
        complete(state, result_id)
    elif existing == "failed":
        state.search_tasks.fail(state.search_tasks.claim(result_id), retry=False)
    elif existing == "timed_out":
        with state.engine.begin() as conn:
            conn.execute(update(search_tasks).where(search_tasks.c.result_id == result_id).values(deadline=0))
    assert state.search_tasks.dead_letter(result_id) is True
    assert state.search_tasks.dead_letter(result_id) is True
    assert state.search_tasks.terminal(result_id)
    with state.engine.connect() as conn:
        row = conn.execute(select(search_tasks)).mappings().first()
        if existing == "unknown":
            assert row is None
            return
        expected = existing if existing in ("succeeded", "failed", "timed_out") else "failed"
        assert row["status"] == expected
        expected_stage = {"succeeded": "completed", "failed": "failed", "timed_out": "failed"}
        assert row["stage"] == expected_stage.get(existing, "dead_letter")
        assert json.loads(row["arguments"]) == {}
        assert conn.scalar(select(search_task_outbox.c.pending))
        assert bool(conn.scalar(select(search_result_snapshots.c.id))) == (existing == "succeeded")


def test_dead_letter_database_failure_propagates_without_ack(service, monkeypatch):
    state, _queue = service
    result_id = submit(service)

    def unavailable(*_args):
        raise ConnectionError("SQL unavailable")

    monkeypatch.setattr(state.search_tasks, "_row", unavailable)
    with pytest.raises(ConnectionError, match="SQL unavailable"):
        state.search_tasks.dead_letter(result_id)


@pytest.mark.parametrize("retry", [True, False])
def test_failure_obeys_sql_deadline_not_provider_error_text(service, retry):
    from rag_mcp.state.schema import search_tasks

    state, _queue = service
    result_id = submit(service)
    task = state.search_tasks.claim(result_id)
    state.search_tasks.fail(task, retry=retry, error="Search execution timed out")
    with state.engine.connect() as conn:
        assert conn.scalar(select(search_tasks.c.status)) == ("queued" if retry else "failed")
    result_id = submit(service)
    task = state.search_tasks.claim(result_id)
    with state.engine.begin() as conn:
        conn.execute(update(search_tasks).where(search_tasks.c.result_id == result_id).values(deadline=0))
    state.search_tasks.fail(task, retry=retry, error="provider error containing private token")
    with state.engine.connect() as conn:
        row = conn.execute(select(search_tasks).where(search_tasks.c.result_id == result_id)).mappings().one()
        assert row["status"] == "timed_out"
        assert json.loads(row["arguments"]) == {}
        assert "private token" not in str(row)


def test_dispatcher_only_expires_business_deadlines_never_redrives_processing(service):
    from rag_mcp.state.schema import search_tasks

    state, queue = service
    result_id = submit(service)
    state.search_dispatcher.run_once()
    state.search_tasks.claim(result_id)
    with state.engine.begin() as conn:
        conn.execute(update(search_tasks).where(search_tasks.c.result_id == result_id).values(
            lease_until=0, retry_at=0, updated_at=0))
    assert state.search_dispatcher.run_once() == 0
    assert queue.sent == [result_id]
    assert state.search_tasks.get("manuals", result_id)["stage"] == "processing"
    with state.engine.begin() as conn:
        conn.execute(update(search_tasks).where(search_tasks.c.result_id == result_id).values(deadline=0))
    state.search_dispatcher.run_once()
    assert queue.sent == [result_id]
    assert state.search_tasks.terminal(result_id)


def test_retry_during_initial_delivery_does_not_reopen_outbox(service):
    from rag_mcp.state.schema import search_task_outbox

    state, _queue = service
    result_id = submit(service)

    class ImmediateConsumer:
        def send(self, incoming):
            state.search_tasks.fail(state.search_tasks.claim(incoming), retry=True)

    state.search_tasks.dispatch(result_id, ImmediateConsumer())
    with state.engine.connect() as conn:
        row = conn.execute(select(search_task_outbox)).mappings().one()
        assert row["pending"] is False
        assert row["generation"] == 1


def test_search_settings_only_configure_business_timeout(monkeypatch):
    from rag_mcp.state.search_tasks import SearchTaskSettings

    for key in ("RAG_SEARCH_MAX_ATTEMPTS", "RAG_SEARCH_LEASE_SECONDS", "RAG_SEARCH_RETRY_SECONDS"):
        monkeypatch.setenv(key, "obsolete-invalid-config")
    monkeypatch.setenv("RAG_SEARCH_TIMEOUT_SECONDS", "30")
    assert SearchTaskSettings.from_env() == SearchTaskSettings(timeout_seconds=30)
    assert set(SearchTaskSettings.__dataclass_fields__) == {"timeout_seconds"}
    for value in (0, -1, float("inf"), float("nan"), True):
        with pytest.raises(ValueError):
            SearchTaskSettings(timeout_seconds=value)


@pytest.mark.parametrize("env_key,source", [
    ("RAG_SEARCH_TOPIC", "cache"), ("RAG_SEARCH_TOPIC", "document"),
    ("RAG_SEARCH_GROUP", "cache"), ("RAG_SEARCH_GROUP", "document"),
])
def test_search_queue_is_separate_from_other_queues(monkeypatch, env_key, source):
    from rag_mcp.state.cache_queue import RocketMQSettings
    from rag_mcp.state.document_queue import document_queue_settings
    from rag_mcp.state.search_queue import search_queue_settings

    cache = RocketMQSettings.from_env()
    document = document_queue_settings()
    settings = cache if source == "cache" else document
    monkeypatch.setenv(env_key, settings.topic if env_key.endswith("TOPIC") else settings.group)
    with pytest.raises(ValueError, match="separate"):
        search_queue_settings()


@pytest.mark.parametrize("outcome", ["processing", "succeeded", "failed", "timed_out", "dead_letter"])
def test_only_producer_confirmation_archives_initial_outbox(service, outcome):
    from rag_mcp.state.schema import search_task_outbox, search_tasks

    state, queue = service
    result_id = submit(service)
    task = state.search_tasks.claim(result_id)
    if outcome == "succeeded":
        state.search_tasks.complete(task, {"results": []})
    elif outcome == "failed":
        state.search_tasks.fail(task, retry=False)
    elif outcome == "timed_out":
        with state.engine.begin() as conn:
            conn.execute(update(search_tasks).where(search_tasks.c.result_id == result_id).values(deadline=0))
        state.search_tasks.expire()
    elif outcome == "dead_letter":
        state.search_tasks.dead_letter(result_id)
    with state.engine.connect() as conn:
        assert conn.scalar(select(search_task_outbox.c.pending)) is True
    state.search_dispatcher.run_once()
    assert queue.sent == [result_id]
    with state.engine.connect() as conn:
        assert conn.scalar(select(search_task_outbox.c.pending)) is False


def test_completed_callback_before_send_confirmation_is_still_archived(service):
    from rag_mcp.state.schema import search_task_outbox

    state, _queue = service
    result_id = submit(service)

    class ImmediateConsumer:
        def send(self, incoming):
            complete(state, incoming)
            with state.engine.connect() as conn:
                assert conn.scalar(select(search_task_outbox.c.pending)) is True

    state.search_tasks.dispatch(result_id, ImmediateConsumer())
    with state.engine.connect() as conn:
        assert conn.scalar(select(search_task_outbox.c.pending)) is False


def test_confirmation_loss_can_resend_completed_job_without_recomputing(service):
    from rag_mcp.state.schema import search_task_outbox

    state, queue = service
    result_id = submit(service)

    class ConfirmationLost:
        def send(self, incoming):
            complete(state, incoming)
            raise ConnectionError("send acknowledgement lost")

    with pytest.raises(ConnectionError):
        state.search_tasks.dispatch(result_id, ConfirmationLost())
    with state.engine.connect() as conn:
        assert conn.scalar(select(search_task_outbox.c.pending)) is True
    assert state.search_tasks.claim(result_id) is None
    state.search_tasks.dispatch_pending(queue)
    assert queue.sent == [result_id]
    with state.engine.connect() as conn:
        assert conn.scalar(select(search_task_outbox.c.pending)) is False
