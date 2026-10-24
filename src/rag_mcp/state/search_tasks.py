"""SQL-authoritative search outcomes and an initial-delivery transactional outbox.

Private invocation arguments never enter broker messages or public status caches.
"""

from __future__ import annotations

import json
import logging
import math
import os
from contextlib import contextmanager
from dataclasses import dataclass
from uuid import uuid4

from sqlalchemy import case, insert, select, text, update

from .result_snapshots import SnapshotUnavailable, canonical_uuid
from .schema import search_task_outbox as outbox
from .schema import search_tasks as tasks

logger = logging.getLogger(__name__)

_ACTIVE = ("queued", "processing")
_UNAVAILABLE = "Search result expired or unavailable"


@dataclass(frozen=True)
class SearchTaskSettings:
    timeout_seconds: float = 120

    def __post_init__(self):
        value = self.timeout_seconds
        if type(value) not in (int, float) or not math.isfinite(value) or value <= 0:
            raise ValueError("Search task timeout_seconds must be finite and positive")

    @classmethod
    def from_env(cls):
        return cls(timeout_seconds=float(os.getenv("RAG_SEARCH_TIMEOUT_SECONDS", "120")))


def validate_result_id(value):
    try:
        return canonical_uuid(value)
    except (ValueError, TypeError, AttributeError):
        raise ValueError("result_id must be a canonical UUID") from None


class SearchTasks:
    def __init__(self, state, settings=None):
        self.state = state
        self.engine = state.engine
        self.settings = settings or SearchTaskSettings.from_env()

    @staticmethod
    def _clock(conn):
        if conn.dialect.name == "mysql":
            return text("UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6))")
        return text("((julianday('now') - 2440587.5) * 86400.0)")

    def _now(self, conn):
        return float(conn.scalar(select(self._clock(conn))))

    def _row(self, conn, result_id, *, arguments=False):
        columns = [c for c in tasks.c if arguments or c.name != "arguments"]
        row = conn.execute(select(*columns).where(tasks.c.result_id == result_id)
                           .with_for_update()).mappings().first()
        return dict(row) if row else None

    @contextmanager
    def _transaction(self):
        with self.engine.begin() as conn:
            # SQLite ignores FOR UPDATE. Acquire its writer before any read so
            # competing completions cannot both attempt the unique snapshot insert.
            if conn.dialect.name == "sqlite":
                conn.exec_driver_sql("BEGIN IMMEDIATE")
            yield conn

    def _finish(self, conn, row, status, now, *, stage="failed"):
        expired = tasks.c.deadline <= self._clock(conn)
        changed = conn.execute(update(tasks).where(
            tasks.c.result_id == row["result_id"], tasks.c.status.in_(_ACTIVE),
        ).values(
            status=case((expired, "timed_out"), else_=status),
            stage=case((expired, "failed"), else_=stage), arguments="{}", updated_at=now))
        return changed.rowcount == 1

    def submit(self, namespace, arguments_dict):
        if not isinstance(namespace, str) or not 1 <= len(namespace) <= 64:
            raise ValueError("Invalid search namespace")
        if not isinstance(arguments_dict, dict):
            raise TypeError("Search arguments must be a dictionary")
        arguments = json.dumps(arguments_dict, ensure_ascii=False, allow_nan=False,
                               separators=(",", ":"))
        if len(arguments.encode("utf-8")) > 1024 * 1024:
            raise ValueError("Search arguments exceed the size limit")
        result_id = str(uuid4())
        with self.engine.begin() as conn:
            now = self._now(conn)
            conn.execute(insert(tasks).values(
                result_id=result_id, namespace=namespace, arguments=arguments,
                status="queued", stage="queued", attempts=0, owner=None, lease_until=0,
                retry_at=now, deadline=now + self.settings.timeout_seconds,
                created_at=now, updated_at=now))
            conn.execute(insert(outbox).values(
                result_id=result_id, pending=True, generation=1, retry_at=now))
        return result_id

    def get(self, namespace, result_id, page=1, page_size=10):
        try:
            validate_result_id(result_id)
        except ValueError:
            raise SnapshotUnavailable(_UNAVAILABLE) from None
        with self._transaction() as conn:
            # Namespace is part of the lookup: foreign jobs are indistinguishable
            # from unknown IDs, including when callers fall back to old snapshots.
            row = conn.execute(select(tasks.c.result_id, tasks.c.status, tasks.c.stage,
                                      tasks.c.deadline).where(
                tasks.c.result_id == result_id, tasks.c.namespace == namespace)
                .with_for_update()).mappings().first()
            if row is None:
                return None
            status = row["status"]
            if status in _ACTIVE and row["deadline"] <= self._now(conn):
                self._finish(conn, row, "timed_out", self._now(conn))
                status = "timed_out"
        if status == "succeeded":
            return self.state.results.get(namespace, result_id, page=page, page_size=page_size)
        if status not in _ACTIVE:
            message = ("Search timed out before completion; submit a new search"
                       if status == "timed_out" else "Search failed; submit a new search")
            raise SnapshotUnavailable(message)
        return {"result_id": result_id, "status": "queued", "stage": row["stage"]}

    def claim(self, result_id):
        """Load active work, without exclusive ownership or SQL retry admission."""
        validate_result_id(result_id)
        with self._transaction() as conn:
            row = self._row(conn, result_id, arguments=True)
            if row is None or row["status"] not in _ACTIVE:
                return None
            now = self._now(conn)
            if row["deadline"] <= now:
                self._finish(conn, row, "timed_out", now)
                return None
            row.update(status="processing", stage="processing", updated_at=now)
            changed = conn.execute(update(tasks).where(
                tasks.c.result_id == result_id, tasks.c.status.in_(_ACTIVE),
                tasks.c.deadline > self._clock(conn)).values(
                    status="processing", stage="processing", updated_at=now))
            if changed.rowcount != 1:
                self._finish(conn, row, "timed_out", self._now(conn))
                return None
            row["arguments"] = json.loads(row["arguments"])
            row["remaining_seconds"] = row["deadline"] - now
        return row

    def complete(self, task, payload):
        """Publish once; duplicates and other terminal winners are harmless no-ops."""
        with self._transaction() as conn:
            row = self._row(conn, task["result_id"])
            if row is None or row["status"] not in _ACTIVE:
                return None
            now = self._now(conn)
            if row["deadline"] <= now:
                self._finish(conn, row, "timed_out", now)
                return None
            # Retain the task lock while saving. A savepoint lets deadline expiry
            # discard the inserted snapshot while committing the timeout outcome.
            with conn.begin_nested() as snapshot:
                public = self.state.results.save(
                    row["namespace"], payload, result_id=row["result_id"], conn=conn)
                clock = self._clock(conn)
                changed = conn.execute(update(tasks).where(
                    tasks.c.result_id == row["result_id"], tasks.c.status.in_(_ACTIVE),
                    tasks.c.deadline > clock).values(
                        status="succeeded", stage="completed", arguments="{}", updated_at=clock))
                if changed.rowcount != 1:
                    snapshot.rollback()
                    public = None
            if public is None:
                self._finish(conn, row, "timed_out", self._now(conn))
                return None
        try:
            self.state.results.cache_saved(row["namespace"], row["result_id"])
        except Exception as exc:  # noqa: BLE001 - cache failures cannot undo a committed snapshot
            logger.warning("Search snapshot cache unavailable (%s)", type(exc).__name__)
        return public

    def fail(self, task, retry=True, error=None):
        # Provider error text may contain credentials/queries and is never stored.
        with self._transaction() as conn:
            row = self._row(conn, task["result_id"])
            if row is None or row["status"] not in _ACTIVE:
                return
            now = self._now(conn)
            if row["deadline"] <= now:
                self._finish(conn, row, "timed_out", now)
            elif not retry:
                self._finish(conn, row, "failed", now)
            else:
                changed = conn.execute(update(tasks).where(
                    tasks.c.result_id == row["result_id"], tasks.c.status.in_(_ACTIVE),
                    tasks.c.deadline > self._clock(conn)).values(
                        status="queued", stage="retry_wait", updated_at=now))
                if changed.rowcount != 1:
                    self._finish(conn, row, "timed_out", self._now(conn))

    def dead_letter(self, result_id):
        """Persist broker exhaustion before ACK; SQL errors deliberately propagate."""
        validate_result_id(result_id)
        with self._transaction() as conn:
            row = self._row(conn, result_id)
            if row is None:
                logger.info("Search dead letter references unknown result_id=%s", result_id)
                return True
            if row["status"] in _ACTIVE:
                now = self._now(conn)
                if row["deadline"] <= now:
                    self._finish(conn, row, "timed_out", now)
                else:
                    self._finish(conn, row, "failed", now, stage="dead_letter")
        return True

    def terminal(self, result_id):
        validate_result_id(result_id)
        with self._transaction() as conn:
            row = self._row(conn, result_id)
            if row is None or row["status"] not in _ACTIVE:
                return True
            now = self._now(conn)
            if row["deadline"] <= now:
                self._finish(conn, row, "timed_out", now)
                return True
        return False

    def expire(self, limit=20):
        with self._transaction() as conn:
            now = self._now(conn)
            rows = conn.execute(select(tasks.c.result_id).where(
                tasks.c.status.in_(_ACTIVE), tasks.c.deadline <= now)
                .order_by(tasks.c.deadline).limit(limit)
                .with_for_update(skip_locked=True)).mappings().all()
            for row in rows:
                self._finish(conn, row, "timed_out", now)
        return len(rows)

    def dispatch(self, result_id, transport):
        if transport is None:
            return
        with self.engine.connect() as conn:
            now = self._now(conn)
            delivery = conn.execute(select(outbox.c.generation).where(
                outbox.c.result_id == result_id, outbox.c.pending.is_(True),
                outbox.c.retry_at <= now)).mappings().first()
        if delivery is None:
            return
        transport.send(result_id)
        with self.engine.begin() as conn:
            conn.execute(update(outbox).where(
                outbox.c.result_id == result_id, outbox.c.generation == delivery["generation"]
            ).values(pending=False))

    def dispatch_pending(self, transport, limit=20):
        if transport is None:
            return 0
        with self.engine.connect() as conn:
            now = self._now(conn)
            ids = list(conn.scalars(select(outbox.c.result_id).where(
                outbox.c.pending.is_(True), outbox.c.retry_at <= now)
                .order_by(outbox.c.retry_at, outbox.c.result_id).limit(limit)))
        sent = 0
        for result_id in ids:
            try:
                self.dispatch(result_id, transport)
                sent += 1
            except Exception as exc:  # noqa: BLE001 - unconfirmed deliveries remain durable
                logger.warning("Search publication deferred (%s)", type(exc).__name__)
                break
        return sent
