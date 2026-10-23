"""Optional synchronous Redis cache for already-redacted state snapshots.

MySQL remains authoritative; callers catch :class:`CacheUnavailable` and load
from the database. Payloads expire, but revision/deletion watermarks deliberately
never expire: memory use grows with the number of distinct cached scopes. Do not
evict watermarks independently of payloads or remove them while stale database
loads/outbox deliveries can still arrive. Use a dedicated, non-evicting Redis
instance when that guarantee is required. Redis loss/reset requires coordinated
repopulation from authoritative revisions, not replay of arbitrary old fills.

This module does not redact values: callers must never supply credentials.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
from dataclasses import dataclass, field
from typing import Any


class CacheUnavailable(RuntimeError):
    """A sanitized cache failure; callers must fall back to authoritative state."""


@dataclass(frozen=True)
class StateCacheSettings:
    """Redis is disabled without a URL, unless a client is explicitly injected."""

    url: str | None = field(default=None, repr=False)
    prefix: str = "rag:state:v1"
    ttl_seconds: int = 60
    timeout_seconds: float = 0.3
    enabled: bool | None = None

    def __post_init__(self) -> None:
        if type(self.ttl_seconds) is not int or self.ttl_seconds <= 0:
            raise ValueError("RAG_REDIS_TTL_SECONDS must be a positive integer")
        if not isinstance(self.timeout_seconds, (int, float)) or isinstance(self.timeout_seconds, bool):
            raise ValueError("RAG_REDIS_TIMEOUT_SECONDS must be a positive finite number")
        if not math.isfinite(self.timeout_seconds) or self.timeout_seconds <= 0:
            raise ValueError("RAG_REDIS_TIMEOUT_SECONDS must be a positive finite number")
        if not isinstance(self.prefix, str) or not self.prefix:
            raise ValueError("RAG_REDIS_PREFIX must not be empty")
        if self.enabled is not None and type(self.enabled) is not bool:
            raise ValueError("RAG_REDIS_ENABLED must be a boolean")

    @classmethod
    def from_env(cls) -> StateCacheSettings:
        raw_enabled = os.getenv("RAG_REDIS_ENABLED", "").strip().lower()
        enabled = None
        if raw_enabled:
            if raw_enabled not in {"1", "true", "yes", "on", "0", "false", "no", "off"}:
                raise ValueError("RAG_REDIS_ENABLED must be a boolean")
            enabled = raw_enabled in {"1", "true", "yes", "on"}
        try:
            ttl = int(os.getenv("RAG_REDIS_TTL_SECONDS", "60").strip())
        except ValueError:
            raise ValueError("RAG_REDIS_TTL_SECONDS must be a positive integer") from None
        try:
            timeout = float(os.getenv("RAG_REDIS_TIMEOUT_SECONDS", "0.3").strip())
        except ValueError:
            raise ValueError("RAG_REDIS_TIMEOUT_SECONDS must be a positive finite number") from None
        return cls(
            url=os.getenv("RAG_REDIS_URL", "").strip() or None,
            prefix=os.getenv("RAG_REDIS_PREFIX", "rag:state:v1").strip(),
            ttl_seconds=ttl,
            timeout_seconds=timeout,
            enabled=enabled,
        )


CacheSettings = StateCacheSettings


# Decimal strings retain all BIGINT precision; Lua numbers cannot represent every
# revision above 2**53. Both keys use the same Redis Cluster hash slot.
_PUT_SCRIPT = """
local current = redis.call('GET', KEYS[2])
local revision = ARGV[1]
local deleted = ARGV[2]
if current then
    local old_revision, old_deleted = string.match(current, '^(%d+):([01])$')
    if not old_revision then
        return redis.error_reply('Invalid state cache watermark')
    end
    if #old_revision > #revision or
       (#old_revision == #revision and old_revision > revision) then
        return 0
    end
    if old_revision == revision and old_deleted == '1' and deleted == '0' then
        return 0
    end
end
redis.call('SET', KEYS[1], ARGV[3], 'EX', ARGV[4])
redis.call('SET', KEYS[2], revision .. ':' .. deleted)
return 1
"""


class RedisStateCache:
    """Redis-only backend; revision allocation and database transactions live elsewhere.

    Construction is lazy so unreachable Redis cannot prevent database startup.
    An injected redis-py compatible client is supported for tests and is closed by
    ``close()``. Explicit ``enabled=False`` overrides even an injected client.
    """

    def __init__(self, settings: StateCacheSettings | None = None, *, client: Any = None) -> None:
        self.settings = settings if settings is not None else StateCacheSettings.from_env()
        self.enabled = (
            self.settings.enabled
            if self.settings.enabled is not None
            else bool(self.settings.url or client is not None)
        )
        self._client = client
        self._closed = False

    def _connection(self) -> Any:
        if self._closed:
            raise CacheUnavailable("State cache is closed")
        if self._client is None:
            try:
                import redis

                if not self.settings.url:
                    raise ValueError("Missing Redis URL")
                self._client = redis.Redis.from_url(
                    self.settings.url,
                    socket_timeout=self.settings.timeout_seconds,
                    socket_connect_timeout=self.settings.timeout_seconds,
                    decode_responses=False,
                )
            except Exception:
                raise CacheUnavailable("State cache connection unavailable") from None
        return self._client

    def _keys(self, kind: str, scope: str) -> tuple[str, str]:
        if not isinstance(kind, str) or not isinstance(scope, str):
            raise TypeError("Cache kind and scope must be strings")
        identity = json.dumps([kind, scope], ensure_ascii=True, separators=(",", ":"))
        digest = hashlib.sha256(identity.encode("utf-8")).hexdigest()
        base = f"{self.settings.prefix}:{{{digest}}}"
        return f"{base}:payload", f"{base}:revision"

    def get(self, kind: str, scope: str) -> tuple[bool, Any]:
        """Return ``(False, None)`` on miss; cached deletions are ``(True, None)``."""
        if not self.enabled:
            return False, None
        keys = self._keys(kind, scope)
        try:
            # MGET observes payload and persistent watermark in one atomic read.
            payload, watermark = self._connection().mget(keys)
            if payload is None:
                return False, None
            envelope = json.loads(payload)
            if not isinstance(envelope, dict) or envelope.get("schema") != 1:
                raise ValueError("Invalid envelope")
            revision = envelope["revision"]
            deleted = envelope["deleted"]
            if not isinstance(revision, str) or not revision.isascii() or not revision.isdecimal():
                raise ValueError("Invalid revision")
            if revision != str(int(revision)) or type(deleted) is not bool or "value" not in envelope:
                raise ValueError("Invalid envelope")
            expected = f"{revision}:{int(deleted)}"
            if isinstance(watermark, bytes):
                watermark = watermark.decode("ascii")
            if watermark != expected:
                return False, None
            return True, None if deleted else envelope["value"]
        except CacheUnavailable:
            raise
        except Exception:
            raise CacheUnavailable("State cache read unavailable") from None

    def put(self, kind: str, scope: str, revision: int, value: Any, deleted: bool = False) -> bool:
        """Atomically publish a snapshot unless a newer revision already exists.

        Equal revisions can refill expired payloads, but cannot undo a deletion.
        Deletion values are discarded rather than accidentally retaining secrets.
        """
        if not self.enabled:
            return False
        if type(revision) is not int or revision < 0:
            raise ValueError("Cache revision must be a nonnegative integer")
        if type(deleted) is not bool:
            raise ValueError("Cache deletion marker must be a boolean")
        keys = self._keys(kind, scope)
        try:
            payload = json.dumps(
                {"schema": 1, "revision": str(revision), "deleted": deleted,
                 "value": None if deleted else value},
                ensure_ascii=True, separators=(",", ":"), allow_nan=False,
            )
            result = self._connection().eval(
                _PUT_SCRIPT, 2, *keys, str(revision), str(int(deleted)),
                payload, str(self.settings.ttl_seconds),
            )
            return bool(result)
        except CacheUnavailable:
            raise
        except Exception:
            raise CacheUnavailable("State cache write unavailable") from None

    def close(self) -> None:
        """Release the Redis client without masking database shutdown failures."""
        if self._closed:
            return
        self._closed = True
        if self._client is not None:
            try:
                self._client.close()
            except Exception:
                raise CacheUnavailable("State cache close unavailable") from None
