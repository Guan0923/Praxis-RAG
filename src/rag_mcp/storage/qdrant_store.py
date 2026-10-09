"""Qdrant persistence for Dense, BM25 Sparse, and RRF Hybrid retrieval."""

from __future__ import annotations

import uuid
from collections.abc import Callable, Sequence
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
        asset_payloads: Sequence[dict[str, object]] = (),
        *,
        after_write: Callable[[], None] | None = None,
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
        old_points = []
        offset = None
        while True:
            page, offset = self._client.scroll(
                collection_name=self.collection,
                scroll_filter=_payload_filter(self._scoped({"document_id": document_id})),
                offset=offset, limit=256, with_payload=True, with_vectors=True,
            )
            old_points.extend(page)
            if offset is None:
                break
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
                        "record_type": "chunk",
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
        for payload in asset_payloads:
            if payload.get("document_id") != document_id or not payload.get("asset_id"):
                raise ValueError("Asset payload must belong to the replaced document")
            points.append(models.PointStruct(
                id=self._point_id(str(payload["asset_id"])), vector={},
                payload={**payload, **({"namespace": self.namespace} if self.namespace else {})},
            ))
        try:
            self._client.upsert(collection_name=self.collection, points=points, wait=True)
            keep_ids = {str(point.id) for point in points}
            stale = [point.id for point in old_points if str(point.id) not in keep_ids]
            if stale:
                self._client.delete(collection_name=self.collection,
                                    points_selector=models.PointIdsList(points=stale), wait=True)
            if after_write is not None:
                after_write()
        except Exception:
            self._restore_document(old_points, [point.id for point in points])
            raise
        return len(chunks)

    def reembed_into(self, collection: str, embedder, expected_chunks: dict[str, int]) -> None:
        """Copy this namespace into an unpublished index, replacing only dense vectors."""
        from collections import Counter

        target = QdrantStore(collection=collection, vector_size=embedder.connection.vector_size,
                             namespace=self.namespace, client=self._client)
        target.ensure_collection()
        copied = Counter()
        offset = None
        if self._client.collection_exists(self.collection):
            while True:
                points, offset = self._client.scroll(
                    collection_name=self.collection, scroll_filter=_payload_filter(self._scoped({})),
                    offset=offset, limit=64, with_payload=True, with_vectors=True,
                )
                chunks = [point for point in points if (point.payload or {}).get("record_type") == "chunk"]
                vectors = embedder.embed_documents([point.payload["text"] for point in chunks])
                dense = {str(point.id): vector for point, vector in zip(chunks, vectors, strict=True)}
                replacements = []
                for point in points:
                    vector = dict(point.vector or {})
                    if str(point.id) in dense:
                        vector[DENSE_VECTOR_NAME] = dense[str(point.id)]
                        copied[point.payload["document_id"]] += 1
                    replacements.append(models.PointStruct(id=point.id, vector=vector, payload=point.payload))
                if replacements:
                    self._client.upsert(collection_name=collection, points=replacements, wait=True)
                if offset is None:
                    break
        if dict(copied) != expected_chunks:
            from ..contracts import RagOperationError
            raise RagOperationError("文档清单与索引片段数量不一致，未切换模型；请检查现有索引")

    def get_assets(self, asset_ids: Sequence[str]) -> dict[str, dict[str, object]]:
        if not asset_ids:
            return {}
        self.ensure_collection()
        points = self._client.retrieve(
            collection_name=self.collection, ids=[self._point_id(value) for value in dict.fromkeys(asset_ids)],
            with_payload=True, with_vectors=False,
        )
        result = {}
        for point in points:
            payload = dict(point.payload or {})
            if payload.get("record_type") != "asset" or payload.get("namespace") != self.namespace:
                continue
            result[str(payload.get("asset_id"))] = payload
        return result

    def _point_id(self, item_id: str) -> str:
        return str(uuid.uuid5(_POINT_NAMESPACE, f"{self.namespace or ''}\0{item_id}"))

    def _restore_document(self, old_points: Sequence[Any], new_ids: Sequence[Any]) -> None:
        old_ids = {str(point.id) for point in old_points}
        introduced = [point_id for point_id in new_ids if str(point_id) not in old_ids]
        try:
            if old_points:
                self._client.upsert(collection_name=self.collection, points=[
                    models.PointStruct(id=point.id, vector=point.vector or {}, payload=dict(point.payload or {}))
                    for point in old_points
                ], wait=True)
            if introduced:
                self._client.delete(collection_name=self.collection,
                                    points_selector=models.PointIdsList(points=introduced), wait=True)
        except Exception as exc:
            raise RuntimeError("Qdrant replacement failed and the previous index could not be restored") from exc

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
