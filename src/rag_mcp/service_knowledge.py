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
from dataclasses import dataclass, field, replace
from functools import wraps
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
from .orchestrator import RagOrchestrator
from .pdf_content import PDF_PARSER_VERSION
from .word_reader import WORD_PARSER_VERSION
from .model_api import HTTPEmbedder, HTTPReranker, ModelAPIError, ModelConnection, ModelHTTPClient
from .model_settings import KINDS, ModelSettingsStore, draft_connection
from .knowledge_bases import KnowledgeBaseStore

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
    """Index result persisted in the tenant manifest."""

    document_id: str
    chunk_count: int
    collection: str
    diagnostics: list[dict] = field(default_factory=list)
    skipped_object_count: int = 0


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
            if chunking.get("pdf_parser_version") or chunking.get("word_parser_version"):
                from .chunking import _source_name, _stable_id

                document_id = _stable_id("doc", _source_name(path, self.paths.knowledge_dir).casefold())
                cached = self.paths.knowledge_dir / (".word-assets" if chunking.get("word_parser_version") else ".pdf-assets") / document_id
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
                        *, deduplicate_across_names: bool = True, embedding_model: str | None = None) -> dict[str, object]:
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
            if embedding_model is not None:
                configuration["embedding_model"] = embedding_model
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
            "diagnostics": indexed.diagnostics,
            "skipped_object_count": indexed.skipped_object_count,
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
        parsed_assets = None
        try:
            if path.suffix.casefold() == ".pdf":
                from .pdf_assets import parse_pdf

                parsed_assets = parse_pdf(
                    path, source_root=source_root, config=chunking,
                    tokenizer_model=self._config.chunk_tokenizer_model,
                    parser_python=self._config.pdf_parser_python,
                    models_path=self._config.pdf_models_path,
                    timeout_seconds=self._config.pdf_parse_timeout,
                )
                chunks = parsed_assets.chunks
            elif path.suffix.casefold() in {".doc", ".docx"}:
                from .word_assets import parse_word

                parsed_assets = parse_word(
                    path, source_root=source_root, config=chunking,
                    tokenizer_model=self._config.chunk_tokenizer_model,
                    converter=self._config.word_converter_path,
                    convert_timeout=self._config.word_convert_timeout,
                    parser_python=self._config.pdf_parser_python,
                    models_path=self._config.pdf_models_path,
                    timeout_seconds=self._config.pdf_parse_timeout,
                )
                chunks = parsed_assets.chunks
            else:
                chunks = chunk_document(path, source_root=source_root, config=chunking,
                                        tokenizer_model=self._config.chunk_tokenizer_model)
        except ValueError as exc:
            raise KnowledgeValidationError(str(exc)) from exc
        if not chunks:
            raise KnowledgeValidationError(f"知识文件没有可索引的文本：{path.name}")
        try:
            vectors = self._embedder.embed_documents([chunk.text for chunk in chunks])
            sparse_vectors = self._sparse_encoder.encode_documents([chunk.text for chunk in chunks])
        except Exception:
            if parsed_assets is not None:
                shutil.rmtree(parsed_assets.staging_dir, ignore_errors=True)
            raise
        final = backup = None
        try:
            asset_payloads = []
            if parsed_assets is not None:
                from .pdf_assets import install_assets

                final, backup = install_assets(parsed_assets, source_root, chunks[0].document_id)
                asset_payloads = [asset.payload(chunks[0].document_id, chunks[0].source) for asset in parsed_assets.assets]
            indexed = IndexedDocument(chunks[0].document_id, len(chunks), self._config.collection,
                                      getattr(parsed_assets, "diagnostics", []), getattr(parsed_assets, "skipped_object_count", 0))
            count = self._store.replace_document(
                chunks, vectors, sparse_vectors, asset_payloads,
                after_write=(lambda: publish(indexed)) if publish is not None else None,
            )
        except Exception:
            if final is not None:
                from .pdf_assets import rollback_assets

                rollback_assets(final, backup)
            elif parsed_assets is not None:
                shutil.rmtree(parsed_assets.staging_dir, ignore_errors=True)
            raise
        if backup is not None:
            from .pdf_assets import finish_assets

            finish_assets(backup)
        self._source_root = source_root
        return replace(indexed, chunk_count=count)

    def delete(self, document_id: str) -> None:
        self._store.delete_document(document_id)
        if self._source_root is not None:
            shutil.rmtree(self._source_root / ".pdf-assets" / document_id, ignore_errors=True)
            shutil.rmtree(self._source_root / ".word-assets" / document_id, ignore_errors=True)


@dataclass(frozen=True)
class TenantResources:
    documents: KnowledgeDocumentStore
    ingester: DenseDocumentIngester
    orchestrator: RagOrchestrator
    lock: RLock


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

    def __init__(self, config: RagConfig, data_root: Path) -> None:
        if config.backend != "qdrant":
            raise KnowledgeError("Standalone RAG service requires RAG_BACKEND=qdrant.")
        from qdrant_client import QdrantClient

        from .sparse import Bm25SparseEncoder

        self.config = config
        self.data_root = data_root.expanduser().resolve()
        if self.data_root.is_symlink():
            raise KnowledgeError("RAG service data root cannot be a symbolic link.")
        self.data_root.mkdir(parents=True, exist_ok=True)
        self._settings_store = ModelSettingsStore(self.data_root)
        self._connections, collection = self._settings_store.load({
            "embedding": ModelConnection(model=config.embedding_model),
            "reranker": ModelConnection(model=config.reranker_model),
        }, config.collection)
        self.config = replace(config, collection=collection,
                              embedding_model=self._connections["embedding"].model,
                              reranker_model=self._connections["reranker"].model,
                              vector_size=self._connections["embedding"].vector_size or config.vector_size)
        self._model_checks: dict[str, dict[str, object]] = {}
        self._client = QdrantClient(
            url=config.qdrant_url,
            path=str(config.qdrant_path) if config.qdrant_path else None,
        )
        self._embedder = HTTPEmbedder(self._connections["embedding"])
        self._sparse_encoder = Bm25SparseEncoder()
        self._reranker = HTTPReranker(
            self._connections["reranker"],
            max_candidates=config.reranker_max_candidates,
            max_text_chars=config.reranker_max_text_chars,
        )
        self._tenants: dict[str, TenantResources] = {}
        self._tenant_lock = RLock()
        self._inference_lock = RLock()

    def status(self) -> dict[str, object]:
        return {
            "status": "ok",
            "service": "praxis-rag",
            "backend": self.config.backend,
            "collection": self.config.collection,
            "models": {kind: {"name": item.model, "configured": item.configured, "provider": "http"}
                       for kind, item in self._connections.items()},
        }

    def model_settings(self) -> dict[str, object]:
        return {kind: {**item.public(), **self._model_checks.get(kind, {})}
                for kind, item in self._connections.items()}

    @_runtime_operation
    def model_action(self, kind: str, values: dict[str, object], action: str) -> dict[str, object]:
        if kind not in KINDS or action not in {"models", "test", "save"}:
            raise ModelAPIError("未知模型设置操作")
        current = self._connections[kind]
        draft = draft_connection(current, values, require_model=action != "models")
        if action == "models":
            return {"models": ModelHTTPClient(draft).models()}
        try:
            if kind == "embedding":
                vectors = HTTPEmbedder(draft).embed_documents(["connection test"])
                draft = replace(draft, vector_size=len(vectors[0]))
            else:
                HTTPReranker(draft).scores("connection test", ["connection test"])
        except ModelAPIError as exc:
            if draft.base_url == current.base_url and draft.model == current.model and draft.api_key == current.api_key:
                self._model_checks[kind] = {"status": "error", "last_error": str(exc),
                                            "checked_at": datetime.now(UTC).isoformat()}
            raise
        checked = {"status": "ok", "last_error": "", "checked_at": datetime.now(UTC).isoformat()}
        if action == "test":
            if draft.base_url == current.base_url and draft.model == current.model and draft.api_key == current.api_key:
                self._model_checks[kind] = checked
            return {**draft.public(), **checked}

        collection = self.config.collection
        size = self.config.vector_size
        if kind == "embedding":
            size = draft.vector_size
            if any(item["vector_size"] != size or item["embedding_model"] != draft.model
                   for item in KnowledgeBaseStore(self.data_root).list()):
                raise ModelAPIError("已有知识库绑定当前 Embedding 模型和向量维度，不能更换模型或维度")
            occupied = self._has_indexed_documents()
            current_size = self.config.vector_size
            try:
                if self._client.collection_exists(collection):
                    current_size = self._client.get_collection(collection).config.params.vectors["dense"].size
            except Exception as exc:
                raise ModelAPIError("无法确认现有索引的向量长度，未修改配置") from exc
            if occupied and (draft.model != self.config.embedding_model or size != current_size):
                raise ModelAPIError("已有知识文档，不能更换 Embedding 模型或向量长度；请先清空所有工作区")
            if size != current_size:
                from .storage.qdrant_store import QdrantStore

                collection = f"{self.config.collection[:120]}_api_{uuid4().hex[:12]}"
                QdrantStore(collection=collection, vector_size=size, client=self._client).ensure_collection()
        connections = {**self._connections, kind: draft}
        config = replace(self.config, collection=collection, vector_size=size,
                         embedding_model=connections["embedding"].model, reranker_model=connections["reranker"].model)
        embedder = HTTPEmbedder(connections["embedding"])
        reranker = HTTPReranker(connections["reranker"], max_candidates=config.reranker_max_candidates,
                               max_text_chars=config.reranker_max_text_chars)
        self._settings_store.save(connections, collection)
        self.config = config
        self._embedder, self._reranker = embedder, reranker
        self._connections = connections
        self._model_checks[kind] = checked
        self._tenants.clear()
        return {**draft.public(), **checked}

    def _has_indexed_documents(self) -> bool:
        try:
            for manifest in self.data_root.glob("*/manifest.json"):
                value = json.loads(manifest.read_text(encoding="utf-8"))
                if not isinstance(value, dict):
                    raise ValueError("invalid manifest")
                if value:
                    return True
            if self._client.collection_exists(self.config.collection):
                return self._client.count(collection_name=self.config.collection, exact=True).count > 0
            return False
        except Exception as exc:
            raise ModelAPIError("无法确认知识库是否为空，未修改模型配置") from exc

    @_runtime_operation
    def knowledge_base_defaults(self) -> dict[str, object]:
        return {"vector_size": self.config.vector_size, "embedding_model": self.config.embedding_model,
                "tokenizer": self.config.chunk_tokenizer_model,
                "embedding_configured": self._connections["embedding"].configured and self._connections["embedding"].vector_size is not None}

    @_runtime_operation
    def create_knowledge_base(self, namespace: str, chunking: ChunkingConfig, vector_size: int) -> dict:
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,63}", namespace):
            raise KnowledgeValidationError("知识库标识无效")
        store = KnowledgeBaseStore(self.data_root)
        if store.get(namespace) is not None:
            raise KnowledgeValidationError("该知识库已存在，创建参数不可修改")
        embedding = self._connections["embedding"]
        if not embedding.configured or embedding.vector_size is None:
            raise KnowledgeValidationError("请先在模型配置中保存并验证 Embedding 模型")
        if vector_size != self.config.vector_size:
            raise KnowledgeValidationError(f"向量维度必须与当前 Embedding 模型一致：{self.config.vector_size}")
        documents = KnowledgeDocumentStore(KnowledgePaths(self.data_root / namespace)).list_documents()
        if documents:
            raise KnowledgeValidationError("该 Namespace 已有文档，请使用新的知识库标识")
        settings = {"namespace": namespace, "vector_size": vector_size,
                    "embedding_model": self.config.embedding_model,
                    "tokenizer": self.config.chunk_tokenizer_model,
                    "strategy": chunking.strategy, "chunk_size": chunking.chunk_size,
                    "chunk_overlap": chunking.chunk_overlap,
                    "created_at": datetime.now(UTC).isoformat()}
        store.create(settings)
        return settings

    @_runtime_operation
    def knowledge_base(self, namespace: str) -> dict:
        settings = KnowledgeBaseStore(self.data_root).get(namespace)
        if settings is None:
            raise KnowledgeValidationError("请先在接入配置中创建知识库")
        return settings

    def _knowledge_chunking(self, namespace: str) -> ChunkingConfig:
        settings = self.knowledge_base(namespace)
        if settings["embedding_model"] != self.config.embedding_model or settings["vector_size"] != self.config.vector_size:
            raise KnowledgeValidationError("当前 Embedding 模型与知识库创建配置不一致")
        return ChunkingConfig(settings["strategy"], settings["chunk_size"], settings["chunk_overlap"])

    @_runtime_operation
    def list_documents(self, namespace: str) -> list[dict[str, object]]:
        tenant = self._tenant(namespace)
        with tenant.lock:
            return tenant.documents.list_documents()

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
    def upload(self, namespace: str, items: list[tuple[str, object]]) -> dict[str, object]:
        chunking = self._knowledge_chunking(namespace)
        tenant = self._tenant(namespace)
        tenant.documents.validate_uploads(items)
        model = self.knowledge_base(namespace)["tokenizer"]
        imported: list[dict[str, object]] = []
        skipped: list[dict[str, object]] = []
        failed: list[dict[str, str]] = []
        with tenant.lock:
            for name, readable in items:
                try:
                    with self._inference_lock:
                        record = tenant.documents.store_and_index(name, readable, tenant.ingester, chunking, model,
                                                                  embedding_model=self.config.embedding_model)
                        if record.pop("skipped", False):
                            skipped.append(record)
                        else:
                            imported.append(record)
                except Exception as exc:
                    failed.append({"name": tenant.documents.sanitize_name(name), "error": _safe_error(exc)})
            documents = tenant.documents.list_documents()
        return {"imported": imported, "skipped": skipped, "failed": failed, "documents": documents}

    @_runtime_operation
    def write_document(self, namespace: str, name: str, content: bytes, *, overwrite: bool) -> dict[str, object]:
        configuration = self._knowledge_chunking(namespace)
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
            readable = io.BytesIO(content)
            tenant.documents.validate_uploads([(name, readable)])
            try:
                with self._inference_lock:
                    record = tenant.documents.store_and_index(
                        name, readable, tenant.ingester, configuration, self.knowledge_base(namespace)["tokenizer"],
                        deduplicate_across_names=False,
                        embedding_model=self.config.embedding_model,
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
    def search(self, namespace: str, arguments: Mapping[str, object]) -> dict[str, object]:
        tenant = self._tenant(namespace)
        request = SearchRequest.from_arguments(dict(arguments))
        with tenant.lock, self._inference_lock:
            return tenant.orchestrator.search(request)

    @_runtime_operation
    def read_document_file(self, namespace: str, name: str) -> bytes:
        tenant = self._tenant(namespace)
        with tenant.lock:
            if tenant.documents.sanitize_name(name) != name:
                raise KnowledgeValidationError("Invalid document name")
            if not any(item["name"] == name for item in tenant.documents.list_documents()):
                raise KnowledgeValidationError("Document does not exist")
            target = tenant.documents._confined_target(name)
            if (tenant.documents.paths.knowledge_dir / name).is_symlink():
                raise KnowledgeValidationError("Document cannot be a symbolic link")
            if not target.is_file():
                raise KnowledgeValidationError("Document does not exist")
            with target.open("rb") as source:
                content = source.read(MAX_FILE_BYTES + 1)
            if len(content) > MAX_FILE_BYTES:
                raise KnowledgeValidationError("Document exceeds the file size limit")
            return content

    @_runtime_operation
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
            elif target.suffix.casefold() in {".doc", ".docx"} and record.get("chunking", {}).get("word_parser_version"):
                from .word_assets import read_word_content

                try:
                    text = read_word_content(target, tenant.documents.paths.knowledge_dir)
                except ValueError as exc:
                    raise KnowledgeValidationError(str(exc)) from exc
            else:
                text = read_document_text(target)
            return text, str(record["indexed_at"])

    def _tenant(self, namespace: str) -> TenantResources:
        with self._tenant_lock:
            existing = self._tenants.get(namespace)
            if existing is not None:
                return existing
            from .retrievers.dense import DenseRetriever
            from .retrievers.hybrid import Bm25Retriever, HybridRetriever
            from .pdf_assets import project_results
            from .storage.qdrant_store import QdrantStore

            config = replace(self.config, namespace=namespace)
            settings = KnowledgeBaseStore(self.data_root).get(namespace)
            if settings:
                if settings["vector_size"] != config.vector_size or settings["embedding_model"] != config.embedding_model:
                    raise KnowledgeValidationError("当前 Embedding 模型与知识库创建配置不一致")
                config = replace(config, vector_size=settings["vector_size"],
                                 chunk_tokenizer_model=settings["tokenizer"])

            store = QdrantStore(
                collection=config.collection,
                vector_size=config.vector_size,
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
                },
                implementation="rag-service-qdrant-hybrid-rerank-v1",
                projector=lambda candidates: project_results(
                    candidates, store, self.data_root / namespace / "knowledge" / ".pdf-assets",
                    word_assets_root=self.data_root / namespace / "knowledge" / ".word-assets",
                ),
            )
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
    if isinstance(exc, (KnowledgeError, ModelAPIError)):
        return str(exc)[:500]
    return f"知识库操作失败：{type(exc).__name__}"
