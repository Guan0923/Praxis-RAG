"""Engine port shared by the service layer and the two engine implementations.

The sidecar must be testable without ``lightrag-hku`` installed, so the graph
engine is an explicit abstract base class: the production adapter
(:mod:`app.lightrag_engine`) imports LightRAG lazily inside its methods, while
the test suite injects an in-memory double through :func:`app.main.create_app`.

The port speaks only in Praxis terms (documents, chunks, contexts). LightRAG's
own objects never cross this boundary, which keeps the HTTP layer and the
replacement semantics independent of upstream releases.

Assumption to verify on upgrade: the pinned LightRAG release derives a custom
chunk row id as ``chunk-<md5(f"{len(doc_id)}:{doc_id}:{chunk_text}")>`` over
sanitized text (``lightrag.utils_pipeline.make_custom_chunk_id``). The adapter
prefers the upstream helper and falls back to :func:`custom_chunk_key`; the same
derivation is used by the fake engine so the mapping exercised in tests matches
production behaviour.
"""

from __future__ import annotations

import hashlib
from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import Mapping

from .config import WorkspaceConfig

#: Retrieval modes accepted by ``POST /v1/workspaces/{namespace}/query``.
SUPPORTED_MODES: tuple[str, ...] = ("local", "global", "hybrid", "naive", "mix")


def custom_chunk_key(document_id: str, text: str) -> str:
    """Derive the LightRAG chunk row id for a Praxis chunk.

    This mirrors ``lightrag.utils_pipeline.make_custom_chunk_id`` in the pinned
    release. The adapter calls the upstream helper when it is importable; this
    function is the documented fallback and the derivation used by the in-memory
    test double.

    Args:
        document_id: LightRAG document key (the Praxis ``document_id``).
        text: Chunk text exactly as handed to LightRAG.

    Returns:
        ``chunk-<md5 hex>`` of the length-prefixed ``doc_id`` and text.
    """

    payload = f"{len(document_id)}:{document_id}:{text}"
    return "chunk-" + hashlib.md5(payload.encode("utf-8", errors="replace")).hexdigest()


def chunk_content_hash(text: str) -> str:
    """Hash chunk text for the registry's best-effort secondary lookup.

    Args:
        text: Chunk text returned by the engine.

    Returns:
        Lowercase MD5 hex digest of the text.
    """

    return hashlib.md5(text.encode("utf-8", errors="replace")).hexdigest()


@dataclass(frozen=True)
class ChunkInput:
    """One Praxis chunk handed to the engine.

    Attributes:
        chunk_id: Praxis chunk identifier.
        text: Exact chunk text.
        page: Source page, if any.
        heading: Section heading path.
        upload_version: Praxis upload version.
    """

    chunk_id: str
    text: str
    page: int | None
    heading: str
    upload_version: int | None


@dataclass(frozen=True)
class DocumentInput:
    """One Praxis document, already split into chunks.

    Attributes:
        document_id: Praxis document id, also used as the LightRAG doc id.
        document_name: Original file name for evidence rendering.
        chunks: Ordered chunks of this upload version.
    """

    document_id: str
    document_name: str
    chunks: tuple[ChunkInput, ...]


@dataclass(frozen=True)
class InsertResult:
    """Outcome of one document ingestion.

    Attributes:
        accepted: Number of chunks handed to LightRAG.
        skipped: Number of chunks skipped because their text was blank.
        entities: Entity rows extracted for the document (best effort).
        relations: Relationship rows extracted for the document (best effort).
        chunk_keys: Mapping ``praxis chunk_id -> lightrag chunk id``.
    """

    accepted: int
    skipped: int
    entities: int
    relations: int
    chunk_keys: Mapping[str, str]


@dataclass(frozen=True)
class EngineChunk:
    """A retrieved text chunk as reported by the engine.

    Attributes:
        chunk_id: LightRAG chunk row id.
        text: Chunk text.
        score: Optional upstream similarity; the sidecar falls back to a rank
            score when the pinned release returns no per-chunk score.
    """

    chunk_id: str
    text: str
    score: float | None = None


@dataclass(frozen=True)
class EngineEntity:
    """An entity row and the chunks that mention it.

    Attributes:
        name: Entity name.
        chunk_ids: LightRAG chunk ids referenced by the entity's ``source_id``.
    """

    name: str
    chunk_ids: tuple[str, ...] = ()


@dataclass(frozen=True)
class EngineRelation:
    """A relationship row and the chunks that mention it.

    Attributes:
        source: Source entity name.
        target: Target entity name.
        description: Relationship description.
        chunk_ids: LightRAG chunk ids referenced by the relationship's
            ``source_id``.
    """

    source: str
    target: str
    description: str
    chunk_ids: tuple[str, ...] = ()


@dataclass(frozen=True)
class EngineQueryResult:
    """Context-only retrieval result. There is deliberately no answer field.

    Attributes:
        chunks: Retrieved chunks in upstream relevance order.
        entities: Entity rows considered for the query.
        relations: Relationship rows considered for the query.
    """

    chunks: tuple[EngineChunk, ...] = ()
    entities: tuple[EngineEntity, ...] = ()
    relations: tuple[EngineRelation, ...] = ()


class GraphEngine(ABC):
    """Abstract graph/retrieval engine used by :class:`app.service.GraphService`.

    Implementations own workspace storage and the mapping to LightRAG. They are
    intentionally small: six operations cover the whole HTTP contract.
    """

    @abstractmethod
    async def ensure_workspace(self, config: WorkspaceConfig) -> None:
        """Create the engine binding for a workspace if it does not exist yet.

        Args:
            config: Credential-bearing binding built from the workspace request.
                Implementations must not persist or log the credentials.

        Returns:
            ``None``.
        """

    @abstractmethod
    async def insert_document(self, workspace: str, document: DocumentInput) -> InsertResult:
        """Insert (or patch) one document's pre-split chunks.

        Args:
            workspace: LightRAG workspace name.
            document: Praxis document with already-split chunks.

        Returns:
            Counts and the Praxis-to-LightRAG chunk id mapping.
        """

    @abstractmethod
    async def delete_document(self, workspace: str, document_id: str) -> bool:
        """Delete one document and its derived graph data.

        Args:
            workspace: LightRAG workspace name.
            document_id: Praxis/LightRAG document id.

        Returns:
            ``True`` when something was removed, ``False`` when it was absent.
        """

    @abstractmethod
    async def query(
        self,
        workspace: str,
        query: str,
        mode: str,
        top_k: int,
        document_ids: tuple[str, ...],
    ) -> EngineQueryResult:
        """Retrieve context only; implementations must never request an answer.

        Args:
            workspace: LightRAG workspace name.
            query: Query text (never logged by implementations).
            mode: One of :data:`SUPPORTED_MODES`.
            top_k: Retrieval width.
            document_ids: Optional document restriction. Implementations may
                over-fetch, the service filters after mapping chunk ids.

        Returns:
            Context-only result.
        """

    @abstractmethod
    async def clear_workspace(self, workspace: str) -> None:
        """Remove all engine storage of one workspace.

        Args:
            workspace: LightRAG workspace name.

        Returns:
            ``None``.
        """

    def installed_version(self) -> str | None:
        """Return the installed upstream package version, or ``None``.

        Returns:
            Distribution version string when the package is installed.
        """

        return None

    def availability_error(self) -> str | None:
        """Return a safe deployment problem for ``/healthz``, or ``None``.

        Returns:
            Human-readable, secret-free description of a missing dependency.
        """

        return None
