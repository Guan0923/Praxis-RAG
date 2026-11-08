"""Durable MySQL document-ingest jobs with a transactional outbox.

MySQL is the single source of truth for an asynchronous upload. Submission
writes the complete job (including the file bytes) and one outbox row inside
the *same* transaction, so a committed upload can never be missing from the
outbox -- a Debezium binlog reader (production) or :class:`DocumentOutboxRelay`
(SQLite/local development) turns that row into a RocketMQ notification.

The queue therefore carries a small notification only: no file bytes, no
512 KiB chunking, no Redis reassembly state and no Redis task state. Redis is
no longer part of the document delivery path at all.
"""

from __future__ import annotations

import hashlib
import logging
import os
import re
import time
from pathlib import Path, PureWindowsPath
from uuid import uuid4

from sqlalchemy import delete, insert, select, update
from sqlalchemy.exc import IntegrityError

from .schema import document_job_outbox, document_jobs, documents, knowledge_bases, upload_operations
from .uploads import UploadConflict

logger = logging.getLogger(__name__)

MAX_FILE_BYTES = 10 * 1024 * 1024
ENVELOPE_SCHEMA = 3

QUEUED = "queued"
PROCESSING = "processing"
SUCCEEDED = "succeeded"
FAILED = "failed"
ACTIVE_STATUSES = (QUEUED, PROCESSING)
TERMINAL_STATUSES = (SUCCEEDED, FAILED)

_NAMESPACE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,63}")


def _settings_float(name, default):
    raw = os.getenv(name, "").strip()
    value = float(raw) if raw else default
    if value <= 0:
        raise ValueError(f"{name} must be positive")
    return value


def job_lease_seconds():
    return _settings_float("RAG_DOCUMENT_JOB_LEASE_SECONDS", 300.0)


def heartbeat_seconds():
    return _settings_float("RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS", 20.0)


def sweep_after_seconds():
    return _settings_float("RAG_DOCUMENT_JOB_SWEEP_SECONDS", 120.0)


def outbox_retention_seconds():
    return _settings_float("RAG_DOCUMENT_OUTBOX_RETENTION_SECONDS", 7 * 24 * 3600.0)


def terminal_retention_seconds():
    return _settings_float("RAG_DOCUMENT_JOB_RETENTION_SECONDS", 30 * 24 * 3600.0)


def validate_artifact_id(value):
    from uuid import UUID

    try:
        if not isinstance(value, str) or str(UUID(value)) != value:
            raise ValueError
    except (ValueError, AttributeError):
        raise ValueError("artifact_id must be a canonical UUID") from None
    return value


def _validate_name(name):
    from ..service_knowledge import SUPPORTED_EXTENSIONS, KnowledgeDocumentStore

    if (not isinstance(name, str) or KnowledgeDocumentStore.sanitize_name(name) != name
            or PureWindowsPath(name).is_reserved()
            or Path(name).suffix.casefold() not in SUPPORTED_EXTENSIONS):
        raise ValueError("Invalid document filename or file type")


def _validate_namespace(namespace):
    if not isinstance(namespace, str) or not _NAMESPACE.fullmatch(namespace):
        raise ValueError("Invalid document namespace")


def document_job_envelope(job):
    """The notification published to RocketMQ. Deliberately carries no content."""

    payload = {
        "schema": ENVELOPE_SCHEMA,
        "artifact_id": job["artifact_id"],
        "namespace": job["namespace"],
        "name": job["name"],
        "created_at": float(job.get("created_at") or 0.0),
    }
    return validate_document_envelope(payload)


def validate_document_envelope(payload):
    """Accept the native schema-3 notification or a raw Debezium change event.

    Debezium publishes the outbox row as a standard change event
    ``{"before": ..., "after": {...}, "op": "c", ...}``. Reading ``after`` keeps
    the pipeline usable without an extra SMT/script transform on the connector.
    """

    if not isinstance(payload, dict):
        raise ValueError("Invalid document message")
    if payload.get("schema") == ENVELOPE_SCHEMA:
        value = payload
    else:
        after = payload.get("after")
        if isinstance(after, dict) and payload.get("op") in {"c", "r", "u"}:
            value = {"schema": ENVELOPE_SCHEMA, **{key: after.get(key) for key in (
                "artifact_id", "namespace", "name", "created_at")}}
        else:
            raise ValueError("Invalid document message schema")
    validate_artifact_id(value.get("artifact_id"))
    _validate_namespace(value.get("namespace"))
    _validate_name(value.get("name"))
    created_at = value.get("created_at", 0.0)
    if type(created_at) not in (int, float) or created_at < 0:
        raise ValueError("Invalid document message timestamp")
    return {"schema": ENVELOPE_SCHEMA, "artifact_id": value["artifact_id"],
            "namespace": value["namespace"], "name": value["name"],
            "created_at": float(created_at)}


class DocumentJobs:
    """Repository for durable ingest jobs and their transactional outbox."""

    def __init__(self, state):
        self.state = state
        self.engine = state.engine

    # -- identity helpers ----------------------------------------------------
    @staticmethod
    def _name_key(namespace, name):
        return hashlib.sha256(f"{namespace}:{name.casefold()}".encode()).hexdigest()

    def _guard(self, conn, namespace):
        # Reuse the journal's persistent namespace row: it serialises every
        # submission for one namespace without a second coordination table.
        self.state.uploads._guard(conn, namespace)

    # -- submission ----------------------------------------------------------
    def submit(self, namespace, name, content, idempotency_key=None):
        """Persist the complete job and its outbox notification atomically."""

        from ..service_knowledge import KnowledgeValidationError

        try:
            _validate_name(name)
        except ValueError as exc:
            raise KnowledgeValidationError(str(exc)) from None
        if not isinstance(content, bytes) or not 0 < len(content) <= MAX_FILE_BYTES:
            raise KnowledgeValidationError("File must be non-empty and no larger than 10 MiB")
        if idempotency_key is not None and (
                not isinstance(idempotency_key, str) or not 1 <= len(idempotency_key) <= 128):
            raise KnowledgeValidationError("idempotency_key must be a string of 1 to 128 characters")
        digest = hashlib.sha256(content).hexdigest()
        name_key = self._name_key(namespace, name)
        artifact_id = str(uuid4())
        event_id = uuid4().hex
        now = time.time()
        try:
            with self.engine.begin() as conn:
                return self._insert_job(conn, namespace, name, content, idempotency_key,
                                        digest, name_key, artifact_id, event_id, now)
        except IntegrityError as exc:
            # Two concurrent retries with the same key: the loser replays the
            # winner's job instead of surfacing a raw database error.
            existing = self._by_idempotency_key(namespace, idempotency_key)
            if existing is None:
                raise UploadConflict("Upload admission conflicted; retry with the same idempotency_key") from exc
            if existing["name"] != name or existing["sha256"] != digest:
                raise UploadConflict("Idempotency key has different file input") from exc
            return {"artifact_id": existing["artifact_id"], "status": existing["status"],
                    "reused": True, "event_id": None}

    def _by_idempotency_key(self, namespace, idempotency_key):
        if idempotency_key is None:
            return None
        with self.engine.connect() as conn:
            row = conn.execute(select(document_jobs).where(
                document_jobs.c.namespace == namespace,
                document_jobs.c.idempotency_key == idempotency_key)).mappings().first()
        return dict(row) if row is not None else None

    def _insert_job(self, conn, namespace, name, content, idempotency_key,
                    digest, name_key, artifact_id, event_id, now):
        """Insert the job and its outbox row inside the caller's transaction."""

        self._guard(conn, namespace)
        version = conn.scalar(select(knowledge_bases.c.version).where(
            knowledge_bases.c.namespace == namespace).with_for_update())
        if version is None:
            raise ValueError("Knowledge base is not configured")
        if idempotency_key is not None:
            existing = conn.execute(select(document_jobs).where(
                document_jobs.c.namespace == namespace,
                document_jobs.c.idempotency_key == idempotency_key,
            ).with_for_update()).mappings().first()
            if existing is not None:
                if existing["name"] != name or existing["sha256"] != digest:
                    raise UploadConflict("Idempotency key has different file input")
                return {"artifact_id": existing["artifact_id"], "status": existing["status"],
                        "reused": True, "event_id": None}
        published = conn.scalars(select(documents.c.name).where(
            documents.c.namespace == namespace, documents.c.del_flag.is_(False)))
        if any(other.casefold() == name.casefold() for other in published):
            raise UploadConflict("Document already exists; use update_document")
        pending = conn.execute(select(document_jobs.c.artifact_id).where(
            document_jobs.c.namespace == namespace,
            document_jobs.c.name_key == name_key,
            document_jobs.c.status.in_(ACTIVE_STATUSES),
        ).limit(1)).first()
        if pending is not None:
            raise UploadConflict("A document with this filename is already queued")
        conn.execute(insert(document_jobs).values(
            artifact_id=artifact_id, namespace=namespace, name=name, name_key=name_key,
            content=content, size=len(content), sha256=digest,
            configuration_version=version, idempotency_key=idempotency_key,
            status=QUEUED, stage=QUEUED, attempts=0, owner=None, execution_id=None,
            lease_until=0.0, operation_id=None, retry_at=now, error=None,
            created_at=now, updated_at=now))
        conn.execute(insert(document_job_outbox).values(
            id=event_id, artifact_id=artifact_id, namespace=namespace, name=name,
            status="PENDING", attempts=0, retry_at=now, created_at=now,
            published_at=None, last_error=None))
        logger.info("Document job accepted artifact_id=%s", artifact_id)
        return {"artifact_id": artifact_id, "status": QUEUED, "reused": False,
                "event_id": event_id}

    # -- queries -------------------------------------------------------------
    def get(self, artifact_id, namespace=None):
        validate_artifact_id(artifact_id)
        query = select(document_jobs).where(document_jobs.c.artifact_id == artifact_id)
        if namespace is not None:
            query = query.where(document_jobs.c.namespace == namespace)
        with self.engine.connect() as conn:
            row = conn.execute(query).mappings().first()
        return dict(row) if row is not None else None

    def status(self, namespace, artifact_id):
        validate_artifact_id(artifact_id)
        row = self.get(artifact_id, namespace)
        if row is None:
            raise LookupError("Document upload not found")
        return {key: row.get(key) for key in ("artifact_id", "status", "stage", "error")}

    def terminal(self, envelope):
        artifact_id = envelope["artifact_id"] if isinstance(envelope, dict) else envelope
        try:
            validate_artifact_id(artifact_id)
        except ValueError:
            return True
        row = self.get(artifact_id)
        return row is None or row["status"] in TERMINAL_STATUSES

    # -- worker lease --------------------------------------------------------
    def claim(self, envelope):
        """Take the execution lease. Returns the job with content, or ``None``."""

        envelope = validate_document_envelope(envelope)
        now = time.time()
        owner, execution_id = uuid4().hex, uuid4().hex
        with self.engine.begin() as conn:
            row = conn.execute(select(document_jobs).where(
                document_jobs.c.artifact_id == envelope["artifact_id"],
                document_jobs.c.namespace == envelope["namespace"],
            ).with_for_update()).mappings().first()
            if row is None or row["status"] in TERMINAL_STATUSES:
                return None
            if row["status"] == PROCESSING and float(row["lease_until"] or 0.0) > now:
                return None
            attempts = int(row["attempts"] or 0) + 1
            conn.execute(update(document_jobs).where(
                document_jobs.c.artifact_id == envelope["artifact_id"]).values(
                status=PROCESSING, stage="receiving", owner=owner, execution_id=execution_id,
                lease_until=now + job_lease_seconds(), attempts=attempts,
                error=None, updated_at=now))
        task = dict(row)
        task.update(status=PROCESSING, stage="receiving", owner=owner,
                    execution_id=execution_id, attempts=attempts,
                    lease_until=now + job_lease_seconds(),
                    content=bytes(row["content"] or b""))
        return task

    def heartbeat(self, task):
        with self.engine.begin() as conn:
            conn.execute(update(document_jobs).where(
                document_jobs.c.artifact_id == task["artifact_id"],
                document_jobs.c.execution_id == task["execution_id"],
                document_jobs.c.status == PROCESSING,
            ).values(lease_until=time.time() + job_lease_seconds()))

    def progress(self, task, stage):
        with self.engine.begin() as conn:
            changed = conn.execute(update(document_jobs).where(
                document_jobs.c.artifact_id == task["artifact_id"],
                document_jobs.c.execution_id == task["execution_id"],
                document_jobs.c.status == PROCESSING,
            ).values(stage=stage, operation_id=task.get("operation_id"),
                     lease_until=time.time() + job_lease_seconds(), updated_at=time.time()))
            if not changed.rowcount:
                raise UploadConflict("Document execution is no longer current")
        task["stage"] = stage

    def bind(self, conn, task, operation):
        # The operation is durable; task linkage is persisted by progress().
        task["operation_id"] = operation["id"]

    def validate_publication(self, conn, task):
        version = conn.scalar(select(knowledge_bases.c.version).where(
            knowledge_bases.c.namespace == task["namespace"]).with_for_update())
        if version != task["configuration_version"]:
            raise UploadConflict("Knowledge-base configuration changed; upload again")

    # -- publication reconciliation -----------------------------------------
    def committed_operation(self, task):
        prefix = f"document-task:{task['artifact_id']}:"
        with self.engine.connect() as conn:
            row = conn.execute(select(upload_operations).where(
                upload_operations.c.namespace == task["namespace"],
                upload_operations.c.idempotency_key.startswith(prefix),
                upload_operations.c.status == "COMMITTED")).mappings().first()
        if row is None:
            return None
        operation = self.state.uploads.get(row["id"])
        result = operation["result"]
        if (operation["name"] != task["name"] or result.get("sha256") != task["sha256"]
                or result.get("size") != task["size"]
                or result.get("task_configuration_version") != task["configuration_version"]):
            raise UploadConflict("Committed upload input differs")
        return operation

    def reconcile(self, task):
        """Adopt an already committed journal publication (crash/retry replay)."""

        operation = self.committed_operation(task)
        if operation is None:
            return False
        self._finish(task, SUCCEEDED, operation_id=operation["id"])
        return True

    def execution_busy(self, task):
        with self.engine.connect() as conn:
            return conn.scalar(select(upload_operations.c.id).where(
                upload_operations.c.namespace == task["namespace"],
                upload_operations.c.idempotency_key.startswith(f"document-task:{task['artifact_id']}:"),
                upload_operations.c.status.in_(("INTENT", "RUNNING"))).limit(1)) is not None

    def succeed(self, conn, task):
        if not self.reconcile(task):
            raise UploadConflict("Document publication is not committed")

    # -- terminal transitions -----------------------------------------------
    def _finish(self, task, status, error=None, operation_id=None):
        now = time.time()
        with self.engine.begin() as conn:
            row = conn.execute(select(document_jobs).where(
                document_jobs.c.artifact_id == task["artifact_id"]).with_for_update()).mappings().first()
            if row is None:
                return None
            if row["status"] == SUCCEEDED:
                return dict(row)
            if (status != SUCCEEDED and task.get("execution_id")
                    and row["execution_id"] != task["execution_id"]):
                raise UploadConflict("Document execution is no longer current")
            values = {"status": status,
                      "stage": "completed" if status == SUCCEEDED else "failed",
                      "error": error, "owner": None, "lease_until": 0.0, "updated_at": now}
            if status == SUCCEEDED:
                # The source bytes live in the blobs bucket now; drop the
                # duplicate copy carried by the in-flight job row.
                values["content"] = None
            if operation_id is not None:
                values["operation_id"] = operation_id
            elif task.get("operation_id"):
                values["operation_id"] = task["operation_id"]
            conn.execute(update(document_jobs).where(
                document_jobs.c.artifact_id == task["artifact_id"]).values(**values))
        logger.info("Document job terminal artifact_id=%s status=%s", task["artifact_id"], status)
        return None

    def fail(self, task, *, retry, error):
        if self.reconcile(task):
            return
        if not retry:
            self._finish(task, FAILED, error)
            return
        now = time.time()
        with self.engine.begin() as conn:
            conn.execute(update(document_jobs).where(
                document_jobs.c.artifact_id == task["artifact_id"],
                document_jobs.c.execution_id == task["execution_id"],
                document_jobs.c.status == PROCESSING,
            ).values(status=QUEUED, stage="retry_wait", owner=None, lease_until=0.0,
                     retry_at=now + sweep_after_seconds(), updated_at=now))

    def dead_letter(self, envelope):
        envelope = validate_document_envelope(envelope)
        row = self.get(envelope["artifact_id"], envelope["namespace"])
        if row is None or row["status"] in TERMINAL_STATUSES:
            return True
        if row["status"] == PROCESSING and float(row["lease_until"] or 0.0) > time.time():
            return False
        if self.reconcile(row):
            return True
        if self.execution_busy(row):
            return False
        self._finish(row, FAILED, "Message retry limit reached")
        return True

    # -- outbox maintenance --------------------------------------------------
    def outbox_event(self, event_id):
        with self.engine.connect() as conn:
            row = conn.execute(select(document_job_outbox).where(
                document_job_outbox.c.id == event_id)).mappings().first()
        return dict(row) if row is not None else None

    def pending_outbox(self, limit=50):
        with self.engine.connect() as conn:
            return [dict(row) for row in conn.execute(select(document_job_outbox).where(
                document_job_outbox.c.status == "PENDING",
                document_job_outbox.c.retry_at <= time.time(),
            ).order_by(document_job_outbox.c.created_at).limit(limit)).mappings()]

    def mark_published(self, event_id, error=None):
        now = time.time()
        with self.engine.begin() as conn:
            if error is None:
                conn.execute(update(document_job_outbox).where(
                    document_job_outbox.c.id == event_id).values(
                    status="PUBLISHED", published_at=now, last_error=None))
            else:
                conn.execute(update(document_job_outbox).where(
                    document_job_outbox.c.id == event_id).values(
                    attempts=document_job_outbox.c.attempts + 1,
                    retry_at=now + 5, last_error=error[:128]))

    def sweep(self, limit=50):
        """Re-emit notifications for jobs stranded without a pending event.

        Covers messages lost to broker DLQ, a Debezium offset reset, or a relay
        that was down while the row was already marked published. A new outbox
        row is an ordinary INSERT, so Debezium picks it up like any other.
        """

        cutoff = time.time() - sweep_after_seconds()
        now = time.time()
        with self.engine.begin() as conn:
            recently_notified = select(document_job_outbox.c.id).where(
                document_job_outbox.c.artifact_id == document_jobs.c.artifact_id,
                document_job_outbox.c.created_at > cutoff,
            ).exists()
            stuck = conn.execute(select(document_jobs).where(
                document_jobs.c.status == QUEUED,
                document_jobs.c.updated_at <= cutoff,
                ~recently_notified,
            ).order_by(document_jobs.c.updated_at).limit(limit)).mappings().all()
            expired = conn.execute(select(document_jobs).where(
                document_jobs.c.status == PROCESSING,
                document_jobs.c.lease_until <= now,
            ).limit(limit)).mappings().all()
            reemitted = 0
            for row in stuck:
                conn.execute(insert(document_job_outbox).values(
                    id=uuid4().hex, artifact_id=row["artifact_id"], namespace=row["namespace"],
                    name=row["name"], status="PENDING", attempts=0, retry_at=now,
                    created_at=now, published_at=None, last_error=None))
                conn.execute(update(document_jobs).where(
                    document_jobs.c.artifact_id == row["artifact_id"]).values(updated_at=now))
                reemitted += 1
            for row in expired:
                conn.execute(update(document_jobs).where(
                    document_jobs.c.artifact_id == row["artifact_id"]).values(
                    status=QUEUED, stage="retry_wait", owner=None, lease_until=0.0,
                    retry_at=now, updated_at=now))
                conn.execute(insert(document_job_outbox).values(
                    id=uuid4().hex, artifact_id=row["artifact_id"], namespace=row["namespace"],
                    name=row["name"], status="PENDING", attempts=0, retry_at=now,
                    created_at=now, published_at=None, last_error=None))
                reemitted += 1
        if reemitted:
            logger.info("Document outbox sweep re-emitted notifications=%d", reemitted)
        return reemitted

    def gc_terminal(self, retain_seconds=None):
        """Delete finished job rows past their retention window."""

        retention = (terminal_retention_seconds() if retain_seconds is None
                     else float(retain_seconds))
        cutoff = time.time() - retention
        with self.engine.begin() as conn:
            result = conn.execute(delete(document_jobs).where(
                document_jobs.c.status.in_(TERMINAL_STATUSES),
                document_jobs.c.updated_at < cutoff))
        return result.rowcount or 0

    def gc_outbox(self, max_age=None):
        cutoff = time.time() - (outbox_retention_seconds() if max_age is None else max_age)
        with self.engine.begin() as conn:
            result = conn.execute(delete(document_job_outbox).where(
                document_job_outbox.c.created_at < cutoff))
        return result.rowcount or 0
