"""Redis upload admission, bounded payload assembly and transient execution state."""

from __future__ import annotations

import base64
import binascii
import hashlib
import logging
import re
from pathlib import Path, PureWindowsPath
from uuid import UUID, uuid4

from sqlalchemy import select

from .schema import documents, knowledge_bases, upload_operations
from .transient import TransientStore
from .uploads import UploadConflict

logger = logging.getLogger(__name__)

CHUNK_BYTES = 512 * 1024
MAX_FILE_BYTES = 10 * 1024 * 1024
_METADATA = ("artifact_id", "namespace", "name", "sha256", "size", "configuration_version", "chunk_count")


def validate_artifact_id(value):
    try:
        if not isinstance(value, str) or str(UUID(value)) != value:
            raise ValueError
    except (ValueError, AttributeError):
        raise ValueError("artifact_id must be a canonical UUID") from None
    return value


def validate_document_envelope(payload):
    if not isinstance(payload, dict) or payload.get("schema") != 2:
        raise ValueError("Invalid document message schema")
    validate_artifact_id(payload.get("artifact_id"))
    namespace, name = payload.get("namespace"), payload.get("name")
    if not isinstance(namespace, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,63}", namespace):
        raise ValueError("Invalid document namespace")
    _validate_name(name)
    size, count, index, version = (payload.get(key) for key in (
        "size", "chunk_count", "chunk_index", "configuration_version"))
    if (type(size) is not int or not 0 < size <= MAX_FILE_BYTES
            or type(count) is not int or count != (size + CHUNK_BYTES - 1) // CHUNK_BYTES
            or type(index) is not int or not 0 <= index < count
            or type(version) is not int or version < 1
            or not isinstance(payload.get("sha256"), str)
            or not re.fullmatch(r"[0-9a-f]{64}", payload["sha256"])):
        raise ValueError("Invalid document message metadata")
    data = payload.get("data")
    if not isinstance(data, str) or len(data) > 4 * ((CHUNK_BYTES + 2) // 3):
        raise ValueError("Invalid document chunk")
    try:
        decoded = base64.b64decode(data, validate=True)
    except (ValueError, binascii.Error):
        raise ValueError("Invalid document chunk") from None
    expected = CHUNK_BYTES if index < count - 1 else size - index * CHUNK_BYTES
    if len(decoded) != expected:
        raise ValueError("Invalid document chunk size")
    return {"schema": 2, **{key: payload[key] for key in _METADATA},
            "chunk_index": index, "data": data}


def _validate_name(name):
    from ..service_knowledge import SUPPORTED_EXTENSIONS, KnowledgeDocumentStore

    if (not isinstance(name, str) or KnowledgeDocumentStore.sanitize_name(name) != name
            or PureWindowsPath(name).is_reserved() or Path(name).suffix.casefold() not in SUPPORTED_EXTENSIONS):
        raise ValueError("Invalid document filename or file type")


def document_envelopes(task):
    content = task["content"]
    metadata = {key: task[key] for key in _METADATA if key != "chunk_count"}
    metadata["chunk_count"] = (len(content) + CHUNK_BYTES - 1) // CHUNK_BYTES
    for index in range(metadata["chunk_count"]):
        yield validate_document_envelope({"schema": 2, **metadata, "chunk_index": index,
            "data": base64.b64encode(content[index * CHUNK_BYTES:(index + 1) * CHUNK_BYTES]).decode("ascii")})


class DocumentTasks:
    def __init__(self, state):
        self.state = state
        self.engine = state.engine
        self.transient = TransientStore(state)

    @staticmethod
    def _identity(namespace, value):
        return hashlib.sha256(f"{namespace}:{value}".encode()).hexdigest()

    def flush(self, artifact_id):
        """Compatibility hook; transient writes are synchronous."""

    def status(self, namespace, artifact_id):
        validate_artifact_id(artifact_id)
        row = self.transient.get("document", artifact_id)
        if row is None or row["namespace"] != namespace:
            raise LookupError("Document upload not found")
        return {key: row.get(key) for key in ("artifact_id", "status", "stage", "error")}

    def submit(self, namespace, name, content, idempotency_key=None):
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
        key = self._identity(namespace, idempotency_key) if idempotency_key is not None else None
        reservation = {"artifact_id": str(uuid4()), "name": name, "sha256": digest}
        if key:
            self.transient.put("document_idempotency", key, reservation, nx=True)
            reservation = self.transient.get("document_idempotency", key)
            if reservation is None:
                raise UploadConflict("Upload admission expired; retry")
            if reservation["name"] != name or reservation["sha256"] != digest:
                raise UploadConflict("Idempotency key has different file input")
        artifact_id = reservation["artifact_id"]
        existing = self.transient.get("document", artifact_id)
        if existing:
            return {**existing, "content": content}
        with self.engine.begin() as conn:
            self.state.uploads._guard(conn, namespace)
            version = conn.scalar(select(knowledge_bases.c.version).where(
                knowledge_bases.c.namespace == namespace).with_for_update())
            if version is None:
                raise ValueError("Knowledge base is not configured")
            names = conn.scalars(select(documents.c.name).where(
                documents.c.namespace == namespace, documents.c.del_flag.is_(False)))
            if any(other.casefold() == name.casefold() for other in names):
                raise UploadConflict("Document already exists; use update_document")
        name_key = self._identity(namespace, name.casefold())
        owner = {"artifact_id": artifact_id}
        self.transient.update("document_name", name_key, lambda row: owner if not row else row)
        if self.transient.get("document_name", name_key) != owner:
            raise UploadConflict("A document with this filename is already queued")
        row = {"artifact_id": artifact_id, "namespace": namespace, "name": name,
               "name_key": name_key, "sha256": digest, "size": len(content),
               "configuration_version": version, "status": "queued", "stage": "sending",
               "error": None, "accepted": False}
        self.transient.put("document", artifact_id, row, nx=True)
        return {**self.transient.get("document", artifact_id), "content": content}

    def accepted(self, task):
        def change(row):
            if row is None:
                raise UploadConflict("Upload admission expired; retry with the same idempotency_key")
            return {**row, "accepted": True, "stage": "queued" if row["status"] == "queued" else row["stage"]}
        self.transient.update("document", task["artifact_id"], change)

    def assemble(self, envelope):
        envelope = validate_document_envelope(envelope)
        artifact_id = envelope["artifact_id"]
        self.terminal(envelope)
        metadata = {key: envelope[key] for key in _METADATA}
        def collect(row):
            if row is not None and row["metadata"] != metadata:
                raise ValueError("Conflicting document message metadata")
            chunks = dict(row["chunks"]) if row else {}
            index = str(envelope["chunk_index"])
            if index in chunks and chunks[index] != envelope["data"]:
                raise ValueError("Conflicting document chunk")
            chunks[index] = envelope["data"]
            return {"metadata": metadata, "chunks": chunks}
        assembled = self.transient.update("document_chunks", artifact_id, collect)
        if len(assembled["chunks"]) != metadata["chunk_count"]:
            return None
        content = b"".join(base64.b64decode(assembled["chunks"][str(i)], validate=True)
                           for i in range(metadata["chunk_count"]))
        if len(content) != metadata["size"] or hashlib.sha256(content).hexdigest() != metadata["sha256"]:
            raise ValueError("Document checksum mismatch")
        return {**metadata, "content": content}

    def _committed(self, task):
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
        operation = self._committed(task)
        if operation is None:
            return False
        self._finish(task, "succeeded", operation_id=operation["id"])
        return True

    def execution_busy(self, task):
        with self.engine.connect() as conn:
            return conn.scalar(select(upload_operations.c.id).where(
                upload_operations.c.namespace == task["namespace"],
                upload_operations.c.idempotency_key.startswith(f"document-task:{task['artifact_id']}:"),
                upload_operations.c.status.in_(("INTENT", "RUNNING"))).limit(1)) is not None

    def claim(self, task):
        artifact_id = task["artifact_id"]
        execution_id = uuid4().hex
        def claim(row):
            if row and row["status"] in ("succeeded", "failed"):
                return None
            if row and any(row.get(key) != task[key] for key in (
                    "namespace", "name", "sha256", "configuration_version")):
                raise UploadConflict("Document task metadata differs")
            return {**{key: value for key, value in task.items() if key != "content"},
                    **(row or {}), "status": "processing", "stage": "receiving",
                    "execution_id": execution_id, "error": None}
        row = self.transient.update("document", artifact_id, claim)
        return {**row, "content": task["content"]} if row else None

    def bind(self, conn, task, operation):
        # The operation is durable; task linkage is written only after begin commits.
        task["operation_id"] = operation["id"]

    def progress(self, task, stage):
        def change(row):
            if row is None or row.get("execution_id") != task.get("execution_id") or row["status"] != "processing":
                raise UploadConflict("Document execution is no longer current")
            return {**row, "stage": stage, "operation_id": task.get("operation_id")}
        self.transient.update("document", task["artifact_id"], change)

    def validate_publication(self, conn, task):
        version = conn.scalar(select(knowledge_bases.c.version).where(
            knowledge_bases.c.namespace == task["namespace"]).with_for_update())
        if version != task["configuration_version"]:
            raise UploadConflict("Knowledge-base configuration changed; upload again")

    def _finish(self, task, status, error=None, **extra):
        def finish(row):
            if row and any(row.get(key) != task[key] for key in (
                    "namespace", "name", "sha256", "size", "configuration_version")):
                raise UploadConflict("Document task metadata differs")
            if row and row["status"] == "succeeded":
                return row
            if (status != "succeeded" and row and task.get("execution_id")
                    and row.get("execution_id") != task["execution_id"]):
                raise UploadConflict("Document execution is no longer current")
            return {**{key: task[key] for key in _METADATA if key in task},
                    **(row or {}), **extra, "status": status,
                    "stage": "completed" if status == "succeeded" else "failed", "error": error}
        result = self.transient.update("document", task["artifact_id"], finish)
        logger.info("Document terminal artifact_id=%s status=%s", task["artifact_id"], result["status"])
        name_key = self._identity(task["namespace"], task["name"].casefold())
        self.transient.update("document_name", name_key, lambda row:
            {} if row and row.get("artifact_id") == task["artifact_id"] else None)

    def succeed(self, conn, task):
        if not self.reconcile(task):
            raise UploadConflict("Document publication is not committed")

    def fail(self, task, *, retry, error):
        if self.reconcile(task):
            return
        if not retry:
            self._finish(task, "failed", error)
        else:
            self.transient.update("document", task["artifact_id"], lambda row:
                {**row, "status": "queued", "stage": "retry_wait", "error": None}
                if row and row.get("execution_id") == task.get("execution_id")
                and row["status"] == "processing" else None)

    def terminal(self, task):
        artifact_id = task["artifact_id"] if isinstance(task, dict) else task
        validate_artifact_id(artifact_id)
        row = self.transient.get("document", artifact_id)
        if row and isinstance(task, dict) and any(row.get(key) != task[key] for key in (
                "namespace", "name", "sha256", "size", "configuration_version")):
            raise UploadConflict("Document task metadata differs")
        return row is not None and row["status"] in ("succeeded", "failed")

    def dead_letter(self, envelope):
        task = validate_document_envelope(envelope)
        token = self.transient.lock("document_execution", task["artifact_id"], 300)
        if token is None:
            return False
        try:
            if self.reconcile(task) or self.terminal(task):
                return True
            # A live durable operation can outlast its Redis execution lock.
            if self.execution_busy(task):
                return False
            self._finish(task, "failed", "Message retry limit reached")
            return True
        finally:
            self.transient.unlock("document_execution", task["artifact_id"], token)
