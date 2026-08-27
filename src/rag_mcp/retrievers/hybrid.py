"""Qdrant BM25 sparse and server-side RRF hybrid retrieval."""

from __future__ import annotations

from ..contracts import Candidate, RagOperationError
from ..embeddings import Embedder
from ..sparse import Bm25SparseEncoder
from ..storage.qdrant_store import QdrantStore
from .common import project_candidates


class Bm25Retriever:
    """Encode a lexical query and search Qdrant's sparse named vector."""

    def __init__(self, encoder: Bm25SparseEncoder, store: QdrantStore) -> None:
        self._encoder = encoder
        self._store = store

    def search(
        self,
        query: str,
        *,
        limit: int,
        filters: dict[str, str | int | float | bool],
    ) -> list[Candidate]:
        try:
            vector = self._encoder.encode_query(query)
            if not vector.indices:
                return []
            return project_candidates(self._store.sparse_search(vector, limit=limit, filters=filters))
        except RagOperationError:
            raise
        except Exception as exc:
            raise RagOperationError(f"BM25 retrieval failed: {exc}") from exc


class HybridRetriever:
    """Fuse Dense and BM25 candidate ranks inside Qdrant with RRF."""

    def __init__(self, embedder: Embedder, encoder: Bm25SparseEncoder, store: QdrantStore) -> None:
        self._embedder = embedder
        self._encoder = encoder
        self._store = store

    def search(
        self,
        query: str,
        *,
        limit: int,
        filters: dict[str, str | int | float | bool],
    ) -> list[Candidate]:
        try:
            dense = self._embedder.embed_query(query)
            sparse = self._encoder.encode_query(query)
            if not sparse.indices:
                return project_candidates(self._store.dense_search(dense, limit=limit, filters=filters))
            points = self._store.hybrid_search(dense, sparse, limit=limit, filters=filters)
            return project_candidates(points)
        except RagOperationError:
            raise
        except Exception as exc:
            raise RagOperationError(f"Hybrid retrieval failed: {exc}") from exc
