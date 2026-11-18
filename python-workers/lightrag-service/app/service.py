"""Service orchestration: HTTP contract semantics on top of the engine port.

The service owns everything the engine must not know about:

* workspace lifecycle (create, idempotent re-bind, fingerprint-triggered rebuild);
* the Praxis metadata registry that turns LightRAG chunk ids back into
  ``document_id`` / ``document_name`` / ``page`` / ``heading``;
* replacement semantics for repeated ingestion of one document;
* context shaping and the document filter.

The engine is an injected :class:`app.engine.GraphEngine`, so the same service is
exercised by the test double without importing ``lightrag-hku``.
"""

from __future__ import annotations

from .engine import (
    ChunkInput,
    DocumentInput,
    EngineQueryResult,
    GraphEngine,
    InsertResult,
    chunk_content_hash,
)
from .errors import NotFoundError
from .models import ChunksRequest, QueryRequest, WorkspaceRequest
from .registry import ChunkRef, ChunkRegistry, DocumentMeta, utc_now
from .workspaces import WorkspaceState, WorkspaceStore

#: Returned in a context when the caller-supplied page number is unknown.
DEFAULT_PAGE = 0
#: Maximum number of contexts one query may return.
MAX_TOP_K = 100


class GraphService:
    """Implement the HTTP contract using an injected graph engine.

    Args:
        engine: Graph engine implementation (real adapter or test double).
        registry: Praxis chunk metadata registry.
        store: Namespace binding store.
    """

    def __init__(self, engine: GraphEngine, registry: ChunkRegistry, store: WorkspaceStore) -> None:
        """Store the collaborators; the service is stateless apart from them."""

        self._engine = engine
        self._registry = registry
        self._store = store

    def workspace_count(self) -> int:
        """Count namespaces that have a persisted workspace binding.

        Returns:
            Number of known namespaces, used by ``/healthz``.
        """

        return len(self._store.namespaces())

    async def ensure_workspace(self, request: WorkspaceRequest) -> dict:
        """Create, re-bind or rebuild the workspace of a namespace.

        Semantics required by the Go backend:

        * first call: engine binding created, ``created=true``;
        * same ``config_fingerprint``: binding reused, ``created=false`` and
          ``rebuilt=false`` (no data touched);
        * different fingerprint: engine storage and chunk metadata wiped, then
          re-created, ``created=false`` and ``rebuilt=true``.

        Args:
            request: Validated ``POST /v1/workspaces`` body.

        Returns:
            JSON-ready response dictionary.
        """

        config = request.to_config()
        namespace = request.namespace
        state = self._store.load(namespace)
        now = utc_now()

        if state is None:
            await self._engine.ensure_workspace(config)
            self._store.save(
                WorkspaceState(
                    namespace=namespace,
                    workspace=config.workspace,
                    config_fingerprint=request.config_fingerprint,
                    language=request.language,
                    created_at=now,
                    updated_at=now,
                )
            )
            return {
                "namespace": namespace,
                "workspace": config.workspace,
                "created": True,
                "rebuilt": False,
            }

        if state.config_fingerprint != request.config_fingerprint:
            await self._engine.clear_workspace(config.workspace)
            self._registry.clear(namespace)
            await self._engine.ensure_workspace(config)
            self._store.save(
                WorkspaceState(
                    namespace=namespace,
                    workspace=config.workspace,
                    config_fingerprint=request.config_fingerprint,
                    language=request.language,
                    created_at=now,
                    updated_at=now,
                )
            )
            return {
                "namespace": namespace,
                "workspace": config.workspace,
                "created": False,
                "rebuilt": True,
            }

        # A matching fingerprint after a restart still needs a live engine
        # binding in this process; ensure_workspace is idempotent when the
        # instance already exists.
        await self._engine.ensure_workspace(config)
        return {
            "namespace": namespace,
            "workspace": state.workspace,
            "created": False,
            "rebuilt": False,
        }

    async def insert_chunks(self, namespace: str, request: ChunksRequest) -> dict:
        """Ingest one document's pre-split chunks, replacing a previous upload.

        A repeated ``document_id`` is delete-then-insert. LightRAG's custom-chunk
        API patches an existing processed document instead of replacing it, so
        the service always deletes first; otherwise a re-ingest would leave
        chunks from the previous upload version behind.

        Args:
            namespace: Praxis namespace.
            request: Validated chunks body.

        Returns:
            JSON-ready response with accepted/skipped chunk and graph counts.
        """

        state = self._require_state(namespace)
        document = DocumentInput(
            document_id=request.document_id,
            document_name=request.document_name,
            chunks=tuple(
                ChunkInput(
                    chunk_id=chunk.chunk_id,
                    text=chunk.text,
                    page=chunk.page,
                    heading=chunk.heading,
                    upload_version=chunk.upload_version,
                )
                for chunk in request.chunks
            ),
        )
        await self._engine.delete_document(state.workspace, document.document_id)
        result = await self._engine.insert_document(state.workspace, document)
        self._registry.remember(namespace, self._document_meta(document, result))
        return {
            "document_id": document.document_id,
            "accepted": result.accepted,
            "skipped": result.skipped,
            "entities": result.entities,
            "relations": result.relations,
        }

    async def delete_document(self, namespace: str, document_id: str) -> dict:
        """Delete one document and its Praxis metadata.

        Args:
            namespace: Praxis namespace.
            document_id: Praxis document id.

        Returns:
            ``{"document_id": ..., "deleted": bool}``. ``deleted`` is ``false``
            when neither the engine nor the registry knew the document, which
            keeps repeated deletes idempotent.
        """

        state = self._require_state(namespace)
        existed = self._registry.document(namespace, document_id) is not None
        removed = await self._engine.delete_document(state.workspace, document_id)
        self._registry.remove_document(namespace, document_id)
        return {"document_id": document_id, "deleted": bool(existed or removed)}

    async def query(self, namespace: str, request: QueryRequest) -> dict:
        """Retrieve context only and map it back to Praxis evidence metadata.

        No generated answer is ever requested from the engine; ``aquery_data``
        in the production adapter stops before LLM generation.

        Args:
            namespace: Praxis namespace.
            request: Validated query body.

        Returns:
            ``{"contexts": [...], "debug": {...}}`` where every context carries
            ``chunk_id``, ``document_id``, ``document_name``, ``page``,
            ``heading``, ``text``, ``score``, ``entities`` and ``relations``.
        """

        state = self._require_state(namespace)
        requested_documents = tuple(dict.fromkeys(request.filters.document_ids))
        result = await self._engine.query(
            state.workspace,
            request.query,
            request.mode,
            request.top_k,
            requested_documents,
        )
        return self._build_query_response(namespace, request, result, requested_documents)

    async def clear_workspace(self, namespace: str) -> dict:
        """Delete every document, graph row and binding of a namespace.

        Args:
            namespace: Praxis namespace.

        Returns:
            ``{"namespace": ..., "cleared": true}``.
        """

        state = self._require_state(namespace)
        await self._engine.clear_workspace(state.workspace)
        self._registry.clear(namespace)
        self._store.delete(namespace)
        return {"namespace": namespace, "cleared": True}

    def _document_meta(self, document: DocumentInput, result: InsertResult) -> DocumentMeta:
        """Build the registry row for a freshly ingested document.

        Args:
            document: Document as sent to the engine.
            result: Engine insertion result carrying the chunk-id mapping.

        Returns:
            Metadata containing one :class:`~app.registry.ChunkRef` per chunk
            that the engine actually accepted.
        """

        chunks: list[ChunkRef] = []
        for chunk in document.chunks:
            lightrag_chunk_id = result.chunk_keys.get(chunk.chunk_id)
            if not lightrag_chunk_id:
                continue
            chunks.append(
                ChunkRef(
                    chunk_id=chunk.chunk_id,
                    document_id=document.document_id,
                    document_name=document.document_name,
                    page=chunk.page,
                    heading=chunk.heading,
                    upload_version=chunk.upload_version,
                    lightrag_chunk_id=lightrag_chunk_id,
                    content_hash=chunk_content_hash(chunk.text),
                )
            )
        return DocumentMeta(
            document_id=document.document_id,
            document_name=document.document_name,
            chunks=tuple(chunks),
            updated_at=utc_now(),
        )

    def _build_query_response(
        self,
        namespace: str,
        request: QueryRequest,
        result: EngineQueryResult,
        requested_documents: tuple[str, ...],
    ) -> dict:
        """Map engine rows to the HTTP context contract.

        Args:
            namespace: Praxis namespace.
            request: Validated query body.
            result: Context-only engine result.
            requested_documents: ``filters.document_ids`` after de-duplication.

        Returns:
            Response dictionary with ``contexts`` and ``debug`` only.
        """

        entities_by_chunk: dict[str, list[str]] = {}
        for entity in result.entities:
            for lightrag_chunk_id in entity.chunk_ids:
                names = entities_by_chunk.setdefault(lightrag_chunk_id, [])
                if entity.name not in names:
                    names.append(entity.name)

        relations_by_chunk: dict[str, list[dict[str, str]]] = {}
        for relation in result.relations:
            entry = {
                "source": relation.source,
                "target": relation.target,
                "description": relation.description,
            }
            for lightrag_chunk_id in relation.chunk_ids:
                bucket = relations_by_chunk.setdefault(lightrag_chunk_id, [])
                if entry not in bucket:
                    bucket.append(entry)

        contexts: list[dict] = []
        seen_chunks: set[str] = set()
        allowed_documents = set(requested_documents)
        for rank, chunk in enumerate(result.chunks):
            if len(contexts) >= min(request.top_k, MAX_TOP_K):
                break
            ref = self._registry.find_by_lightrag_chunk(namespace, chunk.chunk_id)
            if ref is None:
                # Best effort when an upstream release changes chunk id
                # derivation: fall back to the content hash of the returned text.
                ref = self._registry.find_by_content_hash(namespace, chunk_content_hash(chunk.text))
            if ref is None or ref.chunk_id in seen_chunks:
                continue
            if allowed_documents and ref.document_id not in allowed_documents:
                continue
            seen_chunks.add(ref.chunk_id)
            contexts.append(
                {
                    "chunk_id": ref.chunk_id,
                    "document_id": ref.document_id,
                    "document_name": ref.document_name,
                    "page": ref.page if ref.page is not None else DEFAULT_PAGE,
                    "heading": ref.heading,
                    "text": chunk.text,
                    "score": _context_score(chunk.score, rank),
                    "entities": list(entities_by_chunk.get(chunk.chunk_id, []))
                    if request.include_entities
                    else [],
                    "relations": list(relations_by_chunk.get(chunk.chunk_id, []))
                    if request.include_entities
                    else [],
                }
            )

        return {
            "contexts": contexts,
            "debug": {
                "mode": request.mode,
                "entities": len(result.entities),
                "relations": len(result.relations),
                "chunks": len(contexts),
            },
        }

    def _require_state(self, namespace: str) -> WorkspaceState:
        """Return the binding of a namespace or raise a 404.

        Args:
            namespace: Praxis namespace.

        Returns:
            The persisted workspace state.

        Raises:
            NotFoundError: The namespace has no binding yet.
        """

        state = self._store.load(namespace)
        if state is None:
            raise NotFoundError("namespace 尚未创建，请先调用 POST /v1/workspaces")
        return state


def _context_score(upstream: float | None, rank: int) -> float:
    """Compute the score reported for one context.

    The pinned LightRAG release does not return a per-chunk similarity from
    ``aquery_data``, so a deterministic rank score is used instead: ``1/(1+rank)``.
    If a future release starts returning ``score``/``similarity``/``distance``,
    the adapter fills ``EngineChunk.score`` and that value wins.

    Args:
        upstream: Optional engine-provided similarity.
        rank: Zero-based position in the retrieval result.

    Returns:
        Score clamped to ``[0.0, 1.0]`` and rounded to four decimals.
    """

    if upstream is not None:
        try:
            return round(min(1.0, max(0.0, float(upstream))), 4)
        except (TypeError, ValueError):
            pass
    return round(1.0 / (1.0 + max(rank, 0)), 4)
