"""Transactional cache revisions: no Redis/network effects before SQL commit."""

from __future__ import annotations

import time
from uuid import uuid4

from sqlalchemy import insert, select, update
from sqlalchemy.dialects.mysql import insert as mysql_insert
from sqlalchemy.dialects.sqlite import insert as sqlite_insert

from .schema import cache_outbox, cache_revisions

KINDS = frozenset({
    "kb_public", "kb_list", "documents", "catalog", "api_keys", "heads",
    "leases", "upload_status", "upload_list", "document_status",
})


def revision(conn, kind: str, scope: str) -> int:
    value = conn.scalar(select(cache_revisions.c.revision).where(
        cache_revisions.c.kind == kind, cache_revisions.c.scope == scope))
    return int(value or 0)


def touch(conn, *keys: tuple[str, str]) -> list[str]:
    """Bump read-model versions and append events in the caller's transaction.

    Sorted resource acquisition prevents cross-kind lock inversion. The revision
    row is also the snapshot latch used for consistent miss loads and refreshes.
    """
    event_ids = []
    for kind, scope in sorted(set(keys)):
        if kind not in KINDS:
            raise ValueError("Unknown state cache kind")
        values = dict(kind=kind, scope=scope, revision=0)
        if conn.dialect.name == "mysql":
            stmt = mysql_insert(cache_revisions).values(**values)
            stmt = stmt.on_duplicate_key_update(revision=cache_revisions.c.revision)
        else:
            stmt = sqlite_insert(cache_revisions).values(**values).on_conflict_do_nothing()
        conn.execute(stmt)
        conn.execute(update(cache_revisions).where(
            cache_revisions.c.kind == kind, cache_revisions.c.scope == scope
        ).values(revision=cache_revisions.c.revision + 1))
        value = revision(conn, kind, scope)
        event_id = uuid4().hex
        conn.execute(insert(cache_outbox).values(
            id=event_id, kind=kind, scope=scope, revision=value, status="PENDING",
            attempts=0, retry_at=0, created_at=time.time()))
        event_ids.append(event_id)
    return event_ids


def knowledge_keys(namespace: str):
    return (("kb_list", ""), ("kb_public", namespace), ("documents", namespace))


def document_keys(namespace: str):
    return (("kb_list", ""), ("documents", namespace))
