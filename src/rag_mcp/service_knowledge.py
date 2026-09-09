"""Tenant-confined source storage and indexing owned by the RAG service."""

from __future__ import annotations

import json
import logging
import hashlib
import io
import os
import re
import shutil
from collections.abc import Callable, Mapping
from dataclasses import dataclass, replace
from datetime import UTC, datetime
from pathlib import Path
from threading import RLock
from typing import Protocol
from uuid import uuid4

from .chunking import chunk_document
from .chunk_strategies import ChunkingConfig, LEGACY_CONFIG
from .config import RagConfig
from .contracts import SearchRequest
from .document_readers import SUPPORTED_SUFFIXES, read_document_text
from .mock import MockReranker
from .orchestrator import RagOrchestrator
from .pdf_content import PDF_PARSER_VERSION

SUPPORTED_EXTENSIONS = SUPPORTED_SUFFIXES
MAX_FILES_PER_BATCH = 10
MAX_FILE_BYTES = 10 * 1024 * 1024
_READ_SIZE = 1024 * 1024
_WINDOWS_RESERVED = re.compile(r'[<>:"/\\|?*\x00-\x1f]')


def _index_configuration(name: str, configuration: dict) -> dict:
    if Path(name).suffix.casefold() == ".pdf":
        return {**configuration, "pdf_parser_version": PDF_PARSER_VERSION}
    return configuration


class KnowledgeError(RuntimeError):
    """Base error returned by the RAG service knowledge API."""


class KnowledgeValidationError(KnowledgeError):
    """Raised before an unsupported or unsafe operation changes storage."""


@dataclass(frozen=True)
class IndexedDocument:
    """Index result persisted in the tenant manifest."""

    document_id: str
    chunk_count: int
    collection: str


class DocumentIngester(Protocol):
    def ingest(self, path: Path, *, source_root: Path, chunking: ChunkingConfig | None = None) -> IndexedDocument: ...

    def delete(self, document_id: str) -> None: ...


@dataclass(frozen=True)
class KnowledgePaths:
    """Physical paths fully owned by the standalone RAG service."""

    root: Path

    @property
    def knowledge_dir(self) -> Path:
        return self.root / "knowledge"

    @property
    def manifest_file(self) -> Path:
        return self.root / "manifest.json"

    def ensure(self) -> None:
        if self.root.is_symlink() or self.knowledge_dir.is_symlink():
            raise KnowledgeError("RAG tenant storage cannot be a symbolic link.")
        self.knowledge_dir.mkdir(parents=True, exist_ok=True)
        if not self.manifest_file.exists():
            self.manifest_file.write_text("{}", encoding="utf-8")


class KnowledgeDocumentStore:
    """Atomically persist tenant sources and their non-secret manifest."""

    def __init__(self, paths: KnowledgePaths) -> None:
        self.paths = paths
        self.paths.ensure()

    def list_documents(self) -> list[dict[str, object]]:
        manifest = self._read_manifest()
        documents = [dict(value) for value in manifest.values() if isinstance(value, dict)]
        return sorted(documents, key=lambda item: str(item.get("name", "")).casefold())

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

    def find_duplicate(self, sha256: str, chunking: dict[str, object], *, only_name: str | None = None) -> dict[str, object] | None:
        manifest = self._read_manifest()
        changed = False
        found = None
        for name, record in manifest.items():
            if only_name is not None and name != only_name:
                continue
            path = self._confined_target(name)
            if not path.is_file() or (self.paths.knowledge_dir / name).is_symlink():
                continue
            if chunking.get("pdf_parser_version"):
                from .chunking import _source_name, _stable_id

                document_id = _stable_id("doc", _source_name(path, self.paths.knowledge_dir).casefold())
                cached = self.paths.knowledge_dir / ".pdf-assets" / document_id
                if not (cached / "document.md").is_file() or not (cached / "manifest.json").is_file():
                    continue
            if not record.get("sha256"):
                with path.open("rb") as stream:
                    record["sha256"] = hashlib.file_digest(stream, "sha256").hexdigest()
                record.setdefault("chunking", LEGACY_CONFIG.metadata("BAAI/bge-m3"))
                changed = True
            if record["sha256"] == sha256 and record.get("chunking", LEGACY_CONFIG.metadata("BAAI/bge-m3")) == chunking:
                found = dict(record)
                break
        if changed:
            self._write_manifest(manifest)
        return found

    def store_and_index(self, raw_name: str, readable: object, ingester: DocumentIngester,
                        chunking: ChunkingConfig | None = None, tokenizer_model: str = "BAAI/bge-m3",
                        *, deduplicate_across_names: bool = True) -> dict[str, object]:
        """Replace one source and roll back the file if indexing fails."""

        name = self.sanitize_name(raw_name)
        target = self._confined_target(name)
        temporary = self.paths.knowledge_dir / f".upload-{uuid4().hex}.tmp"
        backup = self.paths.knowledge_dir / f".backup-{uuid4().hex}.tmp"
        replaced = target.exists()
        backed_up = False
        installed = False
        metadata = None

        def publish(indexed):
            nonlocal metadata
            metadata = self._metadata(target, indexed)
            metadata.update(sha256=sha256, chunking=configuration)
            manifest = self._read_manifest()
            manifest[name] = metadata
            self._write_manifest(manifest)

        try:
            sha256 = self._write_bounded(temporary, readable, name)
            if target.is_symlink():
                raise KnowledgeValidationError(f"知识文件不能是符号链接：{name}")
            configuration = _index_configuration(name, (chunking or LEGACY_CONFIG).metadata(tokenizer_model))
            previous = self.find_duplicate(sha256, configuration, only_name=None if deduplicate_across_names else name)
            if previous:
                temporary.unlink()
                return {**previous, "skipped": True, "requested_name": name}
            if replaced:
                os.replace(target, backup)
                backed_up = True
            os.replace(temporary, target)
            installed = True
            transactional = getattr(ingester, "ingest_and_publish", None)
            if transactional is not None:
                transactional(target, source_root=self.paths.knowledge_dir, chunking=chunking, publish=publish)
            else:
                publish(ingester.ingest(target, source_root=self.paths.knowledge_dir, chunking=chunking))
        except Exception:
            temporary.unlink(missing_ok=True)
            if installed:
                target.unlink(missing_ok=True)
            if backed_up and backup.exists():
                os.replace(backup, target)
            raise
        try:
            backup.unlink(missing_ok=True)
        except OSError:
            logging.getLogger(__name__).warning("Indexed document published; source backup cleanup failed: %s", backup)
        return metadata

    def delete(self, name: str, ingester: DocumentIngester) -> dict[str, object]:
        safe_name = self.sanitize_name(name)
        manifest = self._read_manifest()
        record = manifest.get(safe_name)
        if not isinstance(record, dict):
            raise KnowledgeValidationError(f"知识文件不存在：{safe_name}")
        document_id = record.get("document_id")
        if not isinstance(document_id, str) or not document_id:
            raise KnowledgeError(f"知识文件索引记录损坏：{safe_name}")
        ingester.delete(document_id)
        self._confined_target(safe_name).unlink(missing_ok=True)
        manifest.pop(safe_name, None)
        self._write_manifest(manifest)
        return record

    @staticmethod
    def sanitize_name(raw_name: str) -> str:
        name = _WINDOWS_RESERVED.sub("_", str(raw_name or "")).strip(" .")[:200]
        if not name or name in {".", ".."}:
            raise KnowledgeValidationError("知识文件名不能为空。")
        return name

    def _confined_target(self, name: str) -> Path:
        root = self.paths.knowledge_dir.resolve()
        target = (root / name).resolve()
        if target.parent != root:
            raise KnowledgeValidationError("知识文件路径必须位于租户知识目录内。")
        return target

    def _write_bounded(self, target: Path, readable: object, name: str) -> str:
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

    def _metadata(self, path: Path, indexed: IndexedDocument) -> dict[str, object]:
        stat = path.stat()
        return {
            "name": path.name,
            "size": stat.st_size,
            "modified_at": datetime.fromtimestamp(stat.st_mtime, tz=UTC).isoformat(),
            "indexed_at": datetime.now(UTC).isoformat(),
            "document_id": indexed.document_id,
            "chunk_count": indexed.chunk_count,
            "collection": indexed.collection,
        }

    def _read_manifest(self) -> dict[str, dict[str, object]]:
        try:
            raw = self.paths.manifest_file.read_text(encoding="utf-8").strip()
            value = json.loads(raw) if raw else {}
        except (OSError, json.JSONDecodeError) as exc:
            raise KnowledgeError("知识库清单无法读取。") from exc
        if not isinstance(value, dict):
            raise KnowledgeError("知识库清单格式无效。")
        return {str(key): dict(item) for key, item in value.items() if isinstance(item, dict)}

    def _write_manifest(self, value: dict[str, dict[str, object]]) -> None:
        temporary = self.paths.root / f".manifest-{uuid4().hex}.tmp"
        try:
            temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2), encoding="utf-8")
            os.replace(temporary, self.paths.manifest_file)
        finally:
            temporary.unlink(missing_ok=True)


class DenseDocumentIngester:
    """Index documents with service-owned shared model dependencies."""

    def __init__(self, config: RagConfig, embedder, sparse_encoder, store, *, source_root: Path | None = None) -> None:
        self._config = config
        self._embedder = embedder
        self._sparse_encoder = sparse_encoder
        self._store = store
        self._source_root = source_root

    def ingest(self, path: Path, *, source_root: Path, chunking: ChunkingConfig | None = None) -> IndexedDocument:
        return self.ingest_and_publish(path, source_root=source_root, chunking=chunking)

    def ingest_and_publish(self, path: Path, *, source_root: Path, chunking: ChunkingConfig | None = None,
                           publish: Callable[[IndexedDocument], None] | None = None) -> IndexedDocument:
        parsed_pdf = None
        try:
            if path.suffix.casefold() == ".pdf":
                from .pdf_assets import parse_pdf

                parsed_pdf = parse_pdf(
                    path, source_root=source_root, config=chunking,
                    tokenizer_model=self._config.embedding_model,
                    parser_python=self._config.pdf_parser_python,
                    models_path=self._config.pdf_models_path,
                    timeout_seconds=self._config.pdf_parse_timeout,
                )
                chunks = parsed_pdf.chunks
            else:
                chunks = chunk_document(path, source_root=source_root, config=chunking,
                                        tokenizer_model=self._config.embedding_model)
        except ValueError as exc:
            raise KnowledgeValidationError(str(exc)) from exc
        if not chunks:
            raise KnowledgeValidationError(f"知识文件没有可索引的文本：{path.name}")
        try:
            vectors = self._embedder.embed_documents([chunk.text for chunk in chunks])
            sparse_vectors = self._sparse_encoder.encode_documents([chunk.text for chunk in chunks])
        except Exception:
            if parsed_pdf is not None:
                shutil.rmtree(parsed_pdf.staging_dir, ignore_errors=True)
            raise
        final = backup = None
        try:
            asset_payloads = []
            if parsed_pdf is not None:
                from .pdf_assets import install_assets

                final, backup = install_assets(parsed_pdf, source_root, chunks[0].document_id)
                asset_payloads = [asset.payload(chunks[0].document_id, chunks[0].source) for asset in parsed_pdf.assets]
            indexed = IndexedDocument(chunks[0].document_id, len(chunks), self._config.collection)
            count = self._store.replace_document(
                chunks, vectors, sparse_vectors, asset_payloads,
                after_write=(lambda: publish(indexed)) if publish is not None else None,
            )
        except Exception:
            if final is not None:
                from .pdf_assets import rollback_assets

                rollback_assets(final, backup)
            elif parsed_pdf is not None:
                shutil.rmtree(parsed_pdf.staging_dir, ignore_errors=True)
            raise
        if backup is not None:
            from .pdf_assets import finish_assets

            finish_assets(backup)
        self._source_root = source_root
        return IndexedDocument(chunks[0].document_id, count, self._config.collection)

    def delete(self, document_id: str) -> None:
        self._store.delete_document(document_id)
        if self._source_root is not None:
            shutil.rmtree(self._source_root / ".pdf-assets" / document_id, ignore_errors=True)


@dataclass(frozen=True)
class TenantResources:
    documents: KnowledgeDocumentStore
    ingester: DenseDocumentIngester
    orchestrator: RagOrchestrator
    lock: RLock


class RagServiceRuntime:
    """Long-lived model/Qdrant owner serving isolated tenant namespaces."""

    def __init__(self, config: RagConfig, data_root: Path) -> None:
        if config.backend != "qdrant":
            raise KnowledgeError("Standalone RAG service requires RAG_BACKEND=qdrant.")
        from qdrant_client import QdrantClient

        from .embeddings import BgeM3Embedder
        from .rerankers.cross_encoder import CrossEncoderReranker
        from .sparse import Bm25SparseEncoder

        self.config = config
        self.data_root = data_root.expanduser().resolve()
        if self.data_root.is_symlink():
            raise KnowledgeError("RAG service data root cannot be a symbolic link.")
        self.data_root.mkdir(parents=True, exist_ok=True)
        self._client = QdrantClient(
            url=config.qdrant_url,
            path=str(config.qdrant_path) if config.qdrant_path else None,
        )
        self._embedder = BgeM3Embedder(
            config.embedding_model,
            device=config.embedding_device,
            vector_size=config.vector_size,
        )
        self._sparse_encoder = Bm25SparseEncoder()
        self._reranker = CrossEncoderReranker(
            config.reranker_model,
            device=config.reranker_device,
            batch_size=config.reranker_batch_size,
            max_candidates=config.reranker_max_candidates,
            max_length=config.reranker_max_length,
            max_text_chars=config.reranker_max_text_chars,
        )
        self._tenants: dict[str, TenantResources] = {}
        self._tenant_lock = RLock()
        self._inference_lock = RLock()

    def status(self) -> dict[str, object]:
        return {
            "status": "ok",
            "service": "rag-mcp-service",
            "backend": self.config.backend,
            "collection": self.config.collection,
            "models": {
                "embedding": {
                    "name": self.config.embedding_model,
                    "device": self.config.embedding_device or "auto",
                    "loaded": self._embedder.is_loaded,
                },
                "reranker": {
                    "name": self.config.reranker_model,
                    "device": self.config.reranker_device or "auto",
                    "loaded": self._reranker.is_loaded,
                },
            },
        }

    def list_documents(self, namespace: str) -> list[dict[str, object]]:
        tenant = self._tenant(namespace)
        with tenant.lock:
            return tenant.documents.list_documents()

    def chunking_metadata(self, chunking: ChunkingConfig) -> dict[str, object]:
        return chunking.metadata(self.config.embedding_model)

    def check_documents(self, namespace: str, items: list[dict[str, str]],
                        chunking: ChunkingConfig) -> dict[str, object]:
        tenant = self._tenant(namespace)
        tenant.documents.validate_uploads([(item["name"], None) for item in items])
        configuration = self.chunking_metadata(chunking)
        with tenant.lock:
            return {"files": [{"name": item["name"], "sha256": item["sha256"],
                               "document": tenant.documents.find_duplicate(
                                   item["sha256"], _index_configuration(item["name"], configuration))}
                              for item in items]}

    def upload(self, namespace: str, items: list[tuple[str, object]],
               chunking: ChunkingConfig | None = None) -> dict[str, object]:
        tenant = self._tenant(namespace)
        tenant.documents.validate_uploads(items)
        model = self.config.embedding_model
        imported: list[dict[str, object]] = []
        skipped: list[dict[str, object]] = []
        failed: list[dict[str, str]] = []
        with tenant.lock:
            for name, readable in items:
                try:
                    with self._inference_lock:
                        record = tenant.documents.store_and_index(name, readable, tenant.ingester, chunking, model)
                        if record.pop("skipped", False):
                            skipped.append(record)
                        else:
                            imported.append(record)
                except Exception as exc:
                    failed.append({"name": tenant.documents.sanitize_name(name), "error": _safe_error(exc)})
            documents = tenant.documents.list_documents()
        return {"imported": imported, "skipped": skipped, "failed": failed, "documents": documents}

    def write_document(self, namespace: str, name: str, content: bytes, *, overwrite: bool) -> dict[str, object]:
        tenant = self._tenant(namespace)
        if tenant.documents.sanitize_name(name) != name:
            raise KnowledgeValidationError("Invalid document name")
        with tenant.lock:
            records = tenant.documents.list_documents()
            existing = next((item for item in records if item["name"] == name), None)
            if overwrite and existing is None:
                raise KnowledgeValidationError(f"Document does not exist: {name}")
            if not overwrite and (any(str(item["name"]).casefold() == name.casefold() for item in records)
                                  or tenant.documents._confined_target(name).exists()):
                raise KnowledgeValidationError(f"Document already exists: {name}; use update_document")
            configuration = ChunkingConfig()
            if existing is not None:
                saved = existing.get("chunking", LEGACY_CONFIG.metadata(self.config.embedding_model))
                configuration = ChunkingConfig(str(saved["strategy"]), saved["chunk_size"], saved["chunk_overlap"])
            readable = io.BytesIO(content)
            tenant.documents.validate_uploads([(name, readable)])
            try:
                with self._inference_lock:
                    record = tenant.documents.store_and_index(
                        name, readable, tenant.ingester, configuration, self.config.embedding_model,
                        deduplicate_across_names=False,
                    )
            except Exception as exc:
                raise KnowledgeError(_safe_error(exc)) from exc
            unchanged = record.pop("skipped", False)
            record.pop("requested_name", None)
            return {"status": "unchanged" if unchanged else ("updated" if overwrite else "added"),
                    "document": record, "documents": tenant.documents.list_documents()}

    def delete(self, namespace: str, name: str) -> dict[str, object]:
        tenant = self._tenant(namespace)
        with tenant.lock:
            deleted = tenant.documents.delete(name, tenant.ingester)
            documents = tenant.documents.list_documents()
        return {"deleted": deleted, "documents": documents}

    def search(self, namespace: str, arguments: Mapping[str, object]) -> dict[str, object]:
        tenant = self._tenant(namespace)
        request = SearchRequest.from_arguments(dict(arguments))
        with tenant.lock, self._inference_lock:
            return tenant.orchestrator.search(request)

    def read_document(self, namespace: str, name: str) -> tuple[str, str]:
        tenant = self._tenant(namespace)
        with tenant.lock:
            if tenant.documents.sanitize_name(name) != name:
                raise KnowledgeValidationError("Invalid document name")
            record = next((item for item in tenant.documents.list_documents() if item["name"] == name), None)
            if record is None:
                raise KnowledgeValidationError("Document does not exist")
            target = tenant.documents._confined_target(name)
            if (tenant.documents.paths.knowledge_dir / name).is_symlink():
                raise KnowledgeValidationError("Document cannot be a symbolic link")
            if target.suffix.casefold() == ".pdf":
                from .pdf_assets import read_pdf_content

                try:
                    text = read_pdf_content(target, tenant.documents.paths.knowledge_dir)
                except ValueError as exc:
                    raise KnowledgeValidationError(str(exc)) from exc
            else:
                text = read_document_text(target)
            return text, str(record["indexed_at"])

    def warmup(self) -> None:
        self._embedder.warmup()
        if self.config.preload_reranker:
            self._reranker.warmup()

    def _tenant(self, namespace: str) -> TenantResources:
        with self._tenant_lock:
            existing = self._tenants.get(namespace)
            if existing is not None:
                return existing
            from .retrievers.dense import DenseRetriever
            from .retrievers.hybrid import Bm25Retriever, HybridRetriever
            from .pdf_assets import project_results
            from .storage.qdrant_store import QdrantStore

            store = QdrantStore(
                collection=self.config.collection,
                vector_size=self.config.vector_size,
                namespace=namespace,
                client=self._client,
            )
            orchestrator = RagOrchestrator(
                retrievers={
                    "bm25": Bm25Retriever(self._sparse_encoder, store),
                    "embedding": DenseRetriever(self._embedder, store),
                    "hybrid": HybridRetriever(self._embedder, self._sparse_encoder, store),
                },
                rerankers={
                    "cross_encoder": self._reranker,
                    "bi_encoder": MockReranker("bi_encoder"),
                },
                implementation="rag-service-qdrant-hybrid-rerank-v1",
                limitations=("bi_encoder remains a deterministic mock.",),
                projector=lambda candidates: project_results(
                    candidates, store, self.data_root / namespace / "knowledge" / ".pdf-assets"
                ),
            )
            config = replace(self.config, namespace=namespace)
            resources = TenantResources(
                documents=KnowledgeDocumentStore(KnowledgePaths(self.data_root / namespace)),
                ingester=DenseDocumentIngester(
                    config, self._embedder, self._sparse_encoder, store,
                    source_root=self.data_root / namespace / "knowledge",
                ),
                orchestrator=orchestrator,
                lock=RLock(),
            )
            self._tenants[namespace] = resources
            return resources


def _safe_error(exc: Exception) -> str:
    if isinstance(exc, KnowledgeError):
        return str(exc)[:500]
    return f"知识库操作失败：{type(exc).__name__}"
