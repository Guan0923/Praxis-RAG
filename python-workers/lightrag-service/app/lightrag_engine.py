"""Production :class:`~app.engine.GraphEngine` backed by ``lightrag-hku``.

Everything upstream-specific is confined to this file and imported **lazily**:
the HTTP layer, the service and the tests never import ``lightrag``, which is why
the test suite runs without the (large) upstream dependency tree installed.

Upstream facts this adapter relies on (pinned release 1.5.7 — verify on every
upgrade, they are listed in README.md):

* ``LightRAG(working_dir=..., workspace=..., llm_model_func=..., role_llm_configs=...,
  embedding_func=..., addon_params=...)`` and ``await rag.initialize_storages()``;
* ``await rag.ainsert_custom_chunks(full_text, text_chunks, doc_id)`` accepts
  caller-split chunks and never splits text itself;
* chunk row ids are ``make_custom_chunk_id(doc_id, sanitized_text)``;
* ``await rag.aquery_data(query, param=QueryParam(mode=..., only_need_context=True,
  top_k=..., chunk_top_k=...))`` returns structured retrieval data and performs no
  LLM generation;
* ``await rag.adelete_by_doc_id(doc_id)`` removes a document and its graph rows.

The sidecar keeps one LightRAG working directory per workspace
(``<WORKING_DIR>/workspaces/praxis_<ns>/lightrag``) so clearing a workspace is a
single directory removal instead of guessing upstream file-name patterns.
"""

from __future__ import annotations

import asyncio
import importlib.metadata
import logging
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from . import LIGHTRAG_PACKAGE, SERVICE_NAME
from .config import WorkspaceConfig
from .engine import (
    SUPPORTED_MODES,
    DocumentInput,
    EngineQueryResult,
    GraphEngine,
    InsertResult,
    custom_chunk_key,
)
from .errors import EngineUnavailableError, InputError, NotFoundError
from .upstream_http import build_completion, build_embedding
from .upstream_query import is_query_validation_error, parse_query_payload

LOGGER = logging.getLogger(SERVICE_NAME)

#: Fallback separator LightRAG uses to join chunk ids in ``source_id``.
GRAPH_FIELD_SEP_FALLBACK = "<SEP>"
#: Per-request embedding HTTP deadline. Embedding batches are small and local
#: model servers answer quickly; a hung embedding endpoint must not pin a slot.
DEFAULT_EMBEDDING_TIMEOUT = 60.0
#: Internal LightRAG concurrency. The sidecar's own gate is the outer bound.
DEFAULT_LLM_MAX_ASYNC = 8
DEFAULT_EMBEDDING_MAX_ASYNC = 8


@dataclass(frozen=True)
class _SdkApi:
    """Resolved entry points of the pinned LightRAG package.

    Attributes:
        lightrag_class: ``lightrag.LightRAG``.
        query_param_class: ``lightrag.QueryParam``.
        role_config_class: ``lightrag.RoleLLMConfig``.
        embedding_func_class: ``lightrag.utils.EmbeddingFunc``.
    """

    lightrag_class: Any
    query_param_class: Any
    role_config_class: Any
    embedding_func_class: Any


class LightRAGEngine(GraphEngine):
    """Graph engine adapter for one process.

    One adapter instance caches one LightRAG object per workspace; the service
    object is shared by all requests, so the cache also keeps upstream file
    handles alive between requests.

    Args:
        working_dir: Service working directory.
        embedding_timeout: HTTP deadline for one embedding call.
        llm_max_async: LightRAG's internal LLM concurrency.
        embedding_max_async: LightRAG's internal embedding concurrency.
    """

    def __init__(
        self,
        working_dir: str | Path,
        *,
        embedding_timeout: float = DEFAULT_EMBEDDING_TIMEOUT,
        llm_max_async: int = DEFAULT_LLM_MAX_ASYNC,
        embedding_max_async: int = DEFAULT_EMBEDDING_MAX_ASYNC,
    ) -> None:
        """Remember the storage root and the upstream concurrency knobs."""

        self._working_dir = Path(working_dir)
        self._embedding_timeout = embedding_timeout
        self._llm_max_async = max(1, llm_max_async)
        self._embedding_max_async = max(1, embedding_max_async)
        self._instances: dict[str, Any] = {}
        self._init_locks: dict[str, asyncio.Lock] = {}
        self._sdk: _SdkApi | None = None
        self._sanitize_func: Any = None
        self._chunk_key_func: Any = None
        self._graph_sep: str | None = None

    def installed_version(self) -> str | None:
        """Read the installed distribution version without importing the package.

        Returns:
            Version string, or ``None`` when ``lightrag-hku`` is not installed.
        """

        try:
            return importlib.metadata.version(LIGHTRAG_PACKAGE)
        except Exception:
            return None

    def availability_error(self) -> str | None:
        """Check that the pinned upstream package can actually be imported.

        Returns:
            Safe Chinese description of the problem, or ``None`` when usable.
        """

        try:
            import lightrag  # noqa: F401
        except Exception:
            return "lightrag-hku 未正确安装：无法导入 lightrag"
        if self.installed_version() is None:
            return "lightrag-hku 版本信息不可用"
        return None

    async def ensure_workspace(self, config: WorkspaceConfig) -> None:
        """Create the LightRAG instance of a workspace when needed.

        Args:
            config: Credential-bearing binding from ``POST /v1/workspaces``.

        Returns:
            ``None``; subsequent calls for the same workspace are no-ops.

        Raises:
            EngineUnavailableError: LightRAG is missing or initialization failed.
        """

        sdk = self._load_sdk()
        if config.workspace in self._instances:
            return
        lock = self._init_locks.setdefault(config.workspace, asyncio.Lock())
        async with lock:
            if config.workspace in self._instances:
                return
            directory = self._workspace_dir(config.workspace)
            directory.mkdir(parents=True, exist_ok=True)
            try:
                instance = sdk.lightrag_class(
                    working_dir=str(directory),
                    workspace=config.workspace,
                    llm_model_func=build_completion(config.extract_llm),
                    role_llm_configs={
                        "keyword": sdk.role_config_class(
                            func=build_completion(config.keyword_llm),
                            timeout=_seconds_as_int(config.keyword_llm.timeout_seconds),
                        )
                    },
                    embedding_func=sdk.embedding_func_class(
                        embedding_dim=config.embedding.dimension,
                        func=build_embedding(config.embedding, self._embedding_timeout),
                    ),
                    llm_model_name=config.extract_llm.model,
                    default_llm_timeout=_seconds_as_int(config.extract_llm.timeout_seconds),
                    addon_params={"language": config.language},
                    llm_model_max_async=self._llm_max_async,
                    embedding_func_max_async=self._embedding_max_async,
                )
                await instance.initialize_storages()
            except EngineUnavailableError:
                raise
            except Exception as error:
                raise EngineUnavailableError("LightRAG workspace 初始化失败") from error
            self._instances[config.workspace] = instance
            LOGGER.info("lightrag workspace ready workspace=%s", config.workspace)

    async def insert_document(self, workspace: str, document: DocumentInput) -> InsertResult:
        """Insert pre-split chunks through ``ainsert_custom_chunks``.

        Args:
            workspace: LightRAG workspace name.
            document: Praxis document with already-split chunks.

        Returns:
            Accepted/skipped counts, best-effort graph counts and the
            Praxis-to-LightRAG chunk id mapping.

        Raises:
            NotFoundError: The workspace is not bound in this process.
            EngineUnavailableError: LightRAG or the upstream LLM failed.
        """

        instance = self._require_instance(workspace)
        chunk_texts: list[str] = []
        praxis_chunk_ids: list[str] = []
        skipped = 0
        for chunk in document.chunks:
            text = self._sanitize(chunk.text)
            if not text.strip():
                skipped += 1
                continue
            chunk_texts.append(text)
            praxis_chunk_ids.append(chunk.chunk_id)
        if not chunk_texts:
            return InsertResult(accepted=0, skipped=skipped, entities=0, relations=0, chunk_keys={})

        chunk_keys = [self._chunk_key(document.document_id, text) for text in chunk_texts]
        full_text = "\n\n".join(chunk_texts)
        try:
            await instance.ainsert_custom_chunks(
                full_text=full_text,
                text_chunks=chunk_texts,
                doc_id=document.document_id,
            )
        except EngineUnavailableError:
            raise
        except Exception as error:
            raise EngineUnavailableError("LightRAG 写入失败") from error

        entities, relations = await self._document_counts(instance, document.document_id)
        mapping: dict[str, str] = {}
        for praxis_chunk_id, chunk_key in zip(praxis_chunk_ids, chunk_keys):
            mapping.setdefault(praxis_chunk_id, chunk_key)
        return InsertResult(
            accepted=len(chunk_texts),
            skipped=skipped,
            entities=entities,
            relations=relations,
            chunk_keys=mapping,
        )

    async def delete_document(self, workspace: str, document_id: str) -> bool:
        """Delete one document through ``adelete_by_doc_id``.

        Args:
            workspace: LightRAG workspace name.
            document_id: Praxis/LightRAG document id.

        Returns:
            ``True`` when LightRAG removed the document, ``False`` when it was
            not present.

        Raises:
            NotFoundError: The workspace is not bound in this process.
            EngineUnavailableError: The deletion was rejected or failed.
        """

        instance = self._require_instance(workspace)
        try:
            result = await instance.adelete_by_doc_id(document_id)
        except EngineUnavailableError:
            raise
        except Exception as error:
            raise EngineUnavailableError("LightRAG 删除文档失败") from error
        status = result.get("status") if isinstance(result, dict) else getattr(result, "status", None)
        if status == "success":
            return True
        if status == "not_found":
            return False
        raise EngineUnavailableError("LightRAG 删除文档失败")

    async def query(
        self,
        workspace: str,
        query: str,
        mode: str,
        top_k: int,
        document_ids: tuple[str, ...],
    ) -> EngineQueryResult:
        """Run a context-only retrieval through ``aquery_data``.

        ``aquery_data`` sets ``only_need_context=True`` internally and stops
        before LLM generation, so no answer text can ever enter the response.
        When a document filter is requested the adapter over-fetches chunks
        because the filter can only be applied after Praxis mapping.

        Args:
            workspace: LightRAG workspace name.
            query: Query text; never logged.
            mode: One of :data:`~app.engine.SUPPORTED_MODES`.
            top_k: Requested number of contexts.
            document_ids: Optional Praxis document restriction.

        Returns:
            Parsed context-only result.

        Raises:
            InputError: LightRAG rejected the query text as unusable.
            NotFoundError: The workspace is not bound in this process.
            EngineUnavailableError: The retrieval failed upstream.
            ProcessingError: The upstream payload shape was not understood.
        """

        instance = self._require_instance(workspace)
        if mode not in SUPPORTED_MODES:
            raise InputError("不支持的检索模式")
        sdk = self._load_sdk()
        chunk_top_k = top_k if not document_ids else min(max(top_k * 4, top_k), 200)
        param = sdk.query_param_class(
            mode=mode,
            only_need_context=True,
            top_k=top_k,
            chunk_top_k=chunk_top_k,
        )
        try:
            payload = await instance.aquery_data(query, param=param)
        except Exception as error:
            if is_query_validation_error(error):
                raise InputError("查询文本不符合 LightRAG 的检索要求") from error
            raise EngineUnavailableError("LightRAG 查询失败") from error
        return parse_query_payload(payload, self._graph_field_sep())

    async def clear_workspace(self, workspace: str) -> None:
        """Finalize and delete the LightRAG storage of one workspace.

        Args:
            workspace: LightRAG workspace name.

        Returns:
            ``None``; the next :meth:`ensure_workspace` creates a fresh store.

        Raises:
            EngineUnavailableError: The storage directory could not be removed.
        """

        instance = self._instances.pop(workspace, None)
        self._init_locks.pop(workspace, None)
        if instance is not None:
            try:
                await instance.finalize_storages()
            except Exception:
                # Finalization is best effort: the directory is removed next and
                # a failure here must not block the rebuild.
                pass
        await self._remove_tree(self._workspace_dir(workspace))
        LOGGER.info("lightrag workspace cleared workspace=%s", workspace)

    def _load_sdk(self) -> _SdkApi:
        """Import and cache the pinned LightRAG entry points.

        Returns:
            Resolved SDK API.

        Raises:
            EngineUnavailableError: ``lightrag-hku`` cannot be imported.
        """

        if self._sdk is not None:
            return self._sdk
        try:
            from lightrag import LightRAG, QueryParam, RoleLLMConfig
            from lightrag.utils import EmbeddingFunc
        except Exception as error:
            raise EngineUnavailableError("lightrag-hku 未正确安装：无法导入 lightrag") from error
        self._sdk = _SdkApi(
            lightrag_class=LightRAG,
            query_param_class=QueryParam,
            role_config_class=RoleLLMConfig,
            embedding_func_class=EmbeddingFunc,
        )
        return self._sdk

    def _workspace_dir(self, workspace: str) -> Path:
        """Return the per-workspace LightRAG storage root.

        Args:
            workspace: LightRAG workspace name (validated at the HTTP boundary).

        Returns:
            ``<WORKING_DIR>/workspaces/<workspace>/lightrag``.
        """

        return self._working_dir / "workspaces" / workspace / "lightrag"

    def _require_instance(self, workspace: str) -> Any:
        """Return the cached LightRAG instance or raise a 404.

        Args:
            workspace: LightRAG workspace name.

        Returns:
            The initialized LightRAG object.

        Raises:
            NotFoundError: The workspace was never bound in this process.
        """

        instance = self._instances.get(workspace)
        if instance is None:
            raise NotFoundError("workspace 未在当前进程绑定，请先调用 POST /v1/workspaces")
        return instance

    async def _remove_tree(self, path: Path) -> None:
        """Delete a workspace directory, tolerating Windows handle lag.

        Args:
            path: Directory to remove.

        Returns:
            ``None``.

        Raises:
            EngineUnavailableError: The directory still exists after retries.
        """

        if not path.exists():
            return
        import shutil

        for attempt in range(3):
            shutil.rmtree(path, ignore_errors=True)
            if not path.exists():
                return
            await asyncio.sleep(0.1 * (attempt + 1))
        raise EngineUnavailableError("无法清空 LightRAG workspace 存储")

    def _sanitize(self, text: str) -> str:
        """Apply LightRAG's text sanitizer when the helper is available.

        The sanitizer changes control characters, and chunk ids are derived from
        the sanitized text, so the adapter must apply the same transformation
        before computing a key.

        Args:
            text: Chunk text from Praxis.

        Returns:
            Text exactly as LightRAG will persist it.
        """

        if self._sanitize_func is None:
            try:
                from lightrag.utils import sanitize_text_for_encoding

                self._sanitize_func = sanitize_text_for_encoding
            except Exception:
                self._sanitize_func = _identity
        try:
            return self._sanitize_func(text)
        except Exception:
            return text

    def _chunk_key(self, document_id: str, text: str) -> str:
        """Derive the upstream chunk row id for one chunk.

        Args:
            document_id: LightRAG document key.
            text: Sanitized chunk text.

        Returns:
            Chunk id produced by the upstream helper, or the documented local
            fallback when the helper is unavailable.
        """

        if self._chunk_key_func is None:
            try:
                from lightrag.utils_pipeline import make_custom_chunk_id

                self._chunk_key_func = make_custom_chunk_id
            except Exception:
                self._chunk_key_func = custom_chunk_key
        try:
            return self._chunk_key_func(document_id, text)
        except Exception:
            return custom_chunk_key(document_id, text)

    def _graph_field_sep(self) -> str:
        """Return LightRAG's chunk-id separator.

        Returns:
            ``"<SEP>"`` from ``lightrag.constants`` when importable, otherwise
            the documented fallback.
        """

        if self._graph_sep is None:
            try:
                from lightrag.constants import GRAPH_FIELD_SEP

                self._graph_sep = str(GRAPH_FIELD_SEP)
            except Exception:
                self._graph_sep = GRAPH_FIELD_SEP_FALLBACK
        return self._graph_sep

    async def _document_counts(self, instance: Any, document_id: str) -> tuple[int, int]:
        """Read the entity/relation counts LightRAG recorded for a document.

        The upstream custom-chunk API returns ``None``; the per-document
        ``full_entities``/``full_relations`` rows carry the counts instead. Any
        failure degrades to zero rather than failing the ingest, because the
        graph itself was written successfully.

        Args:
            instance: LightRAG object.
            document_id: Document key.

        Returns:
            ``(entities, relations)`` counts.
        """

        try:
            entity_row = await instance.full_entities.get_by_id(document_id) or {}
            relation_row = await instance.full_relations.get_by_id(document_id) or {}
        except Exception:
            return 0, 0
        return _row_count(entity_row, "entity_names"), _row_count(relation_row, "relation_pairs")


def _identity(text: str) -> str:
    """Return the input unchanged (sanitizer fallback).

    Args:
        text: Arbitrary text.

    Returns:
        The same text.
    """

    return text


def _seconds_as_int(value: float) -> int:
    """Round a positive timeout to the integer seconds LightRAG expects.

    Args:
        value: Timeout in seconds.

    Returns:
        At least one second.
    """

    return max(1, int(round(value)))


def _row_count(row: Any, key: str) -> int:
    """Read a count from a LightRAG per-document row.

    Args:
        row: ``full_entities``/``full_relations`` row.
        key: List field used as a fallback when ``count`` is absent.

    Returns:
        Non-negative count.
    """

    if not isinstance(row, dict):
        return 0
    count = row.get("count")
    if isinstance(count, int) and count >= 0:
        return count
    values = row.get(key)
    return len(values) if isinstance(values, (list, tuple)) else 0
