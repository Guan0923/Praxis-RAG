"""Dense retrieval using one Embedder and a Qdrant vector store."""

from __future__ import annotations

from ..contracts import Candidate, RagOperationError
from ..embeddings import Embedder
from ..storage.qdrant_store import QdrantStore
from .common import project_candidates


class DenseRetriever:
    """Convert a query to a vector and project scored Qdrant points."""

    def __init__(self, embedder: Embedder, store: QdrantStore) -> None:
        self._embedder = embedder
        self._store = store

    def search(
        self,
        query: str,
        *,
        limit: int,
        filters: dict[str, str | int | float | bool],
    ) -> list[Candidate]:
        """Return dense candidates while preserving citation payload fields."""

        try:
            vector = self._embedder.embed_query(query)
            points = self._store.dense_search(vector, limit=limit, filters=filters)
        except Exception as exc:
            raise RagOperationError(f"Dense retrieval failed: {exc}") from exc

        return project_candidates(points)


class UnavailableRetriever:
    """Explain a retrieval mode that belongs to a later implementation phase."""

    def __init__(self, message: str) -> None:
        self._message = message

    def search(
        self,
        query: str,
        *,
        limit: int,
        filters: dict[str, str | int | float | bool],
    ) -> list[Candidate]:
        del query, limit, filters
        raise RagOperationError(self._message)
