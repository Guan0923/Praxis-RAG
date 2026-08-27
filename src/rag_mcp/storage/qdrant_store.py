"""Qdrant persistence for Dense, BM25 Sparse, and RRF Hybrid retrieval."""

from __future__ import annotations

import uuid
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from qdrant_client import QdrantClient, models

from ..chunking import DocumentChunk
from ..config import DENSE_VECTOR_SIZE
from ..sparse import SparseEmbedding

DENSE_VECTOR_NAME = "dense"
SPARSE_VECTOR_NAME = "sparse"
_POINT_NAMESPACE = uuid.UUID("f8534b63-112c-4edc-9707-c2824aa53c73")


class QdrantStore:
    """Own dual-vector collection setup, idempotent writes, and scoped queries."""

    def __init__(
        self,
        *,
        collection: str,
        url: str | None = None,
        path: Path | None = None,
        vector_size: int = DENSE_VECTOR_SIZE,
        namespace: str | None = None,
        client: Any | None = None,
    ) -> None:
        if url and path:
            raise ValueError("Set only one of url and path")
        self.collection = collection
        self.vector_size = vector_size
        self.namespace = namespace.strip() if namespace else None
        self._client = client or QdrantClient(url=url, path=str(path) if path else None)

    def ensure_collection(self) -> None:
        """Create or migrate the collection to the Phase-3 dual-vector schema."""

        if self._client.collection_exists(self.collection):
            info = self._client.get_collection(self.collection)
            configured = info.config.params.sparse_vectors or {}
            if SPARSE_VECTOR_NAME not in configured:
                raise ValueError(
                    f"Qdrant collection {self.collection!r} uses the Phase-2 dense-only schema; "
                    "configure a new collection and reindex the preserved source documents"
                )
            return
        self._client.create_collection(
            collection_name=self.collection,
            vectors_config={
                DENSE_VECTOR_NAME: models.VectorParams(size=self.vector_size, distance=models.Distance.COSINE)
            },
            sparse_vectors_config={SPARSE_VECTOR_NAME: _sparse_vector_params()},
        )

    def replace_document(
        self,
        chunks: Sequence[DocumentChunk],
        vectors: Sequence[Sequence[float]],
        sparse_vectors: Sequence[SparseEmbedding] | None = None,
    ) -> int:
        """Replace one document with dense and optional sparse vectors."""

        if not chunks:
            return 0
        if len(chunks) != len(vectors):
            raise ValueError("Chunk and vector counts must match")
        if sparse_vectors is not None and len(chunks) != len(sparse_vectors):
            raise ValueError("Chunk and sparse vector counts must match")
        document_ids = {chunk.document_id for chunk in chunks}
        if len(document_ids) != 1:
            raise ValueError("replace_document accepts chunks from exactly one document")
        for vector in vectors:
            if len(vector) != self.vector_size:
                raise ValueError(f"Dense vector must have {self.vector_size} dimensions")

        self.ensure_collection()
        document_id = next(iter(document_ids))
        self._client.delete(
            collection_name=self.collection,
            points_selector=models.FilterSelector(filter=_payload_filter(self._scoped({"document_id": document_id}))),
            wait=True,
        )
        sparse_items: Sequence[SparseEmbedding | None]
        sparse_items = sparse_vectors if sparse_vectors is not None else [None] * len(chunks)
        points = []
        for chunk, vector, sparse in zip(chunks, vectors, sparse_items, strict=True):
            point_vectors: dict[str, Any] = {DENSE_VECTOR_NAME: list(vector)}
            if sparse is not None and sparse.indices:
                point_vectors[SPARSE_VECTOR_NAME] = _qdrant_sparse(sparse)
            points.append(
                models.PointStruct(
                    id=str(uuid.uuid5(_POINT_NAMESPACE, f"{self.namespace or ''}\0{chunk.chunk_id}")),
                    vector=point_vectors,
                    payload={
                        "document_id": chunk.document_id,
                        "chunk_id": chunk.chunk_id,
                        "text": chunk.text,
                        "title": chunk.title,
                        "source": chunk.source,
                        "metadata": dict(chunk.metadata),
                        **({"namespace": self.namespace} if self.namespace else {}),
                    },
                )
            )
        self._client.upsert(collection_name=self.collection, points=points, wait=True)
        return len(points)

    def delete_document(self, document_id: str) -> None:
        """Delete every vector chunk belonging to one stable document id."""

        if not document_id.strip():
            raise ValueError("document_id must not be empty")
        self.ensure_collection()
        self._client.delete(
            collection_name=self.collection,
            points_selector=models.FilterSelector(filter=_payload_filter(self._scoped({"document_id": document_id}))),
            wait=True,
        )

    def dense_search(
        self,
        vector: Sequence[float],
        *,
        limit: int,
        filters: dict[str, str | int | float | bool],
    ) -> list[Any]:
        """Query the dense named vector and return Qdrant scored points."""

        if len(vector) != self.vector_size:
            raise ValueError(f"Dense query vector must have {self.vector_size} dimensions")
        self.ensure_collection()
        response = self._client.query_points(
            collection_name=self.collection,
            query=list(vector),
            using=DENSE_VECTOR_NAME,
            query_filter=_payload_filter(self._scoped(filters)) if filters or self.namespace else None,
            limit=limit,
            with_payload=True,
        )
        return list(response.points)

    def search(
        self,
        vector: Sequence[float],
        *,
        limit: int,
        filters: dict[str, str | int | float | bool],
    ) -> list[Any]:
        """Compatibility alias for the Phase-2 dense search contract."""

        return self.dense_search(vector, limit=limit, filters=filters)

    def sparse_search(
        self,
        vector: SparseEmbedding,
        *,
        limit: int,
        filters: dict[str, str | int | float | bool],
    ) -> list[Any]:
        """Query the BM25 sparse named vector with the same payload boundary."""

        if not vector.indices:
            return []
        self.ensure_collection()
        response = self._client.query_points(
            collection_name=self.collection,
            query=_qdrant_sparse(vector),
            using=SPARSE_VECTOR_NAME,
            query_filter=_payload_filter(self._scoped(filters)) if filters or self.namespace else None,
            limit=limit,
            with_payload=True,
        )
        return list(response.points)

    def hybrid_search(
        self,
        dense: Sequence[float],
        sparse: SparseEmbedding,
        *,
        limit: int,
        filters: dict[str, str | int | float | bool],
    ) -> list[Any]:
        """Prefetch Dense and BM25 candidates and fuse ranks with Qdrant RRF."""

        if len(dense) != self.vector_size:
            raise ValueError(f"Dense query vector must have {self.vector_size} dimensions")
        if not sparse.indices:
            return self.dense_search(dense, limit=limit, filters=filters)
        self.ensure_collection()
        query_filter = _payload_filter(self._scoped(filters)) if filters or self.namespace else None
        response = self._client.query_points(
            collection_name=self.collection,
            prefetch=[
                models.Prefetch(
                    query=list(dense),
                    using=DENSE_VECTOR_NAME,
                    filter=query_filter,
                    limit=limit,
                ),
                models.Prefetch(
                    query=_qdrant_sparse(sparse),
                    using=SPARSE_VECTOR_NAME,
                    filter=query_filter,
                    limit=limit,
                ),
            ],
            query=models.FusionQuery(fusion=models.Fusion.RRF),
            limit=limit,
            with_payload=True,
        )
        return list(response.points)

    def _scoped(
        self,
        values: dict[str, str | int | float | bool],
    ) -> dict[str, str | int | float | bool]:
        scoped = dict(values)
        if self.namespace:
            scoped["namespace"] = self.namespace
        return scoped


def _payload_filter(values: dict[str, str | int | float | bool]) -> models.Filter:
    return models.Filter(
        must=[
            models.FieldCondition(key=key, match=models.MatchValue(value=value))
            for key, value in sorted(values.items())
        ]
    )


def _sparse_vector_params() -> models.SparseVectorParams:
    return models.SparseVectorParams(
        index=models.SparseIndexParams(on_disk=False),
        modifier=models.Modifier.IDF,
    )


def _qdrant_sparse(vector: SparseEmbedding) -> models.SparseVector:
    return models.SparseVector(indices=list(vector.indices), values=list(vector.values))
