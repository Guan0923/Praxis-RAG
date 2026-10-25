"""Immutable Redis results: exact replay, bounded size, expiry and isolation."""

from concurrent.futures import ThreadPoolExecutor
from threading import Barrier
from unittest.mock import Mock
from uuid import uuid4

import pytest
from test_search_tasks import service as service  # noqa: PLC0414 - pytest fixture registration

from rag_mcp.state.result_snapshots import (
    ResultSnapshots,
    SnapshotSettings,
    SnapshotUnavailable,
)


def test_pagination_detaches_full_response_without_inference(service):
    state, _queue = service
    payload = {"results": [{"rank": i + 1, "text": f"text {i}", "score": i / 100} for i in range(23)],
               "debug": {"retrieval": "hybrid", "timings": {"rerank": 0.25}},
               "custom": {"nested": [1, {"value": True}]}}
    saved = state.results.save("ns", payload)
    payload["results"][0]["text"] = "mutated"
    saved["debug"]["timings"]["rerank"] = 99
    page = state.results.get("ns", saved["result_id"], page=2, page_size=10)
    assert page["results"][0]["text"] == "text 10"
    assert page["total_results"] == 23 and page["next_page"] == 3
    replay = state.results.get("ns", saved["result_id"])
    assert replay["results"][0]["text"] == "text 0"
    assert replay["debug"]["timings"]["rerank"] == 0.25
    assert saved["expires_at"] - saved["created_at"] == 3600
    with pytest.raises(SnapshotUnavailable):
        state.results.get("other", saved["result_id"])


def test_expiry_eviction_and_outage_never_recover_from_sql(service):
    state, _queue = service
    clock = Mock(return_value=1000)
    snapshots = ResultSnapshots(state, clock=clock)
    saved = snapshots.save("ns", {"results": []})
    clock.return_value = saved["expires_at"]
    with pytest.raises(SnapshotUnavailable):
        snapshots.get("ns", saved["result_id"])
    clock.return_value = 1000
    snapshots.transient.delete("search-result", saved["result_id"])
    with pytest.raises(SnapshotUnavailable):
        snapshots.get("ns", saved["result_id"])
    state.cache_sync.backend._connection().broken = True
    for operation in [lambda: snapshots.get("ns", saved["result_id"]),
                      lambda: snapshots.save("ns", {"results": []})]:
        with pytest.raises(SnapshotUnavailable) as error:
            operation()
        assert "credential" not in str(error.value)


def test_snapshot_size_limit_and_empty_page(service):
    state, _queue = service
    snapshots = ResultSnapshots(state, settings=SnapshotSettings(max_result_bytes=10))
    with pytest.raises(SnapshotUnavailable, match="size"):
        snapshots.save("ns", {"results": [{"text": "too large"}]})
    saved = state.results.save("ns", {"results": []})
    assert state.results.get("ns", saved["result_id"])["next_page"] is None
    with pytest.raises(ValueError):
        state.results.get("ns", saved["result_id"], page=2)


def test_result_ttl_uses_result_setting_not_cache_ttl(service):
    state, _queue = service
    snapshots = ResultSnapshots(state, settings=SnapshotSettings(ttl_seconds=19))
    saved = snapshots.save("ns", {"results": []})
    client = state.cache_sync.backend._connection()
    key = snapshots.transient._key("search-result", saved["result_id"])
    assert client.expires[key] - client.clock() == pytest.approx(19, abs=0.1)
    assert saved["expires_at"] - saved["created_at"] == 19


@pytest.mark.parametrize("corrupt", [
    b"not json", b"\xff", b"[]", b"null", b"{}",
    b'{"namespace":"ns","expires_at":99999999999,"payload":null}',
    b'{"namespace":"ns","expires_at":NaN,"payload":{}}',
])
def test_corrupt_snapshot_is_unavailable_without_fallback(service, corrupt):
    state, _queue = service
    saved = state.results.save("ns", {"results": [{"text": "original"}]})
    key = state.results.transient._key("search-result", saved["result_id"])
    state.cache_sync.backend._connection().set(key, corrupt, ex=3600)
    with pytest.raises(SnapshotUnavailable):
        state.results.get("ns", saved["result_id"])


def test_uuid_is_globally_immutable_across_tenants(service):
    state, _queue = service
    saved = state.results.save("ns", {"results": [{"text": "original"}]})
    for namespace in ("ns", "other"):
        with pytest.raises(SnapshotUnavailable, match="already exists"):
            state.results.save(namespace, {"results": []}, result_id=saved["result_id"])
    assert state.results.get("ns", saved["result_id"])["results"] == saved["results"]


def test_concurrent_same_uuid_publishes_one_immutable_response(service):
    state, _queue = service
    result_id = str(uuid4())
    barrier = Barrier(2)

    def save(text):
        barrier.wait(timeout=5)
        try:
            return state.results.save("ns", {"results": [{"text": text}]}, result_id=result_id)
        except SnapshotUnavailable:
            return None

    with ThreadPoolExecutor(max_workers=2) as executor:
        successes = [result for result in executor.map(save, ["first", "second"]) if result is not None]
    assert len(successes) == 1
    assert state.results.get("ns", result_id)["results"] == successes[0]["results"]


@pytest.mark.parametrize("field,wrong", [("namespace", "other"), ("result_id", str(uuid4()))])
def test_wrong_tenant_or_result_identity_is_unavailable(service, field, wrong):
    state, _queue = service
    saved = state.results.save("ns", {"results": []})
    store = state.results.transient
    envelope = store.get("search-result", saved["result_id"])
    if field == "namespace":
        envelope[field] = wrong
    else:
        envelope["payload"][field] = wrong
    store.put("search-result", saved["result_id"], envelope)
    with pytest.raises(SnapshotUnavailable):
        state.results.get("ns", saved["result_id"])


@pytest.mark.parametrize("page,page_size", [(0, 10), (True, 10), (1, False), (1, 101), (1, 0)])
def test_invalid_paging(service, page, page_size):
    state, _queue = service
    saved = state.results.save("ns", {"results": []})
    with pytest.raises(ValueError):
        state.results.get("ns", saved["result_id"], page=page, page_size=page_size)


@pytest.mark.parametrize("value", ["", "not-a-uuid", "ABCDEFAB-1234-1234-1234-123456789ABC", None])
def test_invalid_identity(service, value):
    state, _queue = service
    with pytest.raises(SnapshotUnavailable):
        state.results.get("ns", value)


def test_purge_is_bounded_compatibility_noop_and_never_deletes_history(service):
    state, _queue = service
    assert state.results.purge_expired() == 0
    for size in [0, 1001, True, "1"]:
        for keyword in ("limit", "batch_size"):
            with pytest.raises(ValueError):
                state.results.purge_expired(**{keyword: size})


def test_sql_transaction_argument_is_explicitly_rejected(service):
    state, _queue = service
    with pytest.raises(ValueError, match="SQL transactions"):
        state.results.save("ns", {"results": []}, conn=object())
