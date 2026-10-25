"""Required Redis storage for expiring queue state; never falls back to SQL."""

from __future__ import annotations

import hashlib
import json
import math
from uuid import uuid4

from redis.exceptions import WatchError


class TransientUnavailable(RuntimeError):
    """Transient task state is unavailable; no credentials or request data exposed."""


class TransientStore:
    def __init__(self, state):
        self.state = state

    def _connection(self):
        backend = getattr(getattr(self.state, "cache_sync", None), "backend", None)
        if backend is None or not backend.enabled:
            raise TransientUnavailable("Redis is required for queued operations")
        try:
            return backend._connection()
        except Exception:  # noqa: BLE001 - Redis failures must not expose credentials
            raise TransientUnavailable("Redis task storage unavailable") from None

    def _key(self, kind, identity):
        if not isinstance(kind, str) or not isinstance(identity, str):
            raise TypeError("Redis task identity must be a string")
        backend = self.state.cache_sync.backend
        digest = hashlib.sha256(json.dumps([kind, identity], separators=(",", ":")).encode()).hexdigest()
        return f"{backend.settings.prefix}:transient:{{{digest}}}"

    @staticmethod
    def _ttl(value):
        from .result_snapshots import SnapshotSettings

        ttl = SnapshotSettings.from_env().ttl_seconds if value is None else value
        if type(ttl) not in (int, float) or not math.isfinite(ttl) or ttl <= 0:
            raise ValueError("Redis task TTL must be positive")
        return max(1, math.ceil(ttl))

    @staticmethod
    def _encode(value):
        if not isinstance(value, dict):
            raise TypeError("Redis task state must be an object")
        return json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(",", ":"))

    @staticmethod
    def _decode(raw):
        if raw is None:
            return None
        value = json.loads(raw)
        if not isinstance(value, dict):
            raise TypeError("Invalid Redis task state")
        return value

    def get(self, kind, identity):
        connection = self._connection()
        try:
            return self._decode(connection.get(self._key(kind, identity)))
        except Exception:  # noqa: BLE001 - sanitize backend and corrupt-state failures
            raise TransientUnavailable("Redis task read unavailable") from None

    def put(self, kind, identity, value, ttl_seconds=None, nx=False):
        connection = self._connection()
        encoded, ttl = self._encode(value), self._ttl(ttl_seconds)
        try:
            return bool(connection.set(self._key(kind, identity), encoded, ex=ttl, nx=nx))
        except Exception:  # noqa: BLE001 - sanitize backend failures
            raise TransientUnavailable("Redis task write unavailable") from None

    def delete(self, kind, identity):
        connection = self._connection()
        try:
            return connection.delete(self._key(kind, identity))
        except Exception:  # noqa: BLE001 - sanitize backend failures
            raise TransientUnavailable("Redis task delete unavailable") from None

    def update(self, kind, identity, callback, ttl_seconds=None):
        connection = self._connection()
        key, ttl = self._key(kind, identity), self._ttl(ttl_seconds)
        for _ in range(32):
            callback_error = None
            try:
                with connection.pipeline() as transaction:
                    transaction.watch(key)
                    current = self._decode(transaction.get(key))
                    try:
                        updated = callback(current)
                    except Exception as exc:
                        callback_error = exc
                        raise
                    if updated is None:
                        transaction.unwatch()
                        return None
                    encoded = self._encode(updated)
                    transaction.multi()
                    transaction.set(key, encoded, ex=ttl)
                    transaction.execute()
                    return updated
            except WatchError:
                continue
            except Exception:  # noqa: BLE001 - preserve domain errors, sanitize backend errors
                if callback_error is not None:
                    raise callback_error
                raise TransientUnavailable("Redis task update unavailable") from None
        raise TransientUnavailable("Redis task update contention")

    def lock(self, kind, identity, ttl_seconds):
        connection = self._connection()
        token = uuid4().hex
        try:
            acquired = connection.set(self._key("lock:" + kind, identity), token,
                                      ex=self._ttl(ttl_seconds), nx=True)
            return token if acquired else None
        except Exception:  # noqa: BLE001 - sanitize backend failures
            raise TransientUnavailable("Redis task lock unavailable") from None

    def unlock(self, kind, identity, token):
        connection = self._connection()
        key = self._key("lock:" + kind, identity)
        for _ in range(32):
            try:
                with connection.pipeline() as transaction:
                    transaction.watch(key)
                    current = transaction.get(key)
                    if isinstance(current, bytes):
                        current = current.decode("ascii")
                    if current != token:
                        transaction.unwatch()
                        return False
                    transaction.multi()
                    transaction.delete(key)
                    transaction.execute()
                    return True
            except WatchError:
                continue
            except Exception:  # noqa: BLE001 - sanitize backend failures
                raise TransientUnavailable("Redis task unlock unavailable") from None
        raise TransientUnavailable("Redis task unlock contention")
