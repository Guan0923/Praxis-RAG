from concurrent.futures import ThreadPoolExecutor
from types import SimpleNamespace

import pytest
from redis_stub import MemoryRedis

from rag_mcp.state.cache import RedisStateCache, StateCacheSettings
from rag_mcp.state.transient import TransientStore, TransientUnavailable


@pytest.fixture
def storage():
    client = MemoryRedis()
    backend = RedisStateCache(StateCacheSettings(prefix="test:tasks"), client=client)
    state = SimpleNamespace(cache_sync=SimpleNamespace(backend=backend))
    return TransientStore(state), client


def test_required_backend_and_sanitized_failures(storage):
    store, client = storage
    client.broken = True
    with pytest.raises(TransientUnavailable, match="Redis task read unavailable"):
        store.get("search", "id")
    store.state.cache_sync.backend.enabled = False
    with pytest.raises(TransientUnavailable, match="Redis is required"):
        store.put("search", "id", {})


def test_ttl_and_namespaced_identity(storage):
    store, client = storage
    now = [10]
    client.clock = lambda: now[0]
    assert store.put("search", "id", {"status": "queued"}, ttl_seconds=2, nx=True)
    assert not store.put("search", "id", {"status": "failed"}, nx=True)
    assert store.get("search", "id") == {"status": "queued"}
    assert store.get("document", "id") is None
    now[0] += 2
    assert store.get("search", "id") is None


def test_atomic_updates(storage):
    store, _ = storage
    store.put("count", "id", {"value": 0})

    def increase(_):
        for _ in range(20):
            store.update("count", "id", lambda row: {"value": row["value"] + 1})

    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(increase, range(4)))
    assert store.get("count", "id") == {"value": 80}
    assert store.update("count", "id", lambda row: None) is None
    assert store.get("count", "id") == {"value": 80}


def test_domain_callback_error_is_not_masked(storage):
    store, _ = storage

    class Conflict(RuntimeError):
        pass

    def callback(row):
        raise Conflict("Already completed")

    with pytest.raises(Conflict, match="Already completed"):
        store.update("search", "id", callback)


def test_lock_ownership_and_expiry(storage):
    store, client = storage
    now = [0]
    client.clock = lambda: now[0]
    first = store.lock("document", "id", 2)
    assert first
    assert store.lock("document", "id", 2) is None
    assert not store.unlock("document", "id", "wrong")
    now[0] = 2
    second = store.lock("document", "id", 2)
    assert second and first != second
    assert not store.unlock("document", "id", first)
    assert store.unlock("document", "id", second)
