"""Durable, namespace-scoped binary snapshots independent of source lifetime."""

from __future__ import annotations

import base64
import hashlib
import json
import math
import time
import uuid
from collections.abc import Callable, Mapping
from typing import TYPE_CHECKING
from urllib.parse import quote

from sqlalchemy import delete, func, select
from sqlalchemy.dialects.mysql import insert as mysql_insert
from sqlalchemy.dialects.sqlite import insert as sqlite_insert

from .schema import artifact_snapshots

if TYPE_CHECKING:
    from .store import StateStores


class ArtifactUnavailable(ValueError):
    """An artifact is missing, expired, or outside the requested namespace."""


class ArtifactRegistry:
    """Keep immutable bytes in SQL, never in the disposable Redis cache.

    Identity excludes MIME metadata and expiry. Re-registering a retained identity
    only extends its lifetime, preserving the original snapshot and creation time.
    After purge, the same identity recreates the same UUID with a new lifetime.
    ``max_bytes=None`` disables the per-artifact size ceiling.
    """

    def __init__(
        self,
        state: StateStores,
        ttl_seconds: float = 3600,
        clock: Callable[[], float] = time.time,
        *,
        max_bytes: int | None = 64 * 1024 * 1024,
    ) -> None:
        self.state = state
        self.ttl_seconds = _finite(ttl_seconds, "ttl_seconds")
        if self.ttl_seconds <= 0:
            raise ValueError("ttl_seconds must be positive")
        if max_bytes is not None and (type(max_bytes) is not int or max_bytes < 0):
            raise ValueError("max_bytes must be a nonnegative integer or None")
        self.max_bytes = max_bytes
        self.clock = clock

    def register(
        self,
        namespace: str,
        document_id: str,
        version: str | None,
        kind: str,
        name: str,
        mime_type: str,
        content: bytes,
        *,
        expires_at: float | None = None,
    ) -> dict:
        """Commit a snapshot and return public metadata without its binary data."""
        for label, value, maximum in (
            ("namespace", namespace, 64), ("document_id", document_id, 64),
            ("kind", kind, 32), ("name", name, 512), ("mime_type", mime_type, 128),
        ):
            _text(value, label, maximum)
        if version is not None:
            _text(version, "version", 64)
        if not isinstance(content, bytes):
            raise ValueError("content must be bytes")  # noqa: TRY004 - public validation uses ValueError.
        if self.max_bytes is not None and len(content) > self.max_bytes:
            raise ValueError("Artifact exceeds the per-artifact byte limit")
        now = _finite(self.clock(), "clock")
        expiry = _finite(now + self.ttl_seconds if expires_at is None else expires_at, "expires_at")
        identity = hashlib.sha256(json.dumps(
            [namespace, document_id, version, kind, name, hashlib.sha256(content).hexdigest()],
            ensure_ascii=False, separators=(",", ":"),
        ).encode("utf-8")).hexdigest()
        artifact_id = str(uuid.uuid5(uuid.NAMESPACE_URL, identity))
        values = {
            "id": artifact_id, "namespace": namespace, "identity": identity,
            "document_id": document_id, "version": version, "kind": kind,
            "name": name, "mime_type": mime_type, "content": content,
            "created_at": now, "expires_at": expiry,
        }
        dialect = self.state.engine.dialect.name
        if dialect == "sqlite":
            statement = sqlite_insert(artifact_snapshots).values(**values)
            statement = statement.on_conflict_do_update(
                index_elements=[artifact_snapshots.c.namespace, artifact_snapshots.c.identity],
                set_={"expires_at": func.max(artifact_snapshots.c.expires_at, statement.excluded.expires_at)},
            )
        elif dialect in {"mysql", "mariadb"}:
            statement = mysql_insert(artifact_snapshots).values(**values)
            statement = statement.on_duplicate_key_update(
                expires_at=func.greatest(artifact_snapshots.c.expires_at, statement.inserted.expires_at),
            )
        else:
            raise ValueError("Artifact snapshots require SQLite or MySQL")
        with self.state.engine.begin() as conn:
            conn.execute(statement)
            row = conn.execute(select(*_metadata_columns()).where(
                artifact_snapshots.c.namespace == namespace,
                artifact_snapshots.c.identity == identity,
            )).mappings().one()
            result = _public(row)
        return result

    def read(self, namespace: str, artifact_id: str) -> dict:
        """Read a canonical UUID without consulting documents, uploads, or cache."""
        if not isinstance(namespace, str) or not 1 <= len(namespace) <= 64:
            raise ArtifactUnavailable("Artifact is unavailable")
        try:
            if not isinstance(artifact_id, str) or str(uuid.UUID(artifact_id)) != artifact_id:
                raise ValueError
        except ValueError:
            raise ArtifactUnavailable("Artifact is unavailable") from None
        now = _finite(self.clock(), "clock")
        with self.state.engine.connect() as conn:
            row = conn.execute(select(artifact_snapshots).where(
                artifact_snapshots.c.namespace == namespace,
                artifact_snapshots.c.id == artifact_id,
                artifact_snapshots.c.expires_at > now,
            )).mappings().first()
        # MySQL PAD SPACE collations can match a namespace with trailing spaces.
        if row is None or row["namespace"] != namespace:
            raise ArtifactUnavailable("Artifact is unavailable")
        return {**_public(row), "content": bytes(row["content"])}

    def chunk(self, namespace: str, artifact_id: str, offset: int = 0, length: int = 65536) -> dict:
        """Return at most 1 MiB as base64; EOF is valid, offsets past EOF are not."""
        if type(offset) is not int or offset < 0:
            raise ValueError("offset must be a nonnegative integer")
        if type(length) is not int or not 1 <= length <= 1024 * 1024:
            raise ValueError("length must be an integer between 1 and 1048576")
        artifact = self.read(namespace, artifact_id)
        content = artifact.pop("content")
        if offset > len(content):
            raise ValueError("offset exceeds the artifact byte length")
        end = min(offset + length, len(content))
        return {
            **artifact, "encoding": "base64",
            "data": base64.b64encode(content[offset:end]).decode("ascii"),
            "total_bytes": len(content), "offset": offset,
            "next_offset": end if end < len(content) else None,
        }

    def purge_expired(self, limit: int = 100) -> int:
        """Delete a bounded batch, rechecking expiry after any concurrent renewal."""
        if type(limit) is not int or limit < 1:
            raise ValueError("limit must be a positive integer")
        now = _finite(self.clock(), "clock")
        with self.state.engine.connect() as conn:
            candidates = list(conn.execute(select(artifact_snapshots.c.id).where(
                artifact_snapshots.c.expires_at <= now,
            ).order_by(artifact_snapshots.c.expires_at, artifact_snapshots.c.id).limit(limit)).scalars())
        if not candidates:
            return 0
        # Separate selection from deletion so SQLite need not upgrade a stale
        # read transaction; the write predicate protects concurrently renewed IDs.
        with self.state.engine.begin() as conn:
            result = conn.execute(delete(artifact_snapshots).where(
                artifact_snapshots.c.id.in_(candidates), artifact_snapshots.c.expires_at <= now,
            ))
            return result.rowcount


def _metadata_columns() -> list:
    return [column for column in artifact_snapshots.c if column.name != "content"]


def _public(row: Mapping) -> dict:
    return {
        "artifact_id": row["id"], "document_id": row["document_id"],
        "version": row["version"], "kind": row["kind"], "name": row["name"],
        "mime_type": row["mime_type"], "expires_at": row["expires_at"],
        "resource_uri": f"rag://artifacts/{quote(row['namespace'], safe='')}/{row['id']}",
    }


def _text(value: str, label: str, maximum: int) -> None:
    if not isinstance(value, str) or not value or len(value) > maximum:
        raise ValueError(f"{label} must be a nonempty string of at most {maximum} characters")


def _finite(value: float, label: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        raise ValueError(f"{label} must be a finite number")
    return float(value)
