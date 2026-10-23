"""Durable immutable uploads, task ownership and transactional queue outbox."""

from __future__ import annotations

import hashlib
import math
import os
from dataclasses import dataclass
from uuid import UUID, uuid4

from sqlalchemy import insert, select, update

from . import cache_events
from .schema import document_task_outbox as outbox
from .schema import document_tasks as tasks
from .schema import documents, knowledge_bases, upload_operations
from .uploads import LeaseLost, UploadConflict


@dataclass(frozen=True)
class DocumentTaskSettings:
    max_attempts: int = 3
    lease_seconds: float = 120
    retry_seconds: float = 10

    def __post_init__(self):
        if type(self.max_attempts) is not int or self.max_attempts < 1:
            raise ValueError("RAG_DOCUMENT_MAX_ATTEMPTS must be a positive integer")
        for name in ("lease_seconds", "retry_seconds"):
            value = getattr(self, name)
            if not math.isfinite(value) or value <= 0:
                raise ValueError(f"Document task {name} must be finite and positive")

    @classmethod
    def from_env(cls):
        return cls(int(os.getenv("RAG_DOCUMENT_MAX_ATTEMPTS", "3")),
                   float(os.getenv("RAG_DOCUMENT_LEASE_SECONDS", "120")),
                   float(os.getenv("RAG_DOCUMENT_RETRY_SECONDS", "10")))


def validate_artifact_id(value):
    try:
        if not isinstance(value, str) or str(UUID(value)) != value:
            raise ValueError
    except (ValueError, AttributeError):
        raise ValueError("artifact_id must be a canonical UUID") from None
    return value


class DocumentTasks:
    def __init__(self, state, settings=None):
        self.state = state
        self.engine = state.engine
        self.settings = settings or DocumentTaskSettings.from_env()
        state.cache_sync.register("document_status", self._load_status)

    def _changed(self, conn, artifact_id):
        cache_events.touch(conn, ("document_status", artifact_id))

    def flush(self, artifact_id):
        self.state.cache_sync.flush_pending(keys=(("document_status", artifact_id),))

    def _load_status(self, conn, artifact_id):
        row = conn.execute(select(
            tasks.c.artifact_id, tasks.c.namespace, tasks.c.status, tasks.c.stage, tasks.c.error
        ).where(tasks.c.artifact_id == artifact_id)).mappings().first()
        return dict(row) if row else None

    def status(self, namespace, artifact_id):
        validate_artifact_id(artifact_id)
        result = self.state.cache_sync.get_or_load(
            "document_status", artifact_id, lambda conn: self._load_status(conn, artifact_id))
        if result is None or result["namespace"] != namespace:
            raise LookupError("Document upload not found")
        return {key: value for key, value in result.items() if key != "namespace"}

    def submit(self, namespace, name, content, idempotency_key=None):
        from pathlib import Path, PureWindowsPath

        from ..service_knowledge import (
            MAX_FILE_BYTES,
            SUPPORTED_EXTENSIONS,
            KnowledgeDocumentStore,
            KnowledgeValidationError,
        )

        if (not isinstance(name, str) or KnowledgeDocumentStore.sanitize_name(name) != name
                or PureWindowsPath(name).is_reserved() or Path(name).suffix.casefold() not in SUPPORTED_EXTENSIONS):
            raise KnowledgeValidationError("Invalid document filename or file type")
        if not isinstance(content, bytes) or not 0 < len(content) <= MAX_FILE_BYTES:
            raise KnowledgeValidationError("File must be non-empty and no larger than 10 MiB")
        if idempotency_key is not None and (
                not isinstance(idempotency_key, str) or not 1 <= len(idempotency_key) <= 128):
            raise KnowledgeValidationError("idempotency_key must be a string of 1 to 128 characters")
        digest = hashlib.sha256(content).hexdigest()
        with self.engine.begin() as conn:
            journal = self.state.uploads
            journal._guard(conn, namespace)
            if idempotency_key is not None:
                existing = conn.execute(select(tasks.c.artifact_id, tasks.c.name, tasks.c.sha256).where(
                    tasks.c.namespace == namespace, tasks.c.idempotency_key == idempotency_key
                ).with_for_update()).mappings().first()
                if existing:
                    if existing["name"] != name or existing["sha256"] != digest:
                        raise UploadConflict("Idempotency key has different file input")
                    return existing["artifact_id"]
            version = conn.scalar(select(knowledge_bases.c.version).where(
                knowledge_bases.c.namespace == namespace).with_for_update())
            if version is None:
                raise ValueError("Knowledge base is not configured")
            names = conn.scalars(select(documents.c.name).where(
                documents.c.namespace == namespace, documents.c.del_flag.is_(False)))
            if any(other.casefold() == name.casefold() for other in names):
                raise UploadConflict("Document already exists; use update_document")
            name_key = hashlib.sha256(name.casefold().encode()).hexdigest()
            if conn.scalar(select(tasks.c.artifact_id).where(
                tasks.c.namespace == namespace, tasks.c.name_key == name_key,
                tasks.c.status.in_(("queued", "processing"))).limit(1)):
                raise UploadConflict("A document with this filename is already queued")
            artifact_id = str(uuid4())
            now = journal._now(conn)
            self._changed(conn, artifact_id)
            conn.execute(insert(tasks).values(
                artifact_id=artifact_id, namespace=namespace, name=name, name_key=name_key,
                content=content, sha256=digest, idempotency_key=idempotency_key,
                configuration_version=version, status="queued", stage="queued", attempts=0,
                owner=None, lease_until=0, operation_id=None, error=None,
                created_at=now, updated_at=now))
            conn.execute(insert(outbox).values(artifact_id=artifact_id, pending=True, generation=1, retry_at=0))
        self.flush(artifact_id)
        return artifact_id

    def dispatch(self, artifact_id, transport):
        """Confirm send before clearing the outbox; a lost confirmation may duplicate."""
        if transport is None:
            raise RuntimeError("Configure RAG_ROCKETMQ_ENDPOINT for asynchronous add_document")
        with self.engine.connect() as conn:
            delivery = conn.execute(select(outbox).where(
                outbox.c.artifact_id == artifact_id)).mappings().first()
        if delivery is None or not delivery["pending"]:
            return
        transport.send(artifact_id)
        with self.engine.begin() as conn:
            conn.execute(update(outbox).where(
                outbox.c.artifact_id == artifact_id, outbox.c.generation == delivery["generation"]
            ).values(pending=False))

    def dispatch_pending(self, transport, limit=20):
        with self.engine.connect() as conn:
            now = self.state.uploads._now(conn)
            ids = list(conn.scalars(select(outbox.c.artifact_id).where(
                outbox.c.pending.is_(True), outbox.c.retry_at <= now
            ).order_by(outbox.c.retry_at, outbox.c.artifact_id).limit(limit)))
        for artifact_id in ids:
            try:
                self.dispatch(artifact_id, transport)
            except Exception:  # noqa: BLE001 - any failure leaves the durable outbox intact
                # Stop on broker failure; do not serialize a timeout per queued upload.
                break
        return len(ids)

    def _row(self, conn, artifact_id):
        columns = [column for column in tasks.c if column.name != "content"]
        row = conn.execute(select(*columns).where(tasks.c.artifact_id == artifact_id)
                           .with_for_update()).mappings().first()
        return dict(row) if row else None

    def _owned(self, conn, task):
        self.state.uploads._guard(conn, task["namespace"])
        row = self._row(conn, task["artifact_id"])
        if (row is None or row["owner"] != task["owner"] or row["status"] != "processing"
                or row["lease_until"] <= self.state.uploads._now(conn)):
            raise LeaseLost("Document task ownership expired")
        return row

    def _finish(self, conn, row, status, error=None):
        self._changed(conn, row["artifact_id"])
        conn.execute(update(tasks).where(tasks.c.artifact_id == row["artifact_id"]).values(
            status=status, stage="completed" if status == "succeeded" else "failed", error=error,
            owner=None, lease_until=0, updated_at=self.state.uploads._now(conn)))
        conn.execute(update(outbox).where(outbox.c.artifact_id == row["artifact_id"]).values(pending=False))

    def claim(self, artifact_id):
        validate_artifact_id(artifact_id)
        task = self._claim(artifact_id)
        self.flush(artifact_id)
        return task

    def _claim(self, artifact_id):
        with self.engine.connect() as conn:
            namespace = conn.scalar(select(tasks.c.namespace).where(tasks.c.artifact_id == artifact_id))
        if namespace is None:
            return None
        with self.engine.begin() as conn:
            self.state.uploads._guard(conn, namespace)
            row = self._row(conn, artifact_id)
            now = self.state.uploads._now(conn)
            if row["status"] in ("succeeded", "failed") or row["lease_until"] > now:
                return None
            if row["operation_id"]:
                operation = conn.execute(select(upload_operations.c.status).where(
                    upload_operations.c.id == row["operation_id"])).scalar()
                if operation == "COMMITTED":
                    self._finish(conn, row, "succeeded")
                    return None
                if operation not in (None, "ROLLED_BACK"):
                    return None  # Execution recovery must compensate before a fresh attempt.
            if row["attempts"] >= self.settings.max_attempts:
                self._finish(conn, row, "failed", "Document processing retry limit reached")
                return None
            self._changed(conn, artifact_id)
            row.update(status="processing", stage="receiving", owner=uuid4().hex,
                       attempts=row["attempts"] + 1, operation_id=None,
                       lease_until=now + self.settings.lease_seconds, error=None, updated_at=now)
            conn.execute(update(tasks).where(tasks.c.artifact_id == artifact_id).values(
                **{key: row[key] for key in ("status", "stage", "owner", "attempts", "operation_id",
                                            "lease_until", "error", "updated_at")}))
            row["content"] = conn.scalar(select(tasks.c.content).where(tasks.c.artifact_id == artifact_id))
        return row

    def heartbeat(self, task):
        with self.engine.begin() as conn:
            self._owned(conn, task)
            conn.execute(update(tasks).where(tasks.c.artifact_id == task["artifact_id"]).values(
                lease_until=self.state.uploads._now(conn) + self.settings.lease_seconds))

    def bind(self, conn, task, operation):
        self._owned(conn, task)
        conn.execute(update(tasks).where(tasks.c.artifact_id == task["artifact_id"]).values(
            operation_id=operation["id"]))
        task["operation_id"] = operation["id"]

    def progress(self, task, stage):
        with self.engine.begin() as conn:
            self._owned(conn, task)
            self._changed(conn, task["artifact_id"])
            conn.execute(update(tasks).where(tasks.c.artifact_id == task["artifact_id"]).values(
                stage=stage, updated_at=self.state.uploads._now(conn)))
        self.flush(task["artifact_id"])

    def succeed(self, conn, task):
        row = self._owned(conn, task)
        version = conn.scalar(select(knowledge_bases.c.version).where(
            knowledge_bases.c.namespace == task["namespace"]).with_for_update())
        if version != task["configuration_version"]:
            raise UploadConflict("Knowledge-base configuration changed; upload again")
        self._finish(conn, row, "succeeded")

    def fail(self, task, *, retry, error):
        with self.engine.begin() as conn:
            row = self._owned(conn, task)
            if not retry or row["attempts"] >= self.settings.max_attempts:
                self._finish(conn, row, "failed", error)
            else:
                self._changed(conn, task["artifact_id"])
                retry_at = self.state.uploads._now(conn) + self.settings.retry_seconds * row["attempts"]
                conn.execute(update(tasks).where(tasks.c.artifact_id == task["artifact_id"]).values(
                    status="queued", stage="retry_wait", error=None, owner=None, lease_until=retry_at,
                    updated_at=self.state.uploads._now(conn)))
                conn.execute(update(outbox).where(outbox.c.artifact_id == task["artifact_id"]).values(
                    pending=True, generation=outbox.c.generation + 1, retry_at=retry_at))
        self.flush(task["artifact_id"])

    def terminal(self, artifact_id):
        with self.engine.connect() as conn:
            status = conn.scalar(select(tasks.c.status).where(tasks.c.artifact_id == artifact_id))
        return status is None or status in ("succeeded", "failed")

    def redrive_expired(self, limit=20):
        """Republish expired work even after broker retry/DLQ retention is exhausted."""
        with self.engine.begin() as conn:
            now = self.state.uploads._now(conn)
            ids = list(conn.scalars(select(tasks.c.artifact_id).where(
                tasks.c.status.in_(("queued", "processing")), tasks.c.lease_until <= now,
                tasks.c.updated_at <= now - self.settings.lease_seconds
            ).order_by(tasks.c.updated_at).limit(limit).with_for_update(skip_locked=True)))
            for artifact_id in ids:
                conn.execute(update(outbox).where(
                    outbox.c.artifact_id == artifact_id, outbox.c.pending.is_(False)
                ).values(pending=True, generation=outbox.c.generation + 1, retry_at=now))
                conn.execute(update(tasks).where(tasks.c.artifact_id == artifact_id).values(updated_at=now))
        return len(ids)
