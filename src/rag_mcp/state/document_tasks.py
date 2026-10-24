"""Durable immutable uploads, task ownership and transactional queue outbox."""

from __future__ import annotations

import hashlib
import logging
import math
import os
import random
from dataclasses import dataclass
from uuid import UUID, uuid4

from sqlalchemy import func, insert, select, update

from . import cache_events
from .schema import document_task_outbox as outbox
from .schema import document_tasks as tasks
from .schema import documents, knowledge_bases
from .uploads import UploadConflict

logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class DocumentDispatchSettings:
    lease_seconds: float = 30
    retry_seconds: float = 2
    max_retry_seconds: float = 300
    alert_seconds: float = 300

    def __post_init__(self):
        for name in ("lease_seconds", "retry_seconds", "max_retry_seconds", "alert_seconds"):
            value = getattr(self, name)
            if type(value) not in (int, float) or not math.isfinite(value) or value <= 0:
                raise ValueError(f"Document dispatch {name} must be finite and positive")
        if self.max_retry_seconds < self.retry_seconds:
            raise ValueError("Document dispatch max retry must not be below retry base")

    @classmethod
    def from_env(cls):
        return cls(*(float(os.getenv(f"RAG_DOCUMENT_DISPATCH_{key}", default)) for key, default in (
            ("LEASE_SECONDS", "30"), ("RETRY_SECONDS", "2"),
            ("MAX_RETRY_SECONDS", "300"), ("ALERT_SECONDS", "300"))))


def validate_artifact_id(value):
    try:
        if not isinstance(value, str) or str(UUID(value)) != value:
            raise ValueError
    except (ValueError, AttributeError):
        raise ValueError("artifact_id must be a canonical UUID") from None
    return value


class DocumentTasks:
    def __init__(self, state):
        self.state = state
        self.engine = state.engine
        self.dispatch_settings = DocumentDispatchSettings.from_env()
        self._dispatch_pause_until = 0
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
                configuration_version=version, status="queued", stage="dispatch_wait", attempts=0,
                owner=None, lease_until=0, operation_id=None, error=None,
                created_at=now, updated_at=now))
            conn.execute(insert(outbox).values(artifact_id=artifact_id, pending=True, generation=1, retry_at=0))
        return artifact_id

    def _delivery_claim(self, artifact_id):
        with self.engine.connect() as conn:
            namespace = conn.scalar(select(tasks.c.namespace).where(tasks.c.artifact_id == artifact_id))
        if namespace is None:
            return None
        with self.engine.begin() as conn:
            self.state.uploads._guard(conn, namespace)
            now = self.state.uploads._now(conn)
            row = conn.execute(select(outbox).where(outbox.c.artifact_id == artifact_id)
                               .with_for_update()).mappings().first()
            if (row is None or not row["pending"] or row["retry_at"] > now
                    or row["lease_until"] > now):
                return None
            owner = uuid4().hex
            changed = conn.execute(update(outbox).where(
                outbox.c.artifact_id == artifact_id, outbox.c.pending.is_(True),
                outbox.c.generation == row["generation"], outbox.c.lease_until <= now,
                outbox.c.retry_at <= now,
            ).values(owner=owner, lease_until=now + self.dispatch_settings.lease_seconds,
                     attempts=outbox.c.attempts + 1))
            if changed.rowcount != 1:
                return None
            return {**dict(row), "namespace": namespace, "owner": owner,
                    "attempts": row["attempts"] + 1}

    def _delivery_result(self, delivery, error=None):
        artifact_id = delivery["artifact_id"]
        with self.engine.begin() as conn:
            self.state.uploads._guard(conn, delivery["namespace"])
            task = self._row(conn, artifact_id)
            now = self.state.uploads._now(conn)
            predicate = (outbox.c.artifact_id == artifact_id,
                         outbox.c.generation == delivery["generation"],
                         outbox.c.owner == delivery["owner"], outbox.c.pending.is_(True),
                         outbox.c.lease_until > now)
            values = {"owner": None, "lease_until": 0, "pending": error is not None, "last_error": error}
            if error is not None:
                ceiling = min(self.dispatch_settings.max_retry_seconds,
                              self.dispatch_settings.retry_seconds * 2 ** min(delivery["attempts"] - 1, 20))
                values["retry_at"] = now + random.uniform(ceiling / 2, ceiling)
            changed = conn.execute(update(outbox).where(*predicate).values(**values))
            if changed.rowcount == 1 and task["status"] == "queued" and task["stage"] in ("dispatch_wait", "dispatch_retry", "queued"):
                self._changed(conn, artifact_id)
                conn.execute(update(tasks).where(tasks.c.artifact_id == artifact_id).values(
                    stage="dispatch_retry" if error else "queued", updated_at=now))
        self.flush(artifact_id)

    @staticmethod
    def _broker_unavailable(exc):
        if isinstance(exc, (ConnectionError, TimeoutError)):
            return True
        try:
            import grpc
            from rocketmq.v5.exception.client_exception import (
                InternalErrorException,
                ProxyTimeoutException,
            )
        except ImportError:
            return False
        if isinstance(exc, (InternalErrorException, ProxyTimeoutException)):
            return True
        return isinstance(exc, grpc.RpcError) and exc.code() in (
            grpc.StatusCode.UNAVAILABLE, grpc.StatusCode.DEADLINE_EXCEEDED,
            grpc.StatusCode.RESOURCE_EXHAUSTED,
        )

    def dispatch(self, artifact_id, transport):
        """Claim briefly, send outside SQL, then fence this generation's confirmation."""
        if transport is None:
            return False
        delivery = self._delivery_claim(artifact_id)
        if delivery is None:
            return False
        try:
            transport.send(artifact_id)
        except Exception as exc:  # noqa: BLE001 - retain delivery and only persist a safe error category
            # Never retain SDK exception text, which may contain credentials.
            category = type(exc).__name__
            self._delivery_result(delivery, category)
            logger.warning("Document delivery deferred artifact_id=%s attempts=%s error_type=%s",
                           artifact_id, delivery["attempts"], category)
            if self._broker_unavailable(exc):
                with self.engine.connect() as conn:
                    ceiling = min(self.dispatch_settings.max_retry_seconds,
                                  self.dispatch_settings.retry_seconds * 2 ** min(delivery["attempts"] - 1, 20))
                    self._dispatch_pause_until = self.state.uploads._now(conn) + random.uniform(ceiling / 2, ceiling)
                return "unavailable"
            return False
        self._delivery_result(delivery)
        return True

    def dispatch_pending(self, transport, limit=20):
        if transport is None:
            return 0
        with self.engine.connect() as conn:
            now = self.state.uploads._now(conn)
            if now < self._dispatch_pause_until:
                return 0
            ids = list(conn.scalars(select(outbox.c.artifact_id).where(
                outbox.c.pending.is_(True), outbox.c.retry_at <= now, outbox.c.lease_until <= now,
            ).order_by(outbox.c.retry_at, outbox.c.artifact_id).limit(limit)))
        sent = 0
        for artifact_id in ids:
            try:
                result = self.dispatch(artifact_id, transport)
                if result == "unavailable":
                    break
                sent += result is True
            except Exception as exc:  # noqa: BLE001 - SQL/confirmation failure is recovered by delivery lease
                logger.warning("Document relay deferred artifact_id=%s error_type=%s",
                               artifact_id, type(exc).__name__)
                break
        return sent

    def delivery_health(self):
        with self.engine.connect() as conn:
            now = self.state.uploads._now(conn)
            row = conn.execute(select(func.count(), func.min(tasks.c.created_at)).select_from(
                tasks.join(outbox, tasks.c.artifact_id == outbox.c.artifact_id)).where(
                    outbox.c.pending.is_(True), tasks.c.status == "queued")).first()
        age = max(0, now - row[1]) if row[1] is not None else 0
        if row[0] and age >= self.dispatch_settings.alert_seconds:
            logger.warning("Document delivery backlog pending=%s oldest_seconds=%.0f", row[0], age)
        return {"pending": row[0], "oldest_seconds": age}

    def _row(self, conn, artifact_id):
        columns = [column for column in tasks.c if column.name != "content"]
        row = conn.execute(select(*columns).where(tasks.c.artifact_id == artifact_id)
                           .with_for_update()).mappings().first()
        return dict(row) if row else None

    def _active(self, conn, task):
        self.state.uploads._guard(conn, task["namespace"])
        row = self._row(conn, task["artifact_id"])
        if row is None or row["status"] not in ("queued", "processing"):
            raise UploadConflict("Document upload already has a final result")
        return row

    def _finish(self, conn, row, status, error=None, *, stage=None):
        self._changed(conn, row["artifact_id"])
        changed = conn.execute(update(tasks).where(
            tasks.c.artifact_id == row["artifact_id"], tasks.c.status.in_(("queued", "processing")),
        ).values(status=status, stage=stage or ("completed" if status == "succeeded" else "failed"),
                 error=error, owner=None, lease_until=0, updated_at=self.state.uploads._now(conn)))
        if changed.rowcount != 1:
            raise UploadConflict("Document upload already has a final result")

    def claim(self, artifact_id):
        """Load a broker delivery; processing status is not an execution lease."""
        validate_artifact_id(artifact_id)
        with self.engine.connect() as conn:
            namespace = conn.scalar(select(tasks.c.namespace).where(tasks.c.artifact_id == artifact_id))
        if namespace is None:
            return None
        with self.engine.begin() as conn:
            self.state.uploads._guard(conn, namespace)
            row = self._row(conn, artifact_id)
            if row["status"] not in ("queued", "processing"):
                return None
            self._changed(conn, artifact_id)
            conn.execute(update(tasks).where(tasks.c.artifact_id == artifact_id).values(
                status="processing", stage="receiving", updated_at=self.state.uploads._now(conn)))
            row.update(status="processing", stage="receiving", execution_id=uuid4().hex)
            row["content"] = conn.scalar(select(tasks.c.content).where(tasks.c.artifact_id == artifact_id))
        self.flush(artifact_id)
        return row

    def bind(self, conn, task, operation):
        self._active(conn, task)
        conn.execute(update(tasks).where(tasks.c.artifact_id == task["artifact_id"]).values(
            operation_id=operation["id"]))
        task["operation_id"] = operation["id"]

    def progress(self, task, stage):
        with self.engine.begin() as conn:
            self._active(conn, task)
            self._changed(conn, task["artifact_id"])
            conn.execute(update(tasks).where(tasks.c.artifact_id == task["artifact_id"]).values(
                stage=stage, updated_at=self.state.uploads._now(conn)))
        self.flush(task["artifact_id"])

    def succeed(self, conn, task):
        row = self._active(conn, task)
        version = conn.scalar(select(knowledge_bases.c.version).where(
            knowledge_bases.c.namespace == task["namespace"]).with_for_update())
        if version != task["configuration_version"]:
            raise UploadConflict("Knowledge-base configuration changed; upload again")
        self._finish(conn, row, "succeeded")

    def fail(self, task, *, retry, error):
        with self.engine.begin() as conn:
            row = self._active(conn, task)
            if not retry:
                self._finish(conn, row, "failed", error)
            else:
                self._changed(conn, task["artifact_id"])
                conn.execute(update(tasks).where(tasks.c.artifact_id == task["artifact_id"]).values(
                    status="queued", stage="retry_wait", error=None,
                    updated_at=self.state.uploads._now(conn)))
        self.flush(task["artifact_id"])

    def terminal(self, artifact_id):
        with self.engine.connect() as conn:
            status = conn.scalar(select(tasks.c.status).where(tasks.c.artifact_id == artifact_id))
        return status is None or status in ("succeeded", "failed")

    def dead_letter(self, artifact_id):
        """Broker retry exhaustion is terminal; never overwrite a committed result."""
        validate_artifact_id(artifact_id)
        with self.engine.connect() as conn:
            namespace = conn.scalar(select(tasks.c.namespace).where(tasks.c.artifact_id == artifact_id))
        if namespace is None:
            logger.warning("Unknown document dead letter artifact_id=%s", artifact_id)
            return True
        with self.engine.begin() as conn:
            self.state.uploads._guard(conn, namespace)
            row = self._row(conn, artifact_id)
            if row["status"] in ("queued", "processing"):
                self._finish(conn, row, "failed", "Message retry limit reached", stage="dead_letter")
        self.flush(artifact_id)
        logger.warning("Document dead letter finalized artifact_id=%s", artifact_id)
        return True
