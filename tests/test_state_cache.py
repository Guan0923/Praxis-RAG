"""Redis state cache contract; real Redis tests opt in via RAG_TEST_REDIS_URL."""

from __future__ import annotations

import os
import sys
import traceback
import uuid
from types import SimpleNamespace

import pytest

from rag_mcp.state.cache import CacheSettings, CacheUnavailable, RedisStateCache, StateCacheSettings


class FakeRedis:
    """Deterministic time and Redis-like atomic eval for backend contract tests."""

    def __init__(self):
        self.values = {}
        self.expires = {}
        self.now = 0
        self.closed = False
        self.last_script = None

    def advance(self, seconds):
        self.now += seconds

    def get(self, key):
        if self.expires.get(key, float("inf")) <= self.now:
            self.values.pop(key, None)
            self.expires.pop(key, None)
        return self.values.get(key)

    def mget(self, keys):
        return [self.get(key) for key in keys]

    def eval(self, script, numkeys, payload_key, revision_key, revision, deleted, payload, ttl):
        assert numkeys == 2
        self.last_script = script
        watermark = self.get(revision_key)
        if watermark is not None:
            old_revision, old_deleted = watermark.decode().split(":")
            if int(old_revision) > int(revision):
                return 0
            if old_revision == revision and old_deleted == "1" and deleted == "0":
                return 0
        self.values[payload_key] = payload.encode()
        self.values[revision_key] = f"{revision}:{deleted}".encode()
        self.expires[payload_key] = self.now + int(ttl)
        self.expires.pop(revision_key, None)
        return 1

    def close(self):
        self.closed = True


@pytest.fixture()
def cache():
    return RedisStateCache(StateCacheSettings(), client=FakeRedis())


@pytest.fixture()
def clean_env(monkeypatch):
    for name in list(os.environ):
        if name.startswith("RAG_REDIS_"):
            monkeypatch.delenv(name)
    return monkeypatch


def test_settings_alias_matches_parent_import():
    assert CacheSettings is StateCacheSettings
    assert isinstance(CacheSettings(), StateCacheSettings)


def test_settings_defaults_and_disabled_backend(clean_env):
    settings = StateCacheSettings.from_env()
    assert settings.url is None
    assert settings.ttl_seconds == 60
    assert settings.timeout_seconds == 0.3
    backend = RedisStateCache(settings)
    assert not backend.enabled
    assert backend.get("knowledge", "alpha") == (False, None)
    assert backend.put("knowledge", "alpha", 1, {"name": "alpha"}) is False
    backend.close()
    assert backend._client is None


def test_settings_environment_and_url_redaction(clean_env):
    clean_env.setenv("RAG_REDIS_URL", "redis://private-user:private-password@localhost/0")
    clean_env.setenv("RAG_REDIS_PREFIX", "my-state")
    clean_env.setenv("RAG_REDIS_TTL_SECONDS", "90")
    clean_env.setenv("RAG_REDIS_TIMEOUT_SECONDS", "0.7")
    settings = StateCacheSettings.from_env()
    assert settings.ttl_seconds == 90
    assert settings.timeout_seconds == 0.7
    assert settings.prefix == "my-state"
    assert "private-user" not in repr(settings)
    assert "private-password" not in repr(settings)
    assert RedisStateCache(settings).enabled
    clean_env.setenv("RAG_REDIS_ENABLED", "off")
    assert RedisStateCache(StateCacheSettings.from_env(), client=FakeRedis()).enabled is False


@pytest.mark.parametrize("name,value", [
    ("RAG_REDIS_TTL_SECONDS", "0"),
    ("RAG_REDIS_TTL_SECONDS", "-1"),
    ("RAG_REDIS_TTL_SECONDS", "bad-secret-value"),
    ("RAG_REDIS_TIMEOUT_SECONDS", "0"),
    ("RAG_REDIS_TIMEOUT_SECONDS", "nan"),
    ("RAG_REDIS_TIMEOUT_SECONDS", "inf"),
    ("RAG_REDIS_TIMEOUT_SECONDS", "bad-secret-value"),
    ("RAG_REDIS_ENABLED", "bad-secret-value"),
    ("RAG_REDIS_PREFIX", ""),
])
def test_invalid_settings_have_sanitized_errors(clean_env, name, value):
    clean_env.setenv(name, value)
    with pytest.raises(ValueError) as error:
        StateCacheSettings.from_env()
    assert name in str(error.value)
    assert "bad-secret-value" not in str(error.value)


def test_json_values_round_trip_and_tombstone(cache):
    assert cache.get("documents", "alpha") == (False, None)
    value = {"namespace": "alpha", "items": [1, True, None, "中文"], "nested": {"enabled": False}}
    assert cache.put("documents", "alpha", 1, value)
    assert cache.get("documents", "alpha") == (True, value)
    assert cache.put("documents", "alpha", 2, None)
    assert cache.get("documents", "alpha") == (True, None)
    assert cache.put("documents", "alpha", 3, {"discarded": "sensitive"}, deleted=True)
    assert cache.get("documents", "alpha") == (True, None)
    assert b"sensitive" not in b"".join(cache._client.values.values())


def test_keys_are_case_sensitive_hashed_unambiguous_and_cluster_colocated(cache):
    identities = [("kind", "Alpha"), ("kind", "alpha"), ("Kind", "alpha"),
                  ("a:b", "c"), ("a", "b:c"), ("", ""), ("kind", "租户")]
    keys = [cache._keys(*identity) for identity in identities]
    assert len(set(key[0] for key in keys)) == len(identities)
    for payload, revision in keys:
        assert payload.startswith("rag:state:v1:")
        assert payload.split("{")[1].split("}")[0] == revision.split("{")[1].split("}")[0]
    secret_scope = "namespace-with-private-name"
    cache.put("private-kind", secret_scope, 1, [])
    assert all(secret_scope not in key and "private-kind" not in key for key in cache._client.values)


def test_newer_revision_rejects_stale_fill_and_same_revision_can_refresh(cache):
    assert cache.put("knowledge", "alpha", 9, {"version": 9})
    assert cache.put("knowledge", "alpha", 10, {"version": 10})
    assert not cache.put("knowledge", "alpha", 9, {"version": 9})
    assert cache.get("knowledge", "alpha") == (True, {"version": 10})
    cache._client.advance(60)
    assert cache.get("knowledge", "alpha") == (False, None)
    assert not cache.put("knowledge", "alpha", 9, {"version": 9})
    assert cache.put("knowledge", "alpha", 10, {"version": 10})
    assert cache.get("knowledge", "alpha") == (True, {"version": 10})


def test_tombstone_watermark_survives_payload_expiry(cache):
    assert cache.put("knowledge", "alpha", 10, None, deleted=True)
    assert not cache.put("knowledge", "alpha", 10, {"resurrected": True})
    cache._client.advance(61)
    payload_key, revision_key = cache._keys("knowledge", "alpha")
    assert cache.get("knowledge", "alpha") == (False, None)
    assert cache._client.get(payload_key) is None
    assert cache._client.get(revision_key) == b"10:1"
    assert revision_key not in cache._client.expires
    assert not cache.put("knowledge", "alpha", 9, {})
    assert not cache.put("knowledge", "alpha", 10, {})
    assert cache.put("knowledge", "alpha", 10, None, deleted=True)
    assert cache.get("knowledge", "alpha") == (True, None)
    assert cache.put("knowledge", "alpha", 11, {"recreated": True})
    assert cache.get("knowledge", "alpha") == (True, {"recreated": True})


def test_revisions_above_lua_number_precision(cache):
    revision = 2**63 - 2
    assert cache.put("catalog", "alpha", revision + 1, ["new"])
    assert not cache.put("catalog", "alpha", revision, ["old"])
    assert cache.get("catalog", "alpha") == (True, ["new"])
    assert "tonumber" not in cache._client.last_script
    assert cache.put("catalog", "alpha", 10**40, ["largest"])
    assert not cache.put("catalog", "alpha", 10**40 - 1, ["older"])


@pytest.mark.parametrize("revision", [-1, True, 1.0, "1"])
def test_invalid_revision_is_rejected(cache, revision):
    with pytest.raises(ValueError, match="revision"):
        cache.put("kind", "scope", revision, {})


def test_get_rejects_mismatched_or_missing_watermark(cache):
    cache.put("kind", "scope", 1, ["old"])
    _, revision_key = cache._keys("kind", "scope")
    cache._client.values[revision_key] = b"2:0"
    assert cache.get("kind", "scope") == (False, None)
    cache._client.values.pop(revision_key)
    assert cache.get("kind", "scope") == (False, None)


@pytest.mark.parametrize("payload", [b"not JSON", b"[]", b'{}',
    b'{"schema":1,"revision":"01","deleted":false,"value":[]}',
    b'{"schema":1,"revision":1,"deleted":false,"value":[]}',
    b'{"schema":1,"revision":"1","deleted":0,"value":[]}'])
def test_corrupt_payload_is_sanitized(cache, payload):
    payload_key, _ = cache._keys("kind", "scope")
    cache._client.values[payload_key] = payload
    with pytest.raises(CacheUnavailable, match="read unavailable"):
        cache.get("kind", "scope")


def test_invalid_json_value_is_sanitized(cache):
    with pytest.raises(CacheUnavailable, match="write unavailable"):
        cache.put("kind", "scope", 1, {"value": float("nan")})


class BrokenRedis:
    def __getattr__(self, name):
        def fail(*args, **kwargs):
            raise RuntimeError("redis://private-user:private-password@host private-payload")
        return fail


@pytest.mark.parametrize("operation", ["read", "write", "close"])
def test_redis_faults_are_sanitized(operation):
    cache = RedisStateCache(StateCacheSettings(), client=BrokenRedis())
    with pytest.raises(CacheUnavailable) as error:
        if operation == "read":
            cache.get("kind", "scope")
        elif operation == "write":
            cache.put("kind", "scope", 1, {})
        else:
            cache.close()
    rendered = "".join(traceback.format_exception(error.value))
    assert "private-user" not in rendered
    assert "private-password" not in rendered
    assert "private-payload" not in rendered


def test_client_creation_is_lazy_and_timeouts_are_applied(monkeypatch):
    calls = []
    client = FakeRedis()

    def from_url(url, **kwargs):
        calls.append((url, kwargs))
        return client

    monkeypatch.setitem(sys.modules, "redis", SimpleNamespace(Redis=SimpleNamespace(from_url=from_url)))
    settings = StateCacheSettings(url="redis://localhost/0", timeout_seconds=0.7)
    cache = RedisStateCache(settings)
    assert calls == []
    assert cache.get("kind", "scope") == (False, None)
    assert calls == [(settings.url, {
        "socket_timeout": 0.7, "socket_connect_timeout": 0.7, "decode_responses": False,
    })]
    cache.get("kind", "scope")
    assert len(calls) == 1


def test_client_creation_errors_are_sanitized(monkeypatch):
    def from_url(*args, **kwargs):
        raise ValueError("redis://private-user:private-password@host")

    monkeypatch.setitem(sys.modules, "redis", SimpleNamespace(Redis=SimpleNamespace(from_url=from_url)))
    cache = RedisStateCache(StateCacheSettings(url="redis://private-user:private-password@host"))
    with pytest.raises(CacheUnavailable) as error:
        cache.get("kind", "scope")
    assert "private-password" not in "".join(traceback.format_exception(error.value))


def test_missing_url_with_explicit_enabled_fails_open(clean_env):
    cache = RedisStateCache(StateCacheSettings(enabled=True))
    with pytest.raises(CacheUnavailable, match="connection unavailable"):
        cache.get("kind", "scope")


def test_close_is_idempotent_and_prevents_reuse(cache):
    client = cache._client
    cache.close()
    cache.close()
    assert client.closed
    with pytest.raises(CacheUnavailable, match="closed"):
        cache.get("kind", "scope")


@pytest.fixture()
def real_cache():
    url = os.getenv("RAG_TEST_REDIS_URL", "").strip()
    if not url:
        pytest.skip("Set RAG_TEST_REDIS_URL to run Redis Lua integration tests")
    pytest.importorskip("redis")
    backend = RedisStateCache(StateCacheSettings(
        url=url, prefix=f"rag:test:state:{uuid.uuid4().hex}", ttl_seconds=1,
    ))
    try:
        yield backend
    finally:
        # Never flush a shared Redis; delete only this test's two exact keys.
        client = backend._connection()
        client.delete(*backend._keys("knowledge", "alpha"))
        backend.close()


def test_real_redis_atomic_decimal_revision_and_persistent_tombstone(real_cache):
    import time

    backend = real_cache
    revision = 2**63 - 2
    assert backend.put("knowledge", "alpha", revision, {"version": "initial"})
    assert backend.put("knowledge", "alpha", revision + 1, {"version": "new"})
    assert not backend.put("knowledge", "alpha", revision, {"version": "stale"})
    assert backend.get("knowledge", "alpha") == (True, {"version": "new"})
    assert backend.put("knowledge", "alpha", revision + 1, None, deleted=True)
    assert not backend.put("knowledge", "alpha", revision + 1, {"version": "resurrected"})
    payload_key, revision_key = backend._keys("knowledge", "alpha")
    assert backend._connection().ttl(revision_key) == -1
    assert backend._connection().ttl(payload_key) >= 0
    time.sleep(1.1)
    assert backend.get("knowledge", "alpha") == (False, None)
    assert not backend.put("knowledge", "alpha", revision, {})
    assert not backend.put("knowledge", "alpha", revision + 1, {})
    assert backend.put("knowledge", "alpha", revision + 2, {"recreated": True})
    assert backend.get("knowledge", "alpha") == (True, {"recreated": True})
