"""Immutable, tenant-scoped Redis search results; Redis TTL owns reclamation."""

from __future__ import annotations

import json
import math
import os
import time
from dataclasses import dataclass
from uuid import UUID, uuid4


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

    @property
    def transient(self):
        from .transient import TransientStore
        return TransientStore(self.state)

    def prepare(self, namespace, payload, *, result_id=None, created_at=None):
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
            raise ValueError("Search results must be a list")  # noqa: TRY004 - public validation contract
        response.update(result_id=result_id, created_at=created, expires_at=expires,
                        snapshot_available=True)
        envelope = {"namespace": namespace, "expires_at": expires,
                    "status": "succeeded", "stage": "completed", "payload": response}
        content = json.dumps(envelope, ensure_ascii=False, allow_nan=False, separators=(",", ":"))
        if len(content.encode("utf-8")) > self.settings.max_result_bytes:
            raise SnapshotUnavailable("Search result exceeds snapshot size limit")
        return json.loads(content)

    def save(self, namespace, payload, *, result_id=None, created_at=None, conn=None):
        if conn is not None:
            raise ValueError("Search results do not support SQL transactions")
        envelope = self.prepare(namespace, payload, result_id=result_id, created_at=created_at)
        result = envelope["payload"]
        try:
            stored = self.transient.put("search-result", result["result_id"], envelope,
                                        ttl_seconds=math.ceil(result["expires_at"] - self.clock()), nx=True)
        except RuntimeError:
            raise SnapshotUnavailable("Search result storage is temporarily unavailable") from None
        if not stored:
            raise SnapshotUnavailable("Search result ID already exists")
        return result

    def get(self, namespace, result_id, *, page=1, page_size=10):
        try:
            canonical_uuid(result_id)
        except (ValueError, TypeError, AttributeError):
            raise SnapshotUnavailable("Search result expired or unavailable") from None
        if type(page) is not int or page < 1 or type(page_size) is not int or not 1 <= page_size <= 100:
            raise ValueError("page must be positive and page_size must be 1..100")
        try:
            envelope = self.transient.get("search-result", result_id)
        except RuntimeError:
            raise SnapshotUnavailable("Search result expired or unavailable") from None
        try:
            result = envelope["payload"]
            expires = envelope["expires_at"]
            created = result["created_at"]
            valid = (envelope["namespace"] == namespace
                     and type(expires) in (int, float) and math.isfinite(expires)
                     and type(created) in (int, float) and math.isfinite(created)
                     and created < expires and expires > self.clock()
                     and result["expires_at"] == expires and result["result_id"] == result_id
                     and result["snapshot_available"] is True
                     and isinstance(result.get("results", []), list))
            if not valid or len(json.dumps(envelope, ensure_ascii=False, separators=(",", ":")).encode()) > self.settings.max_result_bytes:
                raise ValueError
        except (KeyError, TypeError, ValueError, UnicodeError, RecursionError):
            raise SnapshotUnavailable("Search result expired or unavailable") from None
        items = result.get("results", [])
        total = len(items)
        start = (page - 1) * page_size
        if start >= total and page != 1:
            raise ValueError("Result page does not exist")
        return {**result, "results": items[start:start + page_size], "page": page,
                "page_size": page_size, "total_results": total,
                "next_page": page + 1 if start + page_size < total else None}

    def purge_expired(self, *, limit=100, batch_size=None):
        size = limit if batch_size is None else batch_size
        if type(size) is not int or not 1 <= size <= 1000:
            raise ValueError("limit must be 1..1000")
        return 0
