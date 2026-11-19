"""Chunk metadata registry: how a LightRAG hit maps back to Praxis evidence.

LightRAG returns chunk rows and no Praxis identifiers, so the sidecar keeps its
own registry of the metadata the Go backend needs to build an evidence entry:
``document_name``, ``page`` and ``heading``. Each row records the LightRAG chunk
id this sidecar derived, which is the primary lookup key; an MD5 content hash is
kept as a best-effort secondary key in case an upstream release changes the id
derivation.

Two deliberate boundaries:

* chunk **text** is never written to disk here. It already lives in LightRAG's
  storage; duplicating document text would grow the working directory and widen
  the leak surface. Only identifiers and location metadata are persisted.
* metadata survives a restart, so queries keep returning ``document_name``,
  ``page`` and ``heading`` without asking Go to re-send chunks.

Files are written atomically to ``<working_dir>/registry/<namespace>/<sha>.json``;
the hash-based filename keeps arbitrary Praxis document ids out of paths.
"""

from __future__ import annotations

import hashlib
import json
import os
import threading
import time
from uuid import uuid4
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

from .config import validate_namespace_name

REGISTRY_DIRECTORY = "registry"
REGISTRY_VERSION = 1


@dataclass(frozen=True)
class ChunkRef:
    """Metadata of one registered chunk.

    Attributes:
        chunk_id: Praxis chunk id.
        document_id: Praxis document id.
        document_name: Original document name.
        page: Source page, ``None`` when the source has no pages.
        heading: Section heading path, possibly empty.
        upload_version: Praxis upload version, if provided.
        lightrag_chunk_id: LightRAG chunk row id derived at insertion time.
        content_hash: MD5 of the chunk text as inserted (secondary lookup).
    """

    chunk_id: str
    document_id: str
    document_name: str
    page: int | None
    heading: str
    upload_version: int | None
    lightrag_chunk_id: str
    content_hash: str


@dataclass(frozen=True)
class DocumentMeta:
    """Registered metadata of one ingested document.

    Attributes:
        document_id: Praxis document id.
        document_name: Original document name.
        chunks: Registered chunks of the current upload version.
        updated_at: ISO timestamp of the last registration.
    """

    document_id: str
    document_name: str
    chunks: tuple[ChunkRef, ...]
    updated_at: str
    revision: str = ""
    publication_version: str = ""
    source_hash: str = ""


def utc_now() -> str:
    """Return the current UTC time as a second-precision ISO string.

    Returns:
        ISO-8601 timestamp, for example ``2026-01-01T00:00:00+00:00``.
    """

    return datetime.now(timezone.utc).isoformat(timespec="microseconds")


def _document_filename(document_id: str) -> str:
    """Return a filesystem-safe file name for a document id.

    Args:
        document_id: Arbitrary Praxis document id.

    Returns:
        SHA-256-based file name with a ``.json`` suffix.
    """

    digest = hashlib.sha256(document_id.encode("utf-8", errors="replace")).hexdigest()
    return f"{digest}.json"


class ChunkRegistry:
    """Persistent registry of Praxis chunk metadata, per namespace.

    The registry is loaded lazily per namespace and kept in memory as well as on
    disk. All methods are synchronous and guarded by a re-entrant lock, because
    they are cheap compared to the engine calls that surround them.
    """

    def __init__(self, working_dir: str | Path) -> None:
        """Create a registry rooted at ``<working_dir>/registry``.

        Args:
            working_dir: Service working directory (settings ``WORKING_DIR``).
        """

        self._root = Path(working_dir) / REGISTRY_DIRECTORY
        self._lock = threading.RLock()
        self._documents: dict[str, dict[str, DocumentMeta]] = {}
        self._by_lightrag: dict[str, dict[str, ChunkRef]] = {}
        self._by_chunk: dict[str, dict[str, ChunkRef]] = {}
        self._by_content: dict[str, dict[str, ChunkRef]] = {}
        self._loaded: set[str] = set()

    def documents(self, namespace: str) -> tuple[DocumentMeta, ...]:
        """Return every registered document of a namespace.

        Args:
            namespace: Praxis namespace.

        Returns:
            Documents ordered by filename for deterministic responses.
        """

        with self._lock:
            self._load(namespace)
            return tuple(self._documents.get(namespace, {}).values())

    def document(self, namespace: str, document_id: str) -> DocumentMeta | None:
        """Look up one registered document.

        Args:
            namespace: Praxis namespace.
            document_id: Praxis document id.

        Returns:
            The document metadata, or ``None`` when it is not registered.
        """

        with self._lock:
            self._load(namespace)
            return self._documents.get(namespace, {}).get(document_id)

    def remember(self, namespace: str, document: DocumentMeta) -> None:
        """Persist and index one document, replacing any previous metadata.

        Args:
            namespace: Praxis namespace.
            document: Metadata to store.

        Returns:
            ``None``.
        """

        with self._lock:
            self._load(namespace)
            documents = self._documents.setdefault(namespace, {})
            self._write(namespace, document)
            previous = documents.get(document.document_id)
            if previous is not None:
                self._unindex(namespace, previous)
            documents[document.document_id] = document
            self._index(namespace, document)

    def remove_document(self, namespace: str, document_id: str) -> bool:
        """Remove one document's metadata.

        Args:
            namespace: Praxis namespace.
            document_id: Praxis document id.

        Returns:
            ``True`` when a registered document was removed.
        """

        with self._lock:
            self._load(namespace)
            documents = self._documents.setdefault(namespace, {})
            self._path(namespace, document_id).unlink(missing_ok=True)
            document = documents.pop(document_id, None)
            if document is None:
                return False
            self._unindex(namespace, document)
            return True

    def clear(self, namespace: str) -> None:
        """Drop every document of a namespace, in memory and on disk.

        Args:
            namespace: Praxis namespace.

        Returns:
            ``None``.
        """

        with self._lock:
            self._load(namespace)
            self._documents.pop(namespace, None)
            self._by_lightrag.pop(namespace, None)
            self._by_chunk.pop(namespace, None)
            self._by_content.pop(namespace, None)
            directory = self._namespace_dir(namespace)
            if directory.exists():
                for path in directory.glob("*.json"):
                    path.unlink(missing_ok=True)
                try:
                    directory.rmdir()
                except OSError:
                    pass

    def find_by_lightrag_chunk(self, namespace: str, lightrag_chunk_id: str) -> ChunkRef | None:
        """Resolve a LightRAG chunk id to its Praxis metadata.

        Args:
            namespace: Praxis namespace.
            lightrag_chunk_id: Chunk id returned by the engine.

        Returns:
            The registered chunk, or ``None`` when the id is unknown.
        """

        with self._lock:
            self._load(namespace)
            return self._by_lightrag.get(namespace, {}).get(lightrag_chunk_id)

    def find_by_content_hash(self, namespace: str, digest: str) -> ChunkRef | None:
        """Resolve a chunk by the MD5 of its text (secondary lookup).

        Args:
            namespace: Praxis namespace.
            digest: MD5 hex digest of the returned chunk text.

        Returns:
            The registered chunk, or ``None`` when no chunk matches.
        """

        with self._lock:
            self._load(namespace)
            return self._by_content.get(namespace, {}).get(digest)

    def count_chunks(self, namespace: str) -> int:
        """Count registered chunks of a namespace.

        Args:
            namespace: Praxis namespace.

        Returns:
            Number of chunks across all registered documents.
        """

        with self._lock:
            self._load(namespace)
            return sum(len(meta.chunks) for meta in self._documents.get(namespace, {}).values())

    def _namespace_dir(self, namespace: str) -> Path:
        """Return (and validate) the directory holding one namespace's metadata.

        Args:
            namespace: Praxis namespace.

        Returns:
            ``<root>/<namespace>``.
        """

        validate_namespace_name(namespace)
        return self._root / namespace

    def _path(self, namespace: str, document_id: str) -> Path:
        """Return the JSON path of one document's metadata.

        Args:
            namespace: Praxis namespace.
            document_id: Praxis document id.

        Returns:
            ``<namespace dir>/<sha256(document_id)>.json``.
        """

        return self._namespace_dir(namespace) / _document_filename(document_id)

    def _load(self, namespace: str) -> None:
        """Load a namespace's metadata from disk exactly once per process.

        Args:
            namespace: Praxis namespace.

        Returns:
            ``None``.
        """

        if namespace in self._loaded:
            return
        self._loaded.add(namespace)
        directory = self._namespace_dir(namespace)
        if not directory.exists():
            return
        for path in sorted(directory.glob("*.json")):
            try:
                payload = json.loads(path.read_text(encoding="utf-8"))
                document = self._document_from_json(payload)
            except (OSError, ValueError, TypeError, KeyError):
                # A truncated metadata file must not block the whole namespace:
                # the document can be re-ingested by the Go backend.
                continue
            if document is None:
                continue
            self._documents.setdefault(namespace, {})[document.document_id] = document
            self._index(namespace, document)

    def _index(self, namespace: str, document: DocumentMeta) -> None:
        """Add a document's chunks to the in-memory lookup tables.

        Args:
            namespace: Praxis namespace.
            document: Document metadata to index.

        Returns:
            ``None``.
        """

        by_lightrag = self._by_lightrag.setdefault(namespace, {})
        by_chunk = self._by_chunk.setdefault(namespace, {})
        by_content = self._by_content.setdefault(namespace, {})
        for chunk in document.chunks:
            by_lightrag[chunk.lightrag_chunk_id] = chunk
            by_chunk[chunk.chunk_id] = chunk
            by_content.setdefault(chunk.content_hash, chunk)

    def _unindex(self, namespace: str, document: DocumentMeta) -> None:
        """Remove a document's chunks from the in-memory lookup tables.

        Args:
            namespace: Praxis namespace.
            document: Document metadata being replaced or removed.

        Returns:
            ``None``.
        """

        by_lightrag = self._by_lightrag.get(namespace, {})
        by_chunk = self._by_chunk.get(namespace, {})
        by_content = self._by_content.get(namespace, {})
        for chunk in document.chunks:
            by_lightrag.pop(chunk.lightrag_chunk_id, None)
            by_chunk.pop(chunk.chunk_id, None)
            if by_content.get(chunk.content_hash) is chunk:
                by_content.pop(chunk.content_hash, None)

    def _write(self, namespace: str, document: DocumentMeta) -> None:
        """Atomically persist one document's metadata.

        Args:
            namespace: Praxis namespace.
            document: Document metadata to write.

        Returns:
            ``None``.
        """

        directory = self._namespace_dir(namespace)
        directory.mkdir(parents=True, exist_ok=True)
        path = self._path(namespace, document.document_id)
        temporary = path.with_suffix(f".{uuid4().hex}.tmp")
        temporary.write_text(
            json.dumps(self._document_to_json(document), ensure_ascii=False),
            encoding="utf-8",
        )
        # Windows readers (including filesystem indexing) can briefly deny
        # replacement of the just-written pending registration.
        for attempt in range(6):
            try:
                os.replace(temporary, path)
                break
            except PermissionError:
                if os.name != "nt" or attempt == 5:
                    temporary.unlink(missing_ok=True)
                    raise
                time.sleep(0.01 * 2**attempt)

    @staticmethod
    def _document_to_json(document: DocumentMeta) -> dict:
        """Serialize metadata into the on-disk JSON shape.

        Args:
            document: Document metadata.

        Returns:
            JSON-serializable dictionary (never contains chunk text).
        """

        return {
            "version": REGISTRY_VERSION,
            "document_id": document.document_id,
            "document_name": document.document_name,
            "updated_at": document.updated_at,
            "revision": document.revision,
            "publication_version": document.publication_version,
            "source_hash": document.source_hash,
            "chunks": [
                {
                    "chunk_id": chunk.chunk_id,
                    "lightrag_chunk_id": chunk.lightrag_chunk_id,
                    "content_hash": chunk.content_hash,
                    "page": chunk.page,
                    "heading": chunk.heading,
                    "upload_version": chunk.upload_version,
                }
                for chunk in document.chunks
            ],
        }

    @staticmethod
    def _document_from_json(payload: dict) -> DocumentMeta | None:
        """Rebuild metadata from the on-disk JSON shape.

        Args:
            payload: Parsed JSON object.

        Returns:
            Document metadata, or ``None`` when required fields are missing.
        """

        if not isinstance(payload, dict):
            return None
        document_id = payload.get("document_id")
        document_name = payload.get("document_name")
        if not isinstance(document_id, str) or not isinstance(document_name, str):
            return None
        chunks: list[ChunkRef] = []
        for entry in payload.get("chunks") or []:
            if not isinstance(entry, dict):
                continue
            chunk_id = entry.get("chunk_id")
            lightrag_chunk_id = entry.get("lightrag_chunk_id")
            content_hash = entry.get("content_hash")
            if not all(isinstance(value, str) for value in (chunk_id, lightrag_chunk_id, content_hash)):
                continue
            page = entry.get("page")
            upload_version = entry.get("upload_version")
            chunks.append(
                ChunkRef(
                    chunk_id=chunk_id,
                    document_id=document_id,
                    document_name=document_name,
                    page=page if isinstance(page, int) else None,
                    heading=entry.get("heading") if isinstance(entry.get("heading"), str) else "",
                    upload_version=upload_version if isinstance(upload_version, int) else None,
                    lightrag_chunk_id=lightrag_chunk_id,
                    content_hash=content_hash,
                )
            )
        updated_at = payload.get("updated_at")
        return DocumentMeta(
            document_id=document_id,
            document_name=document_name,
            chunks=tuple(chunks),
            updated_at=updated_at if isinstance(updated_at, str) else utc_now(),
            revision=str(payload.get("revision") or ""),
            publication_version=str(payload.get("publication_version") or ""),
            source_hash=str(payload.get("source_hash") or ""),
        )
