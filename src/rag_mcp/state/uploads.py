"""Durable, fenced uploads; callbacks must be idempotent and version-scoped.

Public API: UploadJournal(state, lease_seconds=None, auto_heartbeat=True),
begin(namespace, name, document_id, collection, idempotency_key=None,
fingerprint=None), get(id), unfinished(namespace=None), claim_recovery(id),
assert_owner(op, conn=None), heartbeat(op), run_step(op, name, action,
details=None), fail(op), rollback(op, cleanup), commit(op, result, conn),
cleanup(op, cleanup_old), active_versions(namespace), old_version(op),
put_object(op, bucket, key, content, conn=None, metadata=None),
get_object(version, bucket, key), lock(namespace, document_id=None).

rollback's callback receives the operation (including collection/document_id/version)
and removes that version from Qdrant; objects are deleted only after it succeeds.
cleanup's callback receives the committed operation and removes only old_version.
Callbacks must not delete by document_id alone. Step details and results must contain
only JSON-safe references, never credentials. Exceptions persist type names only.
commit MUST run inside the caller's engine.begin() documents/blob transaction.
"""

from __future__ import annotations

import hashlib
import json
import logging
import math
import os
import socket
import threading
from contextlib import contextmanager
from uuid import uuid4

from sqlalchemy import delete, insert, select, text, update
from sqlalchemy.dialects.mysql import insert as mysql_insert
from sqlalchemy.dialects.sqlite import insert as sqlite_insert
from sqlalchemy.exc import IntegrityError

from . import cache_events
from .schema import (
    document_heads as heads,
    document_leases as leases,
    upload_objects as objects,
    upload_operations as operations,
    upload_steps as steps,
)


class UploadConflict(RuntimeError):
    """Another upload/lock, immutable artifact, or idempotency key conflicts."""


class LeaseLost(UploadConflict):
    """The worker is no longer authorized to mutate this upload."""


logger = logging.getLogger(__name__)

_ACTIVE = ("INTENT", "RUNNING", "FAILED", "ROLLING_BACK", "ROLLBACK_FAILED")
_GUARD = "namespace:guard"
_NAMESPACE = "namespace:exclusive"


def _resource(document_id):
    return "document:" + hashlib.sha256(document_id.encode()).hexdigest()


def _name_resource(name):
    return "name:" + hashlib.sha256(name.encode()).hexdigest()


def _json(value):
    return json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(",", ":"))


class _Heartbeat:
    def __init__(self, renew, interval):
        self._renew = renew
        self._interval = interval
        self._stop = threading.Event()
        self.error = None
        self._thread = threading.Thread(target=self._run, daemon=True, name="upload-lease")
        self._thread.start()

    def _run(self):
        while not self._stop.wait(self._interval):
            try:
                self._renew()
            except Exception as exc:
                self.error = exc
                return

    def stop(self):
        # Never join while the caller holds a database transaction: renewal may
        # already be waiting for that transaction's row lock.
        self._stop.set()


class UploadJournal:
    def __init__(self, state, *, lease_seconds=None, auto_heartbeat=True,
                 cleanup_delay_seconds=None):
        self.engine = state.engine
        self.cache_sync = getattr(state, "cache_sync", None)
        self.lease_seconds = float(
            lease_seconds if lease_seconds is not None
            else os.environ.get("RAG_UPLOAD_LEASE_SECONDS", "120")
        )
        if not math.isfinite(self.lease_seconds) or self.lease_seconds <= 0:
            raise ValueError("lease_seconds must be finite and positive")
        self.cleanup_delay_seconds = float(
            cleanup_delay_seconds if cleanup_delay_seconds is not None
            else os.environ.get("RAG_UPLOAD_CLEANUP_DELAY_SECONDS", "3600"))
        if not math.isfinite(self.cleanup_delay_seconds) or self.cleanup_delay_seconds < 0:
            raise ValueError("cleanup_delay_seconds must be finite and nonnegative")
        self.auto_heartbeat = auto_heartbeat
        self._heartbeats = {}
        self._mutex = threading.Lock()

    @staticmethod
    def _cache_keys(namespace, operation_id=None, *, heads=False, leases=False):
        keys = []
        if operation_id is not None:
            keys.extend((("upload_status", operation_id), ("upload_list", namespace)))
        if heads:
            keys.append(("heads", namespace))
        if leases:
            keys.append(("leases", namespace))
        return tuple(sorted(set(keys)))

    def _cache_changed(self, conn, namespace, operation_id=None, *, heads=False, leases=False):
        """Touch sorted display keys under the namespace guard; no cache transport I/O."""
        return cache_events.touch(conn, *self._cache_keys(
            namespace, operation_id, heads=heads, leases=leases))

    def _flush_cache(self, namespace, operation_id=None, *, heads=False, leases=False):
        """Refresh display views only after the owning SQL transaction has committed."""
        if self.cache_sync is not None:
            self.cache_sync.flush_pending(keys=self._cache_keys(
                namespace, operation_id, heads=heads, leases=leases))

    def _now(self, conn):
        if conn.dialect.name == "mysql":
            return float(conn.scalar(text("SELECT UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6))")))
        return float(conn.scalar(text("SELECT (julianday('now') - 2440587.5) * 86400.0")))

    def _insert_ignore(self, conn, table, values):
        if conn.dialect.name == "mysql":
            statement = mysql_insert(table).values(**values)
            column = next(iter(table.primary_key.columns))
            statement = statement.on_duplicate_key_update({column.name: column})
        else:
            statement = sqlite_insert(table).values(**values).on_conflict_do_nothing()
        conn.execute(statement)

    def _guard(self, conn, namespace):
        # This persistent row serializes namespace/document lock acquisition. An
        # INSERT also obtains SQLite's writer lock before any ownership reads.
        self._insert_ignore(conn, leases, dict(namespace=namespace, resource=_GUARD,
                                              owner="", operation_id=None, expires_at=0))
        conn.execute(select(leases).where(
            leases.c.namespace == namespace, leases.c.resource == _GUARD
        ).with_for_update()).first()

    def _row(self, conn, operation_id, *, locked=False):
        query = select(operations).where(operations.c.id == operation_id)
        row = conn.execute(query.with_for_update() if locked else query).mappings().first()
        if row is None:
            raise LookupError(operation_id)
        return dict(row)

    def _decode(self, row):
        row = dict(row)
        row["version"] = row["id"]
        row["result"] = json.loads(row["result"]) if row["result"] is not None else None
        return row

    def get(self, operation_id):
        with self.engine.connect() as conn:
            row = self._decode(self._row(conn, operation_id))
            row["steps"] = [dict(item) for item in conn.execute(select(steps).where(
                steps.c.operation_id == operation_id
            ).order_by(steps.c.started_at, steps.c.name)).mappings()]
            for step in row["steps"]:
                step["details"] = json.loads(step["details"])
            return row

    def unfinished(self, namespace=None):
        query = select(operations).where(
            operations.c.status.in_(_ACTIVE) | operations.c.cleanup_pending.is_(True)
        ).order_by(operations.c.created_at)
        if namespace is not None:
            query = query.where(operations.c.namespace == namespace)
        with self.engine.connect() as conn:
            return [self._decode(row) for row in conn.execute(query).mappings()]

    def _lease_rows(self, conn, namespace):
        return list(conn.execute(select(leases).where(
            leases.c.namespace == namespace, leases.c.resource != _GUARD
        ).with_for_update()).mappings())

    def _acquire(self, conn, namespace, resources, owner, operation_id=None):
        now = self._now(conn)
        for row in self._lease_rows(conn, namespace):
            overlaps = (_NAMESPACE in resources or row["resource"] == _NAMESPACE
                        or row["resource"] in resources)
            if overlaps and row["expires_at"] > now and row["owner"] != owner:
                raise UploadConflict("Upload resource is leased")
        self._cache_changed(conn, namespace, operation_id, leases=True)
        for resource in resources:
            values = dict(namespace=namespace, resource=resource, owner=owner,
                          operation_id=operation_id, expires_at=now + self.lease_seconds)
            self._insert_ignore(conn, leases, values)
            conn.execute(update(leases).where(
                leases.c.namespace == namespace, leases.c.resource == resource
            ).values(**values))

    def begin(self, namespace, name, document_id, collection, idempotency_key=None,
              fingerprint=None):
        """Persist INTENT and lease atomically; committed idempotency keys replay."""
        operation_id, owner = uuid4().hex, uuid4().hex
        try:
            with self.engine.begin() as conn:
                self._guard(conn, namespace)
                if idempotency_key is not None:
                    existing = conn.execute(select(operations).where(
                        operations.c.namespace == namespace,
                        operations.c.idempotency_key == idempotency_key
                    ).with_for_update()).mappings().first()
                    if existing is not None:
                        if (fingerprint is not None and fingerprint != existing["fingerprint"]
                                or existing["name"] != name
                                or existing["document_id"] != document_id
                                or existing["collection"] != collection):
                            raise UploadConflict("Idempotency key has different input")
                        if existing["status"] == "COMMITTED":
                            return self._decode(existing)
                        raise UploadConflict("Idempotency key is unfinished; recover its operation")
                unresolved = conn.execute(select(operations.c.id).where(
                    operations.c.namespace == namespace, operations.c.status.in_(_ACTIVE),
                    (operations.c.document_id == document_id) | (operations.c.name == name)
                )).first()
                if unresolved is not None:
                    raise UploadConflict("Previous upload must be recovered before another intent")
                self._acquire(conn, namespace, [_resource(document_id), _name_resource(name)],
                              owner, operation_id)
                old_version = conn.scalar(select(heads.c.version).where(
                    heads.c.namespace == namespace, heads.c.document_id == document_id
                ).with_for_update())
                now = self._now(conn)
                row = dict(id=operation_id, namespace=namespace, name=name,
                           document_id=document_id, collection=collection,
                           idempotency_key=idempotency_key, fingerprint=fingerprint,
                           owner=owner, old_version=old_version, status="INTENT", result=None,
                           error=None, cleanup_pending=False, workspace=None, workspace_node=None,
                           created_at=now, updated_at=now)
                conn.execute(insert(operations).values(**row))
        except IntegrityError as exc:
            raise UploadConflict("Concurrent upload conflicts with this intent") from exc
        self._flush_cache(namespace, operation_id, leases=True)
        op = self._decode(row)
        self._start(op)
        return op

    def _resources(self, op):
        return [_resource(op["document_id"]), _name_resource(op["name"])]

    def _owned(self, conn, op, *, statuses=_ACTIVE):
        self._guard(conn, op["namespace"])
        row = self._row(conn, op["id"], locked=True)
        if row["owner"] != op["owner"] or row["status"] not in statuses:
            raise LeaseLost("Upload ownership or state changed")
        now = self._now(conn)
        held = {item["resource"]: item for item in self._lease_rows(conn, row["namespace"])}
        for resource in self._resources(row):
            lease = held.get(resource)
            if (lease is None or lease["owner"] != op["owner"]
                    or lease["operation_id"] != row["id"] or lease["expires_at"] <= now):
                raise LeaseLost("Upload lease expired or was replaced")
        return row

    def assert_owner(self, op, conn=None):
        if conn is not None:
            return self._decode(self._owned(conn, op))
        with self.engine.begin() as connection:
            return self._decode(self._owned(connection, op))

    def _renew(self, op):
        with self.engine.begin() as conn:
            self._guard(conn, op["namespace"])
            row = self._row(conn, op["id"], locked=True)
            if row["owner"] == op["owner"] and row["status"] in ("COMMITTED", "ROLLED_BACK"):
                self._stop(op)
                return
            self._owned(conn, op)
            self._cache_changed(conn, op["namespace"], leases=True)
            conn.execute(update(leases).where(
                leases.c.namespace == op["namespace"], leases.c.owner == op["owner"],
                leases.c.operation_id == op["id"]
            ).values(expires_at=self._now(conn) + self.lease_seconds))
        self._flush_cache(op["namespace"], leases=True)

    def _start(self, op):
        if self.auto_heartbeat:
            with self._mutex:
                self._heartbeats.setdefault(op["id"], None)
                if self._heartbeats[op["id"]] is None:
                    self._heartbeats[op["id"]] = _Heartbeat(
                        lambda: self._renew(op), self.lease_seconds / 3)

    def _stop(self, op):
        with self._mutex:
            heartbeat = self._heartbeats.pop(op["id"], None)
        if heartbeat is not None:
            heartbeat.stop()

    def close(self):
        """Stop this instance's renewal threads without releasing durable leases."""
        with self._mutex:
            heartbeats, self._heartbeats = self._heartbeats, {}
        for heartbeat in heartbeats.values():
            if heartbeat is not None:
                heartbeat.stop()

    @contextmanager
    def heartbeat(self, op):
        """Renew across arbitrary caller work, even with auto_heartbeat disabled."""
        self.assert_owner(op)
        heartbeat = _Heartbeat(lambda: self._renew(op), self.lease_seconds / 3)
        try:
            yield op
            if heartbeat.error is not None:
                raise LeaseLost("Upload heartbeat failed") from heartbeat.error
            with self.engine.begin() as conn:
                self._guard(conn, op["namespace"])
                row = self._row(conn, op["id"], locked=True)
                if not (row["owner"] == op["owner"]
                        and row["status"] in ("COMMITTED", "ROLLED_BACK")):
                    self._owned(conn, op)
        finally:
            heartbeat.stop()

    def step(self, op, name, action, details=None):
        """Alias for run_step for service orchestration."""
        return self.run_step(op, name, action, details)

    def register_workspace(self, op, path, node=None):
        """Persist a caller-managed workspace before mkdir; path is immutable."""
        path = os.fspath(path)
        node = node or socket.gethostname()
        with self.engine.begin() as conn:
            row = self._owned(conn, op, statuses=("INTENT", "RUNNING"))
            if row["workspace"] is not None and (
                    row["workspace"] != path or row["workspace_node"] != node):
                raise UploadConflict("Workspace is immutable")
            self._cache_changed(conn, op["namespace"], op["id"])
            conn.execute(update(operations).where(operations.c.id == op["id"]).values(
                workspace=path, workspace_node=node, updated_at=self._now(conn)))
        self._flush_cache(op["namespace"], op["id"])
        op["workspace"] = path
        op["workspace_node"] = node
        return path

    def intent(self, op, name, details=None):
        """Commit a step intent before an externally coordinated publication."""
        with self.engine.begin() as conn:
            self._owned(conn, op, statuses=("INTENT", "RUNNING"))
            self._cache_changed(conn, op["namespace"], op["id"])
            conn.execute(insert(steps).values(
                operation_id=op["id"], name=name, status="RUNNING",
                details=_json(details or {}), started_at=self._now(conn)))
            conn.execute(update(operations).where(operations.c.id == op["id"]).values(
                status="RUNNING", updated_at=self._now(conn)))

        self._flush_cache(op["namespace"], op["id"])

    def run_step(self, op, name, action, details=None):
        """Durably record RUNNING before action(); DONE only after fencing."""
        try:
            with self.engine.begin() as conn:
                self._owned(conn, op, statuses=("INTENT", "RUNNING"))
                self._cache_changed(conn, op["namespace"], op["id"])
                conn.execute(insert(steps).values(
                    operation_id=op["id"], name=name, status="RUNNING",
                    details=_json(details or {}), started_at=self._now(conn)))
                conn.execute(update(operations).where(operations.c.id == op["id"]).values(
                    status="RUNNING", updated_at=self._now(conn)))
        except IntegrityError as exc:
            raise UploadConflict("Step name already exists; recover instead of replaying actions") from exc
        self._flush_cache(op["namespace"], op["id"])
        try:
            with self.heartbeat(op):
                logger.info("upload_step operation_id=%s step=%s status=RUNNING", op["id"], name)
                result = action()
            with self.engine.begin() as conn:
                self._owned(conn, op, statuses=("RUNNING",))
                self._cache_changed(conn, op["namespace"], op["id"])
                conn.execute(update(steps).where(
                    steps.c.operation_id == op["id"], steps.c.name == name
                ).values(status="DONE", finished_at=self._now(conn)))
            self._flush_cache(op["namespace"], op["id"])
            logger.info("upload_step operation_id=%s step=%s status=DONE", op["id"], name)
            return result
        except Exception as exc:
            logger.warning("upload_step operation_id=%s step=%s status=FAILED error_type=%s",
                           op["id"], name, type(exc).__name__)
            try:
                self.fail(op, exc, step=name)
            except LeaseLost:
                pass
            raise

    def fail(self, op, error=None, *, step=None):
        """Mark failed but retain the live lease until this worker rolls back/expires."""
        with self.engine.begin() as conn:
            self._owned(conn, op, statuses=("INTENT", "RUNNING", "FAILED"))
            self._cache_changed(conn, op["namespace"], op["id"])
            error_type = type(error).__name__ if error is not None else None
            now = self._now(conn)
            conn.execute(update(operations).where(operations.c.id == op["id"]).values(
                status="FAILED", error=error_type, updated_at=now))
            if step is not None:
                conn.execute(update(steps).where(
                    steps.c.operation_id == op["id"], steps.c.name == step
                ).values(status="FAILED", error=error_type, finished_at=now))

        self._flush_cache(op["namespace"], op["id"])

    def claim_recovery(self, operation_id):
        """Return a fresh owner for expired work, or None for live/terminal work."""
        snapshot = self.get(operation_id)
        with self.engine.begin() as conn:
            self._guard(conn, snapshot["namespace"])
            row = self._row(conn, operation_id, locked=True)
            if row["status"] not in _ACTIVE:
                return None
            now = self._now(conn)
            if any(item["operation_id"] == operation_id and item["expires_at"] > now
                   for item in self._lease_rows(conn, row["namespace"])):
                return None
            owner = uuid4().hex
            try:
                self._acquire(conn, row["namespace"], self._resources(row), owner, operation_id)
            except UploadConflict:
                return None
            row.update(owner=owner, status="ROLLING_BACK", updated_at=now)
            conn.execute(update(operations).where(operations.c.id == operation_id).values(
                owner=owner, status="ROLLING_BACK", updated_at=now))
        self._flush_cache(row["namespace"], operation_id, leases=True)
        op = self._decode(row)
        self._stop(snapshot)
        self._start(op)
        return op

    def _release(self, conn, op):
        conn.execute(delete(leases).where(
            leases.c.namespace == op["namespace"], leases.c.owner == op["owner"],
            leases.c.operation_id == op["id"]
        ))

    def rollback(self, op, cleanup):
        """Compensate this version, then delete staged objects; never modify heads."""
        with self.engine.begin() as conn:
            row = self._owned(conn, op)
            if conn.scalar(select(heads.c.version).where(
                heads.c.namespace == row["namespace"], heads.c.document_id == row["document_id"]
            )) == row["id"]:
                raise UploadConflict("Cannot roll back the active version")
            self._cache_changed(conn, op["namespace"], op["id"])
            now = self._now(conn)
            self._insert_ignore(conn, steps, dict(
                operation_id=op["id"], name="rollback", status="RUNNING",
                details=_json({"version": op["id"]}), started_at=now))
            conn.execute(update(steps).where(
                steps.c.operation_id == op["id"], steps.c.name == "rollback"
            ).values(status="RUNNING", error=None, finished_at=None))
            conn.execute(update(operations).where(operations.c.id == op["id"]).values(
                status="ROLLING_BACK", updated_at=now))
        self._flush_cache(op["namespace"], op["id"])
        try:
            with self.heartbeat(op):
                cleanup(self._decode(row))
            with self.engine.begin() as conn:
                self._owned(conn, op, statuses=("ROLLING_BACK", "ROLLBACK_FAILED"))
                self._cache_changed(conn, op["namespace"], op["id"], leases=True)
                conn.execute(delete(objects).where(objects.c.operation_id == op["id"]))
                conn.execute(update(steps).where(
                    steps.c.operation_id == op["id"], steps.c.name == "rollback"
                ).values(status="DONE", finished_at=self._now(conn)))
                conn.execute(update(operations).where(operations.c.id == op["id"]).values(
                    status="ROLLED_BACK", updated_at=self._now(conn)))
                self._release(conn, op)
            self._flush_cache(op["namespace"], op["id"], leases=True)
        except Exception as exc:
            try:
                with self.engine.begin() as conn:
                    self._owned(conn, op, statuses=("ROLLING_BACK", "ROLLBACK_FAILED"))
                    self._cache_changed(conn, op["namespace"], op["id"])
                    conn.execute(update(steps).where(
                        steps.c.operation_id == op["id"], steps.c.name == "rollback"
                    ).values(status="FAILED", error=type(exc).__name__, finished_at=self._now(conn)))
                    conn.execute(update(operations).where(operations.c.id == op["id"]).values(
                        status="ROLLBACK_FAILED", error=type(exc).__name__,
                        updated_at=self._now(conn)))
                self._flush_cache(op["namespace"], op["id"])
            except LeaseLost:
                pass
            raise
        finally:
            self._stop(op)
        return self.get(op["id"])

    def commit_noop(self, op, result):
        """Finish a deduplicated request without changing the document head."""
        with self.engine.begin() as conn:
            self._owned(conn, op, statuses=("INTENT", "RUNNING"))
            self._cache_changed(conn, op["namespace"], op["id"], leases=True)
            conn.execute(delete(objects).where(objects.c.operation_id == op["id"]))
            conn.execute(update(operations).where(operations.c.id == op["id"]).values(
                status="COMMITTED", result=_json(result), error=None,
                cleanup_pending=False, updated_at=self._now(conn)))
            self._release(conn, op)
        self._flush_cache(op["namespace"], op["id"], leases=True)
        self._stop(op)

    def commit(self, op, result, conn):
        """Fence and publish with the caller's documents/blob transaction (no commit here)."""
        if not conn.in_transaction():
            raise ValueError("commit requires the caller's transaction")
        encoded = _json(result)
        row = self._owned(conn, op, statuses=("INTENT", "RUNNING"))
        self._cache_changed(conn, row["namespace"], row["id"], heads=True, leases=True)
        now = self._now(conn)
        needs_cleanup = row["old_version"] is not None or conn.execute(select(steps.c.name).where(
            steps.c.operation_id == op["id"], steps.c.name == "stage_qdrant"
        )).first() is not None
        values = dict(namespace=row["namespace"], document_id=row["document_id"],
                      version=row["id"], updated_at=now)
        self._insert_ignore(conn, heads, values)
        conn.execute(update(heads).where(
            heads.c.namespace == row["namespace"], heads.c.document_id == row["document_id"]
        ).values(version=row["id"], updated_at=now))
        conn.execute(update(operations).where(operations.c.id == row["id"]).values(
            status="COMMITTED", result=encoded, error=None,
            cleanup_pending=needs_cleanup, updated_at=now))
        conn.execute(update(steps).where(
            steps.c.operation_id == op["id"], steps.c.name == "publish"
        ).values(status="DONE", finished_at=now))
        self._release(conn, op)
        row.update(status="COMMITTED", result=encoded,
                   cleanup_pending=needs_cleanup, updated_at=now)
        return self._decode(row)

    def cleanup(self, op, cleanup_old):
        """Retry post-commit old-version cleanup; never delete the current head."""
        current = self.get(op["id"])
        if current["status"] != "COMMITTED":
            raise UploadConflict("Old-version cleanup requires COMMITTED")
        if not current["cleanup_pending"]:
            return current
        with self.engine.connect() as conn:
            if self._now(conn) < current["updated_at"] + self.cleanup_delay_seconds:
                return current
        # Namespace lock fences all uploads while the old version is removed.
        with self.lock(current["namespace"]) as token:
            current = self.get(op["id"])
            if not current["cleanup_pending"]:
                return current
            if current["old_version"] in self.active_versions(current["namespace"]).values():
                raise UploadConflict("Cannot clean an active version")
            cleanup_old(current)
            with self.engine.begin() as conn:
                self.assert_lock(token, conn)
                self._cache_changed(conn, current["namespace"], current["id"])
                conn.execute(delete(objects).where(
                    objects.c.operation_id == current["old_version"]))
                conn.execute(update(operations).where(
                    operations.c.id == current["id"], operations.c.status == "COMMITTED"
                ).values(cleanup_pending=False, updated_at=self._now(conn)))
            self._flush_cache(current["namespace"], current["id"])
        return self.get(op["id"])

    def active_versions(self, namespace):
        with self.engine.connect() as conn:
            return dict(conn.execute(select(heads.c.document_id, heads.c.version).where(
                heads.c.namespace == namespace)).all())

    def head(self, namespace, document_id):
        return self.active_versions(namespace).get(document_id)

    def remove_head(self, namespace, document_id, conn, token):
        """Publish a deletion in the caller's transaction under a generic lock."""
        if not conn.in_transaction():
            raise ValueError("remove_head requires the caller's transaction")
        if (token["namespace"] != namespace
                or token["resource"] not in (_NAMESPACE, _resource(document_id))):
            raise LeaseLost("Lock does not cover this document")
        self.assert_lock(token, conn)
        if conn.scalar(select(heads.c.version).where(
                heads.c.namespace == namespace, heads.c.document_id == document_id)) is None:
            return
        self._cache_changed(conn, namespace, heads=True)
        conn.execute(delete(heads).where(
            heads.c.namespace == namespace, heads.c.document_id == document_id))

    def purge_document_objects(self, namespace, document_id, conn, token) -> tuple[str, ...]:
        """Purge bytes and return changed operation IDs for display flush after commit.

        The caller owns the transaction and must flush these upload_status keys
        plus upload_list after commit; no cache transport is accessed here.
        """
        if (token["namespace"] != namespace
                or token["resource"] not in (_NAMESPACE, _resource(document_id))):
            raise LeaseLost("Lock does not cover this document")
        self.assert_lock(token, conn)
        ids = select(operations.c.id).where(
            operations.c.namespace == namespace, operations.c.document_id == document_id)
        conn.execute(delete(objects).where(objects.c.operation_id.in_(ids)))
        changed_ids = list(conn.scalars(select(operations.c.id).where(
            operations.c.namespace == namespace, operations.c.document_id == document_id,
            operations.c.status == "COMMITTED", operations.c.cleanup_pending.is_(True))))
        if changed_ids:
            cache_events.touch(conn, ("upload_list", namespace), *(
                ("upload_status", operation_id) for operation_id in changed_ids))
            conn.execute(update(operations).where(
                operations.c.id.in_(changed_ids)
            ).values(cleanup_pending=False))
        return tuple(changed_ids)

    def old_version(self, op):
        return self.get(op["id"])["old_version"]

    def put_object(self, op, bucket, key, content, conn=None, *, metadata=None):
        """Immutable bytes per version; identical retries are allowed before commit."""
        if conn is None:
            with self.engine.begin() as connection:
                return self.put_object(op, bucket, key, content, connection, metadata=metadata)
        self._owned(conn, op, statuses=("INTENT", "RUNNING"))
        content = bytes(content)
        encoded = _json(metadata) if metadata is not None else None
        predicate = (objects.c.operation_id == op["id"], objects.c.bucket == bucket,
                     objects.c.key == key)
        existing = conn.execute(select(objects).where(*predicate).with_for_update()).mappings().first()
        if existing is not None:
            if bytes(existing["content"]) != content or existing["metadata"] != encoded:
                raise UploadConflict("Upload objects are immutable")
            return
        conn.execute(insert(objects).values(operation_id=op["id"], bucket=bucket, key=key,
                                             content=content, metadata=encoded,
                                             created_at=self._now(conn)))

    def get_object(self, version, bucket, key):
        with self.engine.connect() as conn:
            content = conn.scalar(select(objects.c.content).where(
                objects.c.operation_id == version, objects.c.bucket == bucket, objects.c.key == key))
            return bytes(content) if content is not None else None

    def acquire_lock(self, namespace, document_id=None):
        """Generic exclusive namespace lock, or document lock shared with uploads."""
        token = dict(namespace=namespace, resource=_NAMESPACE if document_id is None
                     else _resource(document_id), owner=uuid4().hex)
        with self.engine.begin() as conn:
            self._guard(conn, namespace)
            self._acquire(conn, namespace, [token["resource"]], token["owner"])
        self._flush_cache(namespace, leases=True)
        return token

    def assert_lock(self, token, conn=None):
        if conn is None:
            with self.engine.begin() as connection:
                return self.assert_lock(token, connection)
        self._guard(conn, token["namespace"])
        row = conn.execute(select(leases).where(
            leases.c.namespace == token["namespace"], leases.c.resource == token["resource"]
        ).with_for_update()).mappings().first()
        if row is None or row["owner"] != token["owner"] or row["expires_at"] <= self._now(conn):
            raise LeaseLost("Generic lock expired or was replaced")

    def renew_lock(self, token):
        with self.engine.begin() as conn:
            self.assert_lock(token, conn)
            self._cache_changed(conn, token["namespace"], leases=True)
            conn.execute(update(leases).where(
                leases.c.namespace == token["namespace"], leases.c.resource == token["resource"],
                leases.c.owner == token["owner"]
            ).values(expires_at=self._now(conn) + self.lease_seconds))

        self._flush_cache(token["namespace"], leases=True)

    def release_lock(self, token):
        with self.engine.begin() as conn:
            self._guard(conn, token["namespace"])
            predicate = (leases.c.namespace == token["namespace"],
                         leases.c.resource == token["resource"],
                         leases.c.owner == token["owner"])
            if conn.execute(select(leases.c.resource).where(*predicate)).first() is None:
                return
            self._cache_changed(conn, token["namespace"], leases=True)
            conn.execute(delete(leases).where(*predicate))
        self._flush_cache(token["namespace"], leases=True)

    @contextmanager
    def lock(self, namespace, document_id=None):
        token = self.acquire_lock(namespace, document_id)
        heartbeat = _Heartbeat(lambda: self.renew_lock(token), self.lease_seconds / 3)
        try:
            yield token
            if heartbeat.error is not None:
                raise LeaseLost("Generic lock heartbeat failed") from heartbeat.error
            self.assert_lock(token)
        finally:
            heartbeat.stop()
            self.release_lock(token)
