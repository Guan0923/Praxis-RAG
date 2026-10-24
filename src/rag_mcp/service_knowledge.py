"""Tenant-confined source storage and indexing owned by the RAG service."""

from __future__ import annotations

import logging
import hashlib
import io
import os
import re
import tempfile
import json
import threading
from collections.abc import Callable, Mapping
from dataclasses import asdict, dataclass, field, replace
from functools import wraps
from datetime import UTC, datetime
from pathlib import Path
from threading import RLock
from typing import Protocol
from uuid import uuid4

from sqlalchemy.exc import SQLAlchemyError

from .chunking import _stable_id, chunk_document
from .chunk_strategies import ChunkingConfig, LEGACY_CONFIG
from .config import RagConfig
from .contracts import SearchRequest
from .document_readers import MAX_EXTRACTED_CHARACTERS, SUPPORTED_SUFFIXES
from .orchestrator import RagOrchestrator
from .pdf_content import PDF_PARSER_VERSION
from .state.schema import PDF_ASSET_BUCKET, SOURCE_BUCKET, WORD_ASSET_BUCKET
from .state.settings import StateSettings
from .state.store import StateStores
from .workspace import cleanup_workspace
from .state.uploads import UploadConflict
from .state.upload_coordinator import UploadCoordinator
from .state.result_snapshots import SnapshotUnavailable
from .state.artifacts import ArtifactUnavailable
from .state.search_references import SearchReferences
from .word_reader import WORD_PARSER_VERSION
from .model_api import HTTPEmbedder, HTTPReranker, ModelAPIError, ModelConnection, ModelHTTPClient
from .model_settings import draft_connection
from .knowledge_bases import model_connection, public_settings, stored_connection

SUPPORTED_EXTENSIONS = SUPPORTED_SUFFIXES
MAX_FILES_PER_BATCH = 10
MAX_FILE_BYTES = 10 * 1024 * 1024
_READ_SIZE = 1024 * 1024
_WINDOWS_RESERVED = re.compile(r'[<>:"/\\|?*\x00-\x1f]')


def _index_configuration(name: str, configuration: dict) -> dict:
    if Path(name).suffix.casefold() == ".pdf":
        return {**configuration, "pdf_parser_version": PDF_PARSER_VERSION}
    if Path(name).suffix.casefold() in {".doc", ".docx"}:
        return {**configuration, "word_parser_version": WORD_PARSER_VERSION}
    return configuration


class KnowledgeError(RuntimeError):
    """Base error returned by the RAG service knowledge API."""


class KnowledgeValidationError(KnowledgeError):
    """Raised before an unsupported or unsafe operation changes storage."""


@dataclass(frozen=True)
class IndexedDocument:
    """Index result persisted with the document record."""

    document_id: str
    chunk_count: int
    collection: str
    diagnostics: list[dict] = field(default_factory=list)
    skipped_object_count: int = 0
    artifacts_dir: Path | None = None
    artifacts_bucket: str | None = None


class DocumentIngester(Protocol):
    def ingest(self, path: Path, *, source_name: str, chunking: ChunkingConfig | None = None) -> IndexedDocument: ...

    def delete(self, document_id: str) -> None: ...


class KnowledgeDocumentStore:
    """Persist tenant sources, parse artifacts and their manifest in the state database."""

    def __init__(self, state: StateStores, namespace: str, *, index_settings: dict | None = None) -> None:
        self.state = state
        self.namespace = namespace
        self.index_settings = index_settings

    def list_documents(self) -> list[dict[str, object]]:
        records = self.state.documents.list(self.namespace)
        if self.index_settings:
            # The committed knowledge-base configuration owns the active index.
            for record in records:
                record["collection"] = self.index_settings["collection"]
                if "chunking" in record:
                    record["chunking"] = {
                        **record["chunking"],
                        "embedding_model": self.index_settings["embedding_model"],
                    }
        return records

    def validate_uploads(self, items: list[tuple[str, object]]) -> None:
        if not items:
            raise KnowledgeValidationError("请选择要上传的 Markdown、TXT、PDF 或 DOCX 文件。")
        if len(items) > MAX_FILES_PER_BATCH:
            raise KnowledgeValidationError(f"单次最多上传 {MAX_FILES_PER_BATCH} 个知识文件。")
        for raw_name, _readable in items:
            name = self.sanitize_name(raw_name)
            if Path(name).suffix.casefold() not in SUPPORTED_EXTENSIONS:
                supported = "、".join(sorted(SUPPORTED_EXTENSIONS))
                raise KnowledgeValidationError(f"暂不支持该文件类型：{name}；请选择 {supported}。")

    def find_duplicate(
        self, sha256: str, chunking: dict[str, object], *, only_name: str | None = None
    ) -> dict[str, object] | None:
        found = None
        for record in self.state.documents.candidates(self.namespace, sha256, only_name):
            name = str(record["name"])
            if only_name is not None and name != only_name:
                continue
            if not self.state.blobs.exists(self.namespace, SOURCE_BUCKET, name):
                continue
            if chunking.get("pdf_parser_version") or chunking.get("word_parser_version"):
                document_id = str(record["document_id"])
                bucket = WORD_ASSET_BUCKET if chunking.get("word_parser_version") else PDF_ASSET_BUCKET
                if not self.state.blobs.exists(self.namespace, bucket, f"{document_id}/document.md"):
                    continue
                if not self.state.blobs.exists(self.namespace, bucket, f"{document_id}/manifest.json"):
                    continue
            if not record.get("sha256"):
                content = self.state.blobs.get(self.namespace, SOURCE_BUCKET, name)
                if content is None:
                    continue
                record["sha256"] = hashlib.sha256(content).hexdigest()
                if not record.get("chunking"):
                    record["chunking"] = LEGACY_CONFIG.metadata("BAAI/bge-m3")
                self.state.documents.upsert(self.namespace, record)
            if not record.get("chunking"):
                record["chunking"] = LEGACY_CONFIG.metadata("BAAI/bge-m3")
            if record.get("sha256") == sha256 and record.get(
                "chunking", LEGACY_CONFIG.metadata("BAAI/bge-m3")
            ) == chunking:
                found = dict(record)
                break
        return found

    def store_and_index(
        self, raw_name: str, readable: object, ingester: DocumentIngester,
        chunking: ChunkingConfig | None = None, tokenizer_model: str = "BAAI/bge-m3", *,
        deduplicate_across_names: bool = True, embedding_model: str | None = None,
        idempotency_key: str | None = None, overwrite: bool | None = None,
        task: dict | None = None,
    ) -> dict[str, object]:
        """Write-ahead journal, immutable staging, then one atomic publication."""
        name = self.sanitize_name(raw_name)
        if idempotency_key is not None and (not isinstance(idempotency_key, str)
                or not 1 <= len(idempotency_key) <= 200):
            raise KnowledgeValidationError("Invalid idempotency key")
        configuration = _index_configuration(name, (chunking or LEGACY_CONFIG).metadata(tokenizer_model))
        if embedding_model is not None:
            configuration["embedding_model"] = embedding_model
        journal = self.state.uploads
        coordinator = UploadCoordinator(self.state)
        factory = lambda op: ingester.version_store(op["collection"]) if hasattr(ingester, "version_store") else None
        coordinator.recover(factory, namespace=self.namespace)
        document_id = _stable_id("doc", name.casefold())
        fingerprint = hashlib.sha256(json.dumps(
            {"name": name, "configuration": configuration, "overwrite": overwrite}, sort_keys=True
        ).encode()).hexdigest()
        collection = getattr(getattr(ingester, "_config", None), "collection", "test")
        op = journal.begin(self.namespace, name, document_id, collection,
                           idempotency_key=idempotency_key, fingerprint=fingerprint,
                           on_begin=(lambda conn, operation: self.state.document_tasks.bind(
                               conn, task, operation)) if task else None)
        if op["status"] == "COMMITTED":
            # Validate bytes on replay without repeating any external side effect.
            digest, size = hashlib.sha256(), 0
            while data := readable.read(_READ_SIZE):
                if not isinstance(data, bytes):
                    raise KnowledgeValidationError("知识文件内容无效")
                size += len(data)
                if size > MAX_FILE_BYTES:
                    raise KnowledgeValidationError("Document exceeds the file size limit")
                digest.update(data)
            if digest.hexdigest() != op["result"].get("request_sha256", op["result"].get("sha256")):
                raise UploadConflict("Idempotency key has different file bytes")
            self.state.cache_sync.flush_pending(keys=(("documents", self.namespace), ("kb_list", ""),
                ("heads", self.namespace), ("leases", self.namespace), ("upload_list", self.namespace),
                ("upload_status", op["id"])))
            return {**op["result"], "operation_id": op["id"], "replayed": True}
        workspace = Path(tempfile.gettempdir()).resolve() / ("rag-txn-" + op["id"])
        temporary = workspace / name
        metadata = None
        try:
            journal.register_workspace(op, workspace)
            current_settings = self.state.knowledge_bases.get(self.namespace)
            if (isinstance(ingester, DenseDocumentIngester) and current_settings is not None
                    and current_settings["collection"] != collection):
                raise UploadConflict("Knowledge-base index changed; retry with refreshed settings")
            def receive():
                workspace.mkdir(parents=True, exist_ok=False)
                digest = self._write_bounded(temporary, readable, name)
                journal.put_object(op, SOURCE_BUCKET, name, temporary.read_bytes())
                return digest, temporary.stat().st_size
            sha256, size = journal.step(op, "receive_source", receive)
            existing = self.state.documents.get(self.namespace, name)
            if overwrite is True and existing is None:
                raise KnowledgeValidationError(f"Document does not exist: {name}")
            if overwrite is False and (existing is not None or any(
                    str(r["name"]).casefold() == name.casefold() for r in self.list_documents())):
                raise KnowledgeValidationError(f"Document already exists: {name}; use update_document")
            previous = journal.step(op, "deduplicate", lambda: self.find_duplicate(
                sha256, configuration, only_name=None if deduplicate_across_names else name))
            if previous and task is None:
                result = {**previous, "skipped": True, "requested_name": name,
                          "operation_id": op["id"], "request_sha256": sha256}
                # A no-op is a terminal replayable result, not a new document version.
                journal.commit_noop(op, result)
                self.state.cache_sync.flush_pending(keys=(("documents", self.namespace), ("kb_list", ""),
                ("heads", self.namespace), ("leases", self.namespace), ("upload_list", self.namespace),
                ("upload_status", op["id"])))
                try:
                    coordinator.cleanup_workspace(op)
                except OSError:
                    logging.getLogger(__name__).warning("Committed workspace cleanup pending: %s", op["id"])
                return result

            def publish(indexed):
                nonlocal metadata
                if task is not None:
                    self.state.document_tasks.progress(task, "publishing")
                metadata = self._metadata(name, size, sha256, indexed)
                metadata.update(sha256=sha256, chunking=configuration, operation_id=op["id"])
                journal.intent(op, "publish", {"version": op["id"]})
                with self.state.engine.begin() as conn:
                    journal.assert_owner(op, conn)
                    self.state.documents.upsert(self.namespace, metadata, conn=conn)
                    self.state.blobs.put(self.namespace, SOURCE_BUCKET, name, temporary.read_bytes(),
                                         document_id=str(indexed.document_id), conn=conn)
                    if indexed.artifacts_dir is not None and indexed.artifacts_bucket:
                        self.state.blobs.delete_document(self.namespace, indexed.artifacts_bucket,
                                                         str(indexed.document_id), conn=conn)
                        self.state.blobs.put_tree(self.namespace, indexed.artifacts_bucket,
                                                 str(indexed.document_id), indexed.artifacts_dir, conn=conn)
                    if task is not None:
                        self.state.document_tasks.succeed(conn, task)
                    journal.commit(op, metadata, conn)

            if isinstance(ingester, DenseDocumentIngester):
                ingester.ingest_and_publish(temporary, source_name=name, chunking=chunking,
                    publish=publish, journal=journal, operation=op, workspace=workspace,
                    progress=(lambda stage: self.state.document_tasks.progress(task, stage)) if task else None)
            else:
                transactional = getattr(ingester, "ingest_and_publish", None)
                if transactional is not None:
                    transactional(temporary, source_name=name, chunking=chunking, publish=publish)
                else:
                    indexed = journal.step(op, "parse_chunking", lambda: ingester.ingest(
                        temporary, source_name=name, chunking=chunking))
                    if indexed.artifacts_dir is not None and indexed.artifacts_bucket:
                        for file in indexed.artifacts_dir.rglob("*"):
                            if file.is_file():
                                journal.put_object(op, indexed.artifacts_bucket,
                                    f"{indexed.document_id}/{file.relative_to(indexed.artifacts_dir).as_posix()}",
                                    file.read_bytes())
                    publish(indexed)
                    if indexed.artifacts_dir is not None:
                        cleanup_workspace(indexed.artifacts_dir)
            if metadata is None:
                raise KnowledgeError("知识文件索引未发布")
            self.state.cache_sync.flush_pending(keys=(("documents", self.namespace), ("kb_list", ""),
                ("heads", self.namespace), ("leases", self.namespace), ("upload_list", self.namespace),
                ("upload_status", op["id"])))
            try:
                coordinator.cleanup_workspace(op)
            except OSError:
                logging.getLogger(__name__).warning("Committed workspace cleanup pending: %s", op["id"])
            return metadata
        except Exception as exc:
            exc.operation_id = op["id"]
            # An ambiguous commit acknowledgement must never roll back a committed head.
            current = journal.get(op["id"])
            if current["status"] == "COMMITTED":
                self.state.cache_sync.flush_pending(keys=(("documents", self.namespace), ("kb_list", ""),
                ("heads", self.namespace), ("leases", self.namespace), ("upload_list", self.namespace),
                ("upload_status", op["id"])))
                try:
                    coordinator.cleanup_workspace(current)
                except OSError:
                    logging.getLogger(__name__).warning("Committed workspace cleanup pending: %s", op["id"])
                return current["result"]
            try:
                journal.fail(op, exc)
                coordinator.compensate(op, factory(op))
            except Exception:
                logging.getLogger(__name__).exception("Upload recovery pending: %s", op["id"])
            raise
        finally:
            journal._stop(op)

    def delete(self, name: str, ingester: DocumentIngester) -> dict[str, object]:
        safe_name = self.sanitize_name(name)
        record = self.state.documents.get(self.namespace, safe_name)
        if record is None:
            raise KnowledgeValidationError(f"知识文件不存在：{safe_name}")
        document_id = record.get("document_id")
        if not isinstance(document_id, str) or not document_id:
            raise KnowledgeError(f"知识文件索引记录损坏：{safe_name}")
        journal = self.state.uploads
        try:
            with journal.lock(self.namespace, document_id) as token:
                # Logical deletion never touches retained source/index bytes.
                # New lookups and Qdrant visibility consult del_flag in MySQL.
                with self.state.engine.begin() as conn:
                    journal.assert_lock(token, conn)
                    journal._cache_changed(conn, self.namespace, heads=True)
                    if not self.state.documents.delete(self.namespace, safe_name, conn=conn):
                        raise KnowledgeValidationError(f"知识文件不存在：{safe_name}")
        except UploadConflict as exc:
            raise KnowledgeValidationError("文档正在更新，请稍后再删除") from exc
        self.state.cache_sync.flush_pending(keys=(("documents", self.namespace), ("kb_list", ""),
                ("heads", self.namespace), ("leases", self.namespace), ("upload_list", self.namespace),
                ))
        return record

    def read_source(self, name: str, record=None) -> bytes:
        record = record or self.state.documents.get(self.namespace, name)
        if record is None:
            raise KnowledgeValidationError("Document does not exist")
        version = record.get("operation_id")
        content = (self.state.uploads.get_object(version, SOURCE_BUCKET, name) if version else
                   self.state.blobs.get(self.namespace, SOURCE_BUCKET, name))
        if content is None:
            raise KnowledgeValidationError("Document content is unavailable")
        return content

    def read_text(self, name: str, record: Mapping[str, object]) -> tuple[str, str]:
        suffix = Path(name).suffix.casefold()
        chunking = record.get("chunking") if isinstance(record.get("chunking"), dict) else {}
        version = record.get("operation_id")
        if version and suffix in {".pdf", ".doc", ".docx"}:
            bucket = PDF_ASSET_BUCKET if suffix == ".pdf" else WORD_ASSET_BUCKET
            content = self.state.uploads.get_object(version, bucket, f"{record['document_id']}/document.md")
            if content is None:
                raise KnowledgeValidationError("Saved content unavailable; reimport this document")
            return content.decode("utf-8"), str(record["indexed_at"])
        if suffix == ".pdf":
            from .pdf_assets import read_pdf_content

            text = read_pdf_content(self.state, self.namespace, name, chunking)
        elif suffix in {".doc", ".docx"} and chunking.get("word_parser_version"):
            from .word_assets import read_word_content

            text = read_word_content(self.state, self.namespace, name, chunking)
        else:
            content = self.read_source(name, record)
            try:
                text = content.decode("utf-8-sig")
            except UnicodeDecodeError as exc:
                raise KnowledgeValidationError(f"文本文件必须使用 UTF-8 编码：{name}") from exc
            if len(text) > MAX_EXTRACTED_CHARACTERS:
                raise KnowledgeValidationError(f"文档提取文本过大：{name}")
        return text, str(record["indexed_at"])

    @staticmethod
    def sanitize_name(raw_name: str) -> str:
        name = _WINDOWS_RESERVED.sub("_", str(raw_name or "")).strip(" .")[:200]
        if not name or name in {".", ".."}:
            raise KnowledgeValidationError("知识文件名不能为空。")
        return name

    @staticmethod
    def _write_bounded(target: Path, readable: object, name: str) -> str:
        size = 0
        digest = hashlib.sha256()
        with target.open("wb") as handle:
            while True:
                chunk = readable.read(_READ_SIZE)  # type: ignore[attr-defined]
                if not chunk:
                    break
                if not isinstance(chunk, bytes):
                    raise KnowledgeValidationError(f"知识文件内容无效：{name}")
                size += len(chunk)
                if size > MAX_FILE_BYTES:
                    raise KnowledgeValidationError(
                        f"单个知识文件不能超过 {MAX_FILE_BYTES // (1024 * 1024)} MiB：{name}"
                    )
                handle.write(chunk)
                digest.update(chunk)
            handle.flush()
            os.fsync(handle.fileno())
        return digest.hexdigest()

    @staticmethod
    def _metadata(name: str, size: int, sha256: str, indexed: IndexedDocument) -> dict[str, object]:
        now = datetime.now(UTC).isoformat()
        return {
            "name": name,
            "size": size,
            "modified_at": now,
            "indexed_at": now,
            "document_id": indexed.document_id,
            "chunk_count": indexed.chunk_count,
            "collection": indexed.collection,
            "diagnostics": indexed.diagnostics,
            "skipped_object_count": indexed.skipped_object_count,
            "sha256": sha256,
        }


class DenseDocumentIngester:
    """Index documents with service-owned shared model dependencies."""

    def __init__(self, config: RagConfig, embedder, sparse_encoder, store) -> None:
        self._config = config
        self._embedder = embedder
        self._sparse_encoder = sparse_encoder
        self._store = store

    def ingest(self, path: Path, *, source_name: str, chunking: ChunkingConfig | None = None) -> IndexedDocument:
        return self.ingest_and_publish(path, source_name=source_name, chunking=chunking)

    def ingest_and_publish(self, path: Path, *, source_name: str, chunking: ChunkingConfig | None = None,
                           publish: Callable[[IndexedDocument], None] | None = None,
                           journal=None, operation=None, workspace=None, progress=None) -> IndexedDocument:
        parsed_assets = None

        def trace(name, action):
            if progress is not None:
                stage = {"embedding": "embedding", "sparse": "embedding",
                         "chunking_tokenizer": "chunking", "stage_mysql": "staging",
                         "stage_qdrant": "indexing"}.get(name, "parsing")
                progress(stage)
            return journal.step(operation, name, action) if journal else action()
        parser_options = {"workspace": workspace / "parsed", "trace": trace} if workspace else {}
        try:
            if path.suffix.casefold() == ".pdf":
                from .pdf_assets import parse_pdf

                parsed_assets = parse_pdf(
                    path, source_name=source_name, config=chunking,
                    tokenizer_model=self._config.chunk_tokenizer_model,
                    parser_python=self._config.pdf_parser_python,
                    models_path=self._config.pdf_models_path,
                    timeout_seconds=self._config.pdf_parse_timeout,
                    **parser_options,
                )
                chunks = parsed_assets.chunks
            elif path.suffix.casefold() in {".doc", ".docx"}:
                from .word_assets import parse_word

                parsed_assets = parse_word(
                    path, source_name=source_name, config=chunking,
                    tokenizer_model=self._config.chunk_tokenizer_model,
                    converter=self._config.word_converter_path,
                    convert_timeout=self._config.word_convert_timeout,
                    parser_python=self._config.pdf_parser_python,
                    models_path=self._config.pdf_models_path,
                    timeout_seconds=self._config.pdf_parse_timeout,
                    **parser_options,
                )
                chunks = parsed_assets.chunks
            else:
                chunks = trace("chunking_tokenizer", lambda: chunk_document(
                    path, source_name=source_name, config=chunking,
                    tokenizer_model=self._config.chunk_tokenizer_model))
            if not chunks:
                raise KnowledgeValidationError(f"知识文件没有可索引的文本：{source_name}")
            vectors = trace("embedding", lambda: self._embedder.embed_documents([chunk.text for chunk in chunks]))
            sparse_vectors = trace("sparse", lambda: self._sparse_encoder.encode_documents([chunk.text for chunk in chunks]))
            asset_payloads = []
            if parsed_assets is not None:
                asset_payloads = [asset.payload(chunks[0].document_id, chunks[0].source)
                                  for asset in parsed_assets.assets]
            indexed = IndexedDocument(
                chunks[0].document_id, len(chunks), self._config.collection,
                getattr(parsed_assets, "diagnostics", []), getattr(parsed_assets, "skipped_object_count", 0),
                artifacts_dir=getattr(parsed_assets, "staging_dir", None),
                artifacts_bucket=(
                    WORD_ASSET_BUCKET if path.suffix.casefold() in {".doc", ".docx"} else PDF_ASSET_BUCKET
                ) if parsed_assets is not None else None,
            )
            if journal is not None:
                def stage_mysql():
                    with journal.engine.begin() as conn:
                        journal.assert_owner(operation, conn)
                        journal.put_object(operation, "index", "chunks.json", json.dumps(
                            [{"chunk_id": c.chunk_id, "text": c.text, "metadata": c.metadata} for c in chunks],
                            ensure_ascii=False).encode(), conn=conn)
                        if parsed_assets is not None:
                            for file in sorted(parsed_assets.staging_dir.rglob("*")):
                                if file.is_file():
                                    key = f"{indexed.document_id}/{file.relative_to(parsed_assets.staging_dir).as_posix()}"
                                    journal.put_object(operation, indexed.artifacts_bucket, key,
                                                       file.read_bytes(), conn=conn)
                trace("stage_mysql", stage_mysql)
                try:
                    count = trace("stage_qdrant", lambda: self._store.stage_version(
                        chunks, vectors, sparse_vectors, asset_payloads, operation["id"]))
                except Exception:
                    # Qdrant has no lease CAS: a timed-out old write may complete
                    # after recovery. Remove only our unpublished version again.
                    current = journal.get(operation["id"])
                    if current["status"] != "COMMITTED":
                        try:
                            self._store.delete_version(indexed.document_id, operation["id"])
                        except Exception:
                            logging.getLogger(__name__).warning(
                                "Version cleanup requires recovery: %s", operation["id"])
                    raise
                if publish is None:
                    raise KnowledgeError("Journaled uploads require publication")
                publish(indexed)
            else:
                count = self._store.replace_document(
                    chunks, vectors, sparse_vectors, asset_payloads,
                    after_write=(lambda: publish(indexed)) if publish is not None else None,
                )
            return replace(indexed, chunk_count=count)
        except ValueError as exc:
            raise KnowledgeValidationError(str(exc)) from exc
        finally:
            if parsed_assets is not None and workspace is None:
                cleanup_workspace(parsed_assets.staging_dir)

    def version_store(self, collection):
        from .storage.qdrant_store import QdrantStore
        return QdrantStore(collection=collection, vector_size=self._config.vector_size,
                           namespace=self._store.namespace, client=self._store._client)

    def delete(self, document_id: str) -> None:
        self._store.delete_document(document_id)


@dataclass(frozen=True)
class TenantResources:
    documents: KnowledgeDocumentStore
    ingester: DenseDocumentIngester
    orchestrator: RagOrchestrator
    lock: RLock
    kb_version: int | None = None


def _namespace_mutation(method):
    @wraps(method)
    def guarded(self, namespace, *args, **kwargs):
        with self.state.uploads.lock(namespace):
            return method(self, namespace, *args, **kwargs)
    return guarded


def _runtime_operation(method):
    # Acquire before resolving a tenant: configuration changes cannot leave queued
    # operations holding stale model/collection references.
    @wraps(method)
    def run(self, *args, **kwargs):
        with self._inference_lock:
            return method(self, *args, **kwargs)
    return run


class RagServiceRuntime:
    """Long-lived model/Qdrant owner serving isolated tenant namespaces."""

    def __init__(self, config: RagConfig, data_root: Path, *, state: StateStores | None = None) -> None:
        if config.backend != "qdrant":
            raise KnowledgeError("Standalone RAG service requires RAG_BACKEND=qdrant.")
        from qdrant_client import QdrantClient

        from .sparse import Bm25SparseEncoder

        self.config = config
        self.data_root = data_root.expanduser().resolve()
        if self.data_root.is_symlink():
            raise KnowledgeError("RAG service data root cannot be a symbolic link.")
        self.data_root.mkdir(parents=True, exist_ok=True)
        self.state = state or StateStores.create(
            self.data_root, settings=StateSettings.from_env(self.data_root)
        )
        logging.getLogger(__name__).info("State store: %s", self.state.describe())
        self._client = QdrantClient(
            url=config.qdrant_url,
            path=str(config.qdrant_path) if config.qdrant_path else None,
        )
        self._sparse_encoder = Bm25SparseEncoder()
        self._tenants: dict[str, TenantResources] = {}
        self._tenant_lock = RLock()
        self._inference_lock = RLock()
        self._recovery_stop = threading.Event()
        self._recovery_thread = None

    def _recovery_store(self, operation):
        from .storage.qdrant_store import QdrantStore
        return QdrantStore(collection=operation["collection"], namespace=operation["namespace"],
                           client=self._client)

    def recover_uploads(self):
        recovered = UploadCoordinator(self.state).recover(self._recovery_store)
        self.state.artifacts.purge_expired(limit=100)
        self.state.results.purge_expired(limit=100)
        return recovered

    def start_upload_recovery(self):
        if not hasattr(self, "_recovery_stop"):
            return
        if self._recovery_thread is not None:
            return
        self.recover_uploads()
        def recover():
            while not self._recovery_stop.wait(10):
                try:
                    self.recover_uploads()
                except Exception:
                    logging.getLogger(__name__).exception("Upload recovery pass failed")
        self._recovery_thread = threading.Thread(target=recover, name="upload-recovery", daemon=True)
        self._recovery_thread.start()

    def stop_upload_recovery(self):
        if not hasattr(self, "_recovery_stop"):
            return
        self._recovery_stop.set()
        if self._recovery_thread is not None:
            self._recovery_thread.join(timeout=15)
            self._recovery_thread = None

    def close(self) -> None:
        self.stop_upload_recovery()
        self._client.close()
        self.state.close()

    def status(self) -> dict[str, object]:
        return {
            "status": "ok",
            "service": "praxis-rag",
            "backend": self.config.backend,
            "collection": self.config.collection,
            "state": self.state.describe(),
        }

    @_runtime_operation
    def knowledge_base_models(self, kind: str, values: dict, namespace: str | None = None) -> list[str]:
        current = model_connection(self.knowledge_base(namespace), kind) if namespace else ModelConnection()
        return ModelHTTPClient(draft_connection(current, values)).models()

    @_runtime_operation
    def knowledge_base_key(self, namespace: str, kind: str) -> str:
        return model_connection(self.knowledge_base(namespace), kind).api_key

    @_runtime_operation
    def list_knowledge_bases(self) -> list[dict]:
        return self.state.views.knowledge_bases()

    @_runtime_operation
    def create_knowledge_base(self, namespace: str, chunking: ChunkingConfig,
                              embedding_model: str, reranker_model: str = "",
                              *, embedding: dict, reranker: dict | None = None) -> dict:
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,63}", namespace):
            raise KnowledgeValidationError("知识库标识无效")
        if self.state.knowledge_bases.get(namespace) is not None:
            raise KnowledgeValidationError("该知识库已存在，请在知识库页面修改配置")
        if self.state.documents.list(namespace):
            raise KnowledgeValidationError("该 Namespace 已有文档，请使用新的知识库标识")
        embedding_model = embedding_model.strip()
        reranker_model = reranker_model.strip()
        if not embedding_model:
            raise KnowledgeValidationError("请选择 Embedding 模型")
        embedding_connection = replace(draft_connection(ModelConnection(), embedding), model=embedding_model)
        vector_size = len(HTTPEmbedder(embedding_connection).embed_query("connection test"))
        reranker_connection = ModelConnection()
        if reranker_model:
            reranker_connection = replace(draft_connection(ModelConnection(), reranker or {}), model=reranker_model)
            HTTPReranker(reranker_connection).scores("connection test", ["connection test"])
        settings = {"namespace": namespace, "vector_size": vector_size,
                    "embedding_model": embedding_model, "reranker_model": reranker_model,
                    "embedding": stored_connection(embedding_connection), "reranker": stored_connection(reranker_connection),
                    "collection": f"knowledge_{uuid4().hex}",
                    "tokenizer": self.config.chunk_tokenizer_model,
                    "strategy": chunking.strategy, "chunk_size": chunking.chunk_size,
                    "chunk_overlap": chunking.chunk_overlap,
                    "created_at": datetime.now(UTC).isoformat()}
        self.state.knowledge_bases.create(settings)
        self._tenants.pop(namespace, None)
        return public_settings(settings)

    @_runtime_operation
    def knowledge_base(self, namespace: str) -> dict:
        settings = self.state.knowledge_bases.get(namespace)
        if settings is None:
            raise KnowledgeValidationError("该知识库没有配置记录，请先创建知识库")
        return settings

    @_runtime_operation
    def public_knowledge_base(self, namespace: str) -> dict:
        settings = self.state.views.knowledge_base(namespace)
        if settings is None:
            raise KnowledgeValidationError("该知识库没有配置记录，请先创建知识库")
        return settings

    @_runtime_operation
    def update_knowledge_base(self, namespace: str, values: dict, *, reembed: bool = False) -> dict:
        published = None
        publication_error = None
        try:
            with self.state.uploads.lock(namespace) as token:
                try:
                    published = self._publish_knowledge_base(namespace, values, reembed=reembed, token=token)
                except Exception as exc:
                    publication_error = exc
                    raise
        except Exception:
            if published is None:
                if publication_error is not None:
                    raise publication_error
                raise
            logging.getLogger(__name__).warning("Published knowledge base %s; lease cleanup pending", namespace)
        self._tenants.pop(namespace, None)
        try:
            from .state.cache_events import knowledge_keys
            self.state.cache_sync.flush_pending(keys=knowledge_keys(namespace))
        except Exception:  # noqa: BLE001 - committed outbox retries cache refresh independently
            logging.getLogger(__name__).warning("Published knowledge base %s; cache refresh pending", namespace)
        return public_settings(published)

    def _publish_knowledge_base(self, namespace, values, *, reembed, token):
        from .state.repositories import PublicationRejected
        from .storage.qdrant_store import QdrantStore

        current, expected_version = self.state.knowledge_bases.snapshot(namespace)
        if current is None:
            raise KnowledgeValidationError("该知识库没有配置记录，请先创建知识库")
        old_embedding = model_connection(current, "embedding")
        old_reranker = model_connection(current, "reranker")
        embedding = replace(draft_connection(old_embedding, values.get("embedding", {})),
                            model=values.get("embedding_model", old_embedding.model).strip(), vector_size=None)
        if not embedding.model:
            raise KnowledgeValidationError("请选择 Embedding 模型")
        changed_embedding = embedding.model != old_embedding.model or embedding.base_url != old_embedding.base_url
        if changed_embedding and not reembed:
            raise KnowledgeValidationError("修改 Embedding URL 或模型前，必须重新向量化（re embedding）")
        reranker_model = values.get("reranker_model", old_reranker.model).strip()
        reranker = ModelConnection()
        if reranker_model:
            reranker = replace(draft_connection(old_reranker, values.get("reranker", {})), model=reranker_model)
            HTTPReranker(reranker).scores("connection test", ["connection test"])
        size = len(HTTPEmbedder(embedding).embed_query("connection test"))
        if size != current["vector_size"] and not reembed:
            raise KnowledgeValidationError("Embedding 向量长度已改变，必须重新向量化（re embedding）")
        embedding = replace(embedding, vector_size=size)
        updated = {**current, "embedding": stored_connection(embedding), "reranker": stored_connection(reranker),
                   "embedding_model": embedding.model, "reranker_model": reranker.model,
                   "vector_size": size, "updated_at": datetime.now(UTC).isoformat()}
        staging = f"knowledge_{uuid4().hex}" if reembed else None
        try:
            if staging:
                records = self.state.documents.list(namespace)
                source = QdrantStore(collection=current["collection"], vector_size=current["vector_size"],
                                     namespace=namespace, client=self._client,
                                     active_versions=lambda: self.state.documents.visible_versions(namespace))
                source.reembed_into(staging, HTTPEmbedder(embedding),
                                    {item["document_id"]: item["chunk_count"] for item in records})
                updated.update(collection=staging, reembedded_at=datetime.now(UTC).isoformat())
        except Exception:
            self._discard_unpublished_index(staging)
            raise
        try:
            self.state.knowledge_bases.publish(
                updated, expected_version=expected_version, journal=self.state.uploads, token=token)
        except PublicationRejected as exc:
            self._discard_unpublished_index(staging)
            raise KnowledgeValidationError("知识库发布已回滚，请刷新配置后重试") from exc
        except Exception as exc:
            try:
                confirmed = self.state.knowledge_bases.confirm_publication(
                    namespace, updated, expected_version=expected_version, journal=self.state.uploads)
            except Exception:  # noqa: BLE001 - uncertainty forbids destructive cleanup
                confirmed = False
            if not confirmed:
                self._tenants.pop(namespace, None)
                raise KnowledgeError(
                    "Knowledge-base publication outcome is unconfirmed; the new index was retained. "
                    "Check the current configuration before retrying."
                ) from exc
        return updated

    def _discard_unpublished_index(self, collection):
        if collection is None:
            return
        try:
            if self._client.collection_exists(collection):
                self._client.delete_collection(collection)
        except Exception:  # noqa: BLE001 - cleanup cannot replace the original construction/publication error
            logging.getLogger(__name__).warning("Unpublished index cleanup pending: %s", collection)

    def _knowledge_chunking(self, namespace: str) -> ChunkingConfig:
        settings = self.knowledge_base(namespace)
        return ChunkingConfig(settings["strategy"], settings["chunk_size"], settings["chunk_overlap"])

    @_runtime_operation
    def list_documents(self, namespace: str) -> list[dict[str, object]]:
        return self.state.views.documents(namespace)

    @_runtime_operation
    def authoritative_documents(self, namespace: str) -> list[dict[str, object]]:
        """Read current records for mutation validation and change notifications."""
        tenant = self._tenant(namespace)
        with tenant.lock:
            return tenant.documents.list_documents()

    def upload_operations(self, namespace: str) -> list[dict]:
        return self.state.views.upload_operations(namespace)

    def upload_operation(self, namespace: str, operation_id: str) -> dict | None:
        return self.state.views.upload_operation(namespace, operation_id)

    def published_versions(self, namespace: str) -> dict[str, str]:
        return self.state.views.published_versions(namespace)

    def upload_leases(self, namespace: str) -> list[dict]:
        return self.state.views.leases(namespace)

    def chunking_metadata(self, namespace: str, chunking: ChunkingConfig) -> dict[str, object]:
        settings = self.knowledge_base(namespace)
        return {**chunking.metadata(settings["tokenizer"]), "embedding_model": settings["embedding_model"]}

    @_runtime_operation
    def check_documents(self, namespace: str, items: list[dict[str, str]]) -> dict[str, object]:
        chunking = self._knowledge_chunking(namespace)
        tenant = self._tenant(namespace)
        tenant.documents.validate_uploads([(item["name"], None) for item in items])
        configuration = self.chunking_metadata(namespace, chunking)
        with tenant.lock:
            return {"files": [{"name": item["name"], "sha256": item["sha256"],
                               "document": tenant.documents.find_duplicate(
                                   item["sha256"], _index_configuration(item["name"], configuration))}
                              for item in items]}

    @_runtime_operation
    def upload(self, namespace: str, items: list[tuple[str, object]], *,
               idempotency_key: str | None = None) -> dict[str, object]:
        chunking = self._knowledge_chunking(namespace)
        tenant = self._tenant(namespace)
        tenant.documents.validate_uploads(items)
        model = self.knowledge_base(namespace)["tokenizer"]
        imported: list[dict[str, object]] = []
        skipped: list[dict[str, object]] = []
        failed: list[dict[str, str]] = []
        with tenant.lock:
            for position, (name, readable) in enumerate(items):
                try:
                    with self._inference_lock:
                        record = tenant.documents.store_and_index(name, readable, tenant.ingester, chunking, model,
                                                                  embedding_model=self.knowledge_base(namespace)["embedding_model"],
                                                                  idempotency_key=(hashlib.sha256(
                                                                      f"{idempotency_key}:{position}".encode()).hexdigest()
                                                                      if idempotency_key else None))
                        if record.pop("skipped", False):
                            skipped.append(record)
                        else:
                            imported.append(record)
                except Exception as exc:
                    failed.append({"name": tenant.documents.sanitize_name(name), "error": _safe_error(exc),
                                   **({"operation_id": exc.operation_id} if hasattr(exc, "operation_id") else {})})
            documents = tenant.documents.list_documents()
        return {"imported": imported, "skipped": skipped, "failed": failed, "documents": documents}

    def add_document(self, namespace: str, name: str, content: bytes, *,
                     idempotency_key: str | None = None) -> dict:
        try:
            return self.state.document_dispatcher.submit(namespace, name, content, idempotency_key)
        except (KnowledgeValidationError, UploadConflict):
            raise
        except ValueError as exc:
            raise KnowledgeValidationError(str(exc)) from exc
        except Exception as exc:
            raise KnowledgeError(
                "Document storage was not confirmed; retry with the same idempotency_key"
            ) from exc

    def get_document_status(self, namespace: str, artifact_id: str) -> dict:
        try:
            return self.state.document_tasks.status(namespace, artifact_id)
        except (ValueError, LookupError) as exc:
            raise KnowledgeValidationError(str(exc)) from exc

    def process_document_task(self, task: dict) -> dict:
        namespace = task["namespace"]
        if self.state.knowledge_bases.version(namespace) != task["configuration_version"]:
            raise KnowledgeValidationError("Knowledge-base configuration changed; upload again")
        settings = self.knowledge_base(namespace)
        tenant = self._tenant(namespace)
        configuration = ChunkingConfig(settings["strategy"], settings["chunk_size"], settings["chunk_overlap"])
        with tenant.lock:
            self.state.document_tasks.progress(task, "parsing")
            return tenant.documents.store_and_index(
                task["name"], io.BytesIO(task["content"]), tenant.ingester, configuration,
                settings["tokenizer"], deduplicate_across_names=False,
                embedding_model=settings["embedding_model"], overwrite=False,
                idempotency_key=f"document-task:{task['artifact_id']}:{task['execution_id']}", task=task,
            )

    @_runtime_operation
    def write_document(self, namespace: str, name: str, content: bytes, *, overwrite: bool,
                       idempotency_key: str | None = None) -> dict[str, object]:
        configuration = self._knowledge_chunking(namespace)
        tenant = self._tenant(namespace)
        if tenant.documents.sanitize_name(name) != name:
            raise KnowledgeValidationError("Invalid document name")
        with tenant.lock:
            records = tenant.documents.list_documents()
            existing = next((item for item in records if item["name"] == name), None)
            if not idempotency_key and overwrite and existing is None:
                raise KnowledgeValidationError(f"Document does not exist: {name}")
            if not idempotency_key and not overwrite and (any(str(item["name"]).casefold() == name.casefold() for item in records)
                                  or self.state.documents.get(namespace, name) is not None):
                raise KnowledgeValidationError(f"Document already exists: {name}; use update_document")
            readable = io.BytesIO(content)
            tenant.documents.validate_uploads([(name, readable)])
            try:
                with self._inference_lock:
                    record = tenant.documents.store_and_index(
                        name, readable, tenant.ingester, configuration, self.knowledge_base(namespace)["tokenizer"],
                        deduplicate_across_names=False,
                        embedding_model=self.knowledge_base(namespace)["embedding_model"],
                        idempotency_key=idempotency_key, overwrite=overwrite,
                    )
            except Exception as exc:
                raise KnowledgeError(_safe_error(exc)) from exc
            unchanged = record.pop("skipped", False)
            record.pop("requested_name", None)
            return {"status": "unchanged" if unchanged else ("updated" if overwrite else "added"),
                    "document": record, "documents": tenant.documents.list_documents()}

    @_runtime_operation
    def delete(self, namespace: str, name: str) -> dict[str, object]:
        tenant = self._tenant(namespace)
        with tenant.lock:
            deleted = tenant.documents.delete(name, tenant.ingester)
            documents = tenant.documents.list_documents()
        return {"deleted": deleted, "documents": documents}

    @_runtime_operation
    def search(self, namespace: str, arguments: Mapping[str, object], *, save_snapshot=True) -> dict[str, object]:
        tenant = self._tenant(namespace)
        request = SearchRequest.from_arguments(dict(arguments))
        with tenant.lock, self._inference_lock:
            context = None
            config = getattr(getattr(tenant, "ingester", None), "_config", None)
            if config is not None:
                context = {key: getattr(config, key) for key in (
                    "collection", "embedding_model", "reranker_model", "vector_size", "chunk_tokenizer_model")}
                context["configuration_version"] = getattr(tenant, "kb_version", None)
            payload = tenant.orchestrator.search(request)
            if context is not None:
                payload = {**payload, "snapshot_context": context}
            # Immutable hit versions are copied into expiring artifacts; do not
            # take a writer lease or block reads of the current committed index.
            try:
                return SearchReferences(self.state).capture(namespace, payload, save_snapshot=save_snapshot)
            except Exception as exc:
                logging.getLogger(__name__).warning("Search reference capture unavailable: %s", type(exc).__name__)
                return {**payload, "snapshot_available": False,
                        "snapshot_error": "Search result references are unavailable"}

    def submit_search(self, namespace: str, arguments: dict) -> dict:
        validated = SearchRequest.from_arguments(arguments)
        try:
            return self.state.search_dispatcher.submit(namespace, asdict(validated))
        except (KnowledgeError, SnapshotUnavailable):
            raise
        except Exception as exc:
            raise KnowledgeError("Search submission unavailable; queue acceptance was not confirmed") from exc

    def get_search_result(self, namespace: str, result_id: str, *, page=1, page_size=10) -> dict:
        try:
            queued = self.state.search_tasks.get(namespace, result_id, page=page, page_size=page_size)
        except SQLAlchemyError:
            # Only committed result snapshots may be replayed when the task database is unavailable.
            return self.state.results.get(namespace, result_id, page=page, page_size=page_size)
        if queued is not None:
            return queued
        return self.state.results.get(namespace, result_id, page=page, page_size=page_size)

    def get_artifact(self, namespace: str, artifact_id: str, *, offset=0, length=65536) -> dict:
        try:
            return self.state.artifacts.chunk(namespace, artifact_id, offset=offset, length=length)
        except ArtifactUnavailable as exc:
            raise SnapshotUnavailable(str(exc)) from exc

    def download_artifact(self, namespace: str, artifact_id: str):
        try:
            artifact = self.state.artifacts.read(namespace, artifact_id)
        except ArtifactUnavailable as exc:
            raise SnapshotUnavailable(str(exc)) from exc
        return artifact["content"], artifact["mime_type"], artifact["name"]

    @_runtime_operation
    def read_document_file(self, namespace: str, name: str) -> bytes:
        tenant = self._tenant(namespace)
        with tenant.lock:
            if tenant.documents.sanitize_name(name) != name:
                raise KnowledgeValidationError("Invalid document name")
            record = self.state.documents.get(namespace, name)
            if record is None:
                raise KnowledgeValidationError("Document does not exist")
            content = tenant.documents.read_source(name, record)
            if len(content) > MAX_FILE_BYTES:
                raise KnowledgeValidationError("Document exceeds the file size limit")
            return content

    @_runtime_operation
    def read_document(self, namespace: str, name: str) -> tuple[str, str]:
        tenant = self._tenant(namespace)
        with tenant.lock:
            if tenant.documents.sanitize_name(name) != name:
                raise KnowledgeValidationError("Invalid document name")
            record = self.state.documents.get(namespace, name)
            if record is None:
                raise KnowledgeValidationError("Document does not exist")
            try:
                return tenant.documents.read_text(name, record)
            except ValueError as exc:
                raise KnowledgeValidationError(str(exc)) from exc

    def _tenant(self, namespace: str) -> TenantResources:
        with self._tenant_lock:
            settings, version = self.state.knowledge_bases.snapshot(namespace)
            existing = self._tenants.get(namespace)
            if existing is not None and existing.kb_version == version:
                return existing
            from .retrievers.dense import DenseRetriever
            from .retrievers.hybrid import Bm25Retriever, HybridRetriever
            from .pdf_assets import project_results
            from .storage.qdrant_store import QdrantStore

            config = replace(self.config, namespace=namespace)
            if settings:
                config = replace(config, collection=settings["collection"], vector_size=settings["vector_size"],
                                 embedding_model=settings["embedding_model"], reranker_model=settings["reranker_model"],
                                 chunk_tokenizer_model=settings["tokenizer"])
            embedder = HTTPEmbedder(model_connection(settings, "embedding") if settings else ModelConnection())
            reranker = HTTPReranker(model_connection(settings, "reranker") if settings else ModelConnection(),
                                   max_candidates=config.reranker_max_candidates,
                                   max_text_chars=config.reranker_max_text_chars)

            store = QdrantStore(
                collection=config.collection,
                vector_size=config.vector_size,
                namespace=namespace,
                client=self._client,
                active_versions=lambda: self.state.documents.visible_versions(namespace),
            )
            orchestrator = RagOrchestrator(
                retrievers={
                    "bm25": Bm25Retriever(self._sparse_encoder, store),
                    "embedding": DenseRetriever(embedder, store),
                    "hybrid": HybridRetriever(embedder, self._sparse_encoder, store),
                },
                rerankers={
                    "cross_encoder": reranker,
                },
                implementation="rag-service-qdrant-hybrid-rerank-v1",
                projector=lambda candidates: project_results(
                    candidates, store, self.state, namespace,
                ),
            )
            resources = TenantResources(
                documents=KnowledgeDocumentStore(self.state, namespace, index_settings=settings),
                ingester=DenseDocumentIngester(config, embedder, self._sparse_encoder, store),
                orchestrator=orchestrator,
                lock=RLock(),
                kb_version=version,
            )
            self._tenants[namespace] = resources
            return resources


def _safe_error(exc: Exception) -> str:
    if isinstance(exc, (KnowledgeError, ModelAPIError)):
        return str(exc)[:500]
    return f"知识库操作失败：{type(exc).__name__}"
