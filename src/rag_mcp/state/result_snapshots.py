"""SQL-authoritative immutable search snapshots with best-effort Redis caching.

Only public search responses belong here, never private invocation arguments.
Snapshots have no dependency on live documents or their deletion status.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import time
from dataclasses import dataclass
from uuid import UUID, uuid4

from sqlalchemy import delete, insert, select
from sqlalchemy.exc import IntegrityError, SQLAlchemyError

from .schema import search_result_snapshots


class SnapshotUnavailable(ValueError):
    """Missing, expired, evicted or temporarily inaccessible reference."""


@dataclass(frozen=True)
class SnapshotSettings:
    ttl_seconds: int = 3600
    max_result_bytes: int = 16 * 1024 * 1024
    max_artifact_bytes: int = 64 * 1024 * 1024
    max_artifacts_per_search: int = 100

    @classmethod
    def from_env(cls):
        def positive(name, default):
            try:
                value = int(os.getenv(name, str(default)))
            except ValueError:
                raise ValueError(f"{name} must be a positive integer") from None
            if value <= 0:
                raise ValueError(f"{name} must be a positive integer")
            return value
        return cls(
            ttl_seconds=positive("RAG_RESULT_TTL_SECONDS", 3600),
            max_result_bytes=positive("RAG_RESULT_MAX_BYTES", 16 * 1024 * 1024),
            max_artifact_bytes=positive("RAG_ARTIFACT_MAX_BYTES", 64 * 1024 * 1024),
            max_artifacts_per_search=positive("RAG_RESULT_MAX_ARTIFACTS", 100))


def canonical_uuid(value):
    if not isinstance(value, str) or str(UUID(value)) != value:
        raise ValueError("Expected a canonical UUID")
    return value


class ResultSnapshots:
    def __init__(self, state, *, settings=None, clock=time.time):
        self.state = state
        self.settings = settings or SnapshotSettings.from_env()
        self.clock = clock

    def _backend(self):
        synchronizer = getattr(self.state, "cache_sync", None)
        backend = getattr(synchronizer, "backend", None)
        return backend if backend is not None and backend.enabled else None

    def _key(self, namespace, result_id):
        backend = self._backend()
        scope = hashlib.sha256(namespace.encode("utf-8")).hexdigest()
        return f"{backend.settings.prefix}:search-result:{scope}:{result_id}"

    def _cache(self, namespace, result_id, encoded, expires):
        try:
            backend = self._backend()
            ttl = math.ceil(expires - self.clock())
            if backend is not None and ttl > 0:
                backend._connection().set(self._key(namespace, result_id), encoded, ex=ttl, nx=False)
        except Exception:
            # Cache failures must neither prevent durable writes nor leak credentials.
            pass

    def _decode(self, raw, namespace, result_id):
        try:
            if len(raw.encode("utf-8") if isinstance(raw, str) else raw) > self.settings.max_result_bytes:
                return None
            envelope = json.loads(raw)
            result = envelope["payload"]
            expires = envelope["expires_at"]
            created = result["created_at"]
            if (envelope["namespace"] != namespace
                    or type(expires) not in (int, float) or not math.isfinite(expires)
                    or type(created) not in (int, float) or not math.isfinite(created)
                    or created >= expires or expires <= self.clock()
                    or result["expires_at"] != expires
                    or result["result_id"] != result_id
                    or result["snapshot_available"] is not True
                    or not isinstance(result.get("results", []), list)):
                return None
            return result
        except (KeyError, TypeError, ValueError, UnicodeError, RecursionError):
            return None

    def save(self, namespace, payload, *, result_id=None, created_at=None, conn=None):
        """Persist a detached public response before publishing its cache or UUID."""
        if not isinstance(namespace, str) or not 1 <= len(namespace) <= 64:
            raise ValueError("Invalid result namespace")
        result_id = str(uuid4()) if result_id is None else canonical_uuid(result_id)
        created = self.clock() if created_at is None else created_at
        if type(created) not in (int, float) or not math.isfinite(created):
            raise ValueError("Invalid result creation time")
        expires = created + self.settings.ttl_seconds
        if not math.isfinite(expires) or expires <= created:
            raise ValueError("Invalid result expiry time")
        if expires <= self.clock():
            raise SnapshotUnavailable("Search result has already expired")
        response = dict(payload)
        if not isinstance(response.get("results", []), list):
            raise ValueError("Search results must be a list")
        response.update(result_id=result_id, created_at=created, expires_at=expires,
                        snapshot_available=True)
        content = json.dumps({"namespace": namespace, "expires_at": expires, "payload": response},
                             ensure_ascii=False, allow_nan=False, separators=(",", ":"))
        encoded = content.encode("utf-8")
        if len(encoded) > self.settings.max_result_bytes:
            raise SnapshotUnavailable("Search result exceeds snapshot size limit")
        statement = insert(search_result_snapshots).values(
            id=result_id, namespace=namespace, content=content, created_at=created, expires_at=expires)
        try:
            if conn is not None:
                conn.execute(statement)
            else:
                with self.state.engine.begin() as connection:
                    connection.execute(statement)
        except IntegrityError:
            raise SnapshotUnavailable("Search result ID already exists") from None
        except SQLAlchemyError:
            raise SnapshotUnavailable("Search result storage is temporarily unavailable") from None
        if conn is None:
            self._cache(namespace, result_id, encoded, expires)
        return json.loads(content)["payload"]

    def cache_saved(self, namespace, result_id):
        """Publish only committed bytes; called after a task/result transaction commits."""
        with self.state.engine.connect() as conn:
            row = conn.execute(select(search_result_snapshots).where(
                search_result_snapshots.c.id == result_id,
                search_result_snapshots.c.namespace == namespace,
            )).mappings().first()
        if row is not None:
            self._cache(namespace, result_id, row["content"].encode("utf-8"), row["expires_at"])

    def get(self, namespace, result_id, *, page=1, page_size=10):
        """Read a frozen execution, never the live index or an inference model."""
        try:
            canonical_uuid(result_id)
        except (ValueError, TypeError, AttributeError):
            raise SnapshotUnavailable("Search result expired or unavailable") from None
        if type(page) is not int or page < 1 or type(page_size) is not int or not 1 <= page_size <= 100:
            raise ValueError("page must be positive and page_size must be 1..100")
        result = None
        try:
            backend = self._backend()
            if backend is not None:
                raw = backend._connection().get(self._key(namespace, result_id))
                result = self._decode(raw, namespace, result_id)
        except Exception:
            pass
        if result is None:
            try:
                with self.state.engine.connect() as conn:
                    row = conn.execute(select(search_result_snapshots).where(
                        search_result_snapshots.c.id == result_id,
                        search_result_snapshots.c.namespace == namespace,
                        search_result_snapshots.c.expires_at > self.clock(),
                    )).mappings().first()
            except SQLAlchemyError:
                raise SnapshotUnavailable("Search result expired or unavailable") from None
            if row is not None:
                result = self._decode(row["content"], namespace, result_id)
                if result is not None:
                    if (result["created_at"] != row["created_at"]
                            or result["expires_at"] != row["expires_at"]):
                        result = None
                    else:
                        self._cache(namespace, result_id, row["content"].encode("utf-8"),
                                    row["expires_at"])
        if result is None or result["expires_at"] <= self.clock():
            raise SnapshotUnavailable("Search result expired or unavailable")
        items = result.get("results", [])
        total = len(items)
        start = (page - 1) * page_size
        if start >= total and page != 1:
            raise ValueError("Result page does not exist")
        return {**result, "results": items[start:start + page_size], "page": page,
                "page_size": page_size, "total_results": total,
                "next_page": page + 1 if start + page_size < total else None}

    def purge_expired(self, *, limit=100, batch_size=None):
        """Delete one bounded SQL batch; ``batch_size`` is a compatibility alias.

        Redis keys expire independently, so cleanup never requires Redis.
        """
        size = limit if batch_size is None else batch_size
        if type(size) is not int or not 1 <= size <= 1000:
            raise ValueError("limit must be 1..1000")
        try:
            with self.state.engine.begin() as conn:
                ids = list(conn.execute(select(search_result_snapshots.c.id).where(
                    search_result_snapshots.c.expires_at <= self.clock()
                ).order_by(search_result_snapshots.c.expires_at, search_result_snapshots.c.id)
                    .limit(size)).scalars())
                if not ids:
                    return 0
                return conn.execute(delete(search_result_snapshots).where(
                    search_result_snapshots.c.id.in_(ids))).rowcount
        except SQLAlchemyError:
            raise SnapshotUnavailable("Search result storage is temporarily unavailable") from None
