"""Phase-3 tests for BM25 sparse vectors and Qdrant RRF fusion."""

from __future__ import annotations

from pathlib import Path

import pytest
from qdrant_client import QdrantClient, models
from rag_mcp.chunking import DocumentChunk
from rag_mcp.config import DENSE_VECTOR_SIZE
from rag_mcp.retrievers.hybrid import Bm25Retriever, HybridRetriever
from rag_mcp.sparse import Bm25SparseEncoder, tokenize
from rag_mcp.storage.qdrant_store import DENSE_VECTOR_NAME, QdrantStore


class FakeEmbedder:
    """Return one controlled semantic query vector without model downloads."""

    def __init__(self, query_vector: list[float]) -> None:
        self.query_vector = query_vector

    def embed_documents(self, texts: list[str]) -> list[list[float]]:
        return [_vector(index) for index, _text in enumerate(texts)]

    def embed_query(self, text: str) -> list[float]:
        assert text
        return self.query_vector


def test_multilingual_tokenization_keeps_codes_and_cjk_bigrams() -> None:
    tokens = tokenize("星河 X7 出现 E03，水路供水不足")

    assert {"星", "星河", "x7", "e03", "供水", "不足"}.issubset(tokens)


def test_sparse_encoding_is_stable_sorted_and_positive() -> None:
    encoder = Bm25SparseEncoder()

    first = encoder.encode_document("E03 E03 水路供水不足")
    second = encoder.encode_document("E03 E03 水路供水不足")

    assert first == second
    assert first.indices == tuple(sorted(first.indices))
    assert len(first.indices) == len(set(first.indices))
    assert all(value > 0 for value in first.values)


def test_existing_dense_collection_requires_safe_reindex_migration() -> None:
    client = QdrantClient(":memory:")
    client.create_collection(
        "test_migration",
        vectors_config={
            DENSE_VECTOR_NAME: models.VectorParams(size=DENSE_VECTOR_SIZE, distance=models.Distance.COSINE)
        },
    )

    with pytest.raises(ValueError, match="dense-only schema"):
        QdrantStore(collection="test_migration", client=client).ensure_collection()


def test_bm25_retriever_prioritizes_an_exact_error_code() -> None:
    client = QdrantClient(":memory:")
    store = QdrantStore(collection="test_bm25", client=client)
    encoder = Bm25SparseEncoder()
    chunks = [
        _chunk("doc-e03", "chunk-e03", "错误代码 E03 表示水路供水不足", "e03.md"),
        _chunk("doc-temp", "chunk-temp", "咖啡温度偏低时先预热杯子", "temperature.md"),
    ]
    sparse = encoder.encode_documents([chunk.text for chunk in chunks])
    for index, (chunk, sparse_vector) in enumerate(zip(chunks, sparse, strict=True)):
        store.replace_document([chunk], [_vector(index)], [sparse_vector])

    results = Bm25Retriever(encoder, store).search("E03 怎么处理", limit=5, filters={})

    assert results
    assert results[0].document_id == "doc-e03"
    assert results[0].source == "e03.md"


def test_hybrid_rrf_keeps_semantic_and_exact_code_candidates() -> None:
    client = QdrantClient(":memory:")
    store = QdrantStore(collection="test_hybrid", client=client)
    encoder = Bm25SparseEncoder()
    chunks = [
        _chunk("doc-semantic", "chunk-semantic", "供水异常时检查水箱阀门", "semantic.md"),
        _chunk("doc-code", "chunk-code", "E03 的处理步骤是排出管路空气", "code.md"),
        _chunk("doc-noise", "chunk-noise", "滤芯建议每两个月更换", "noise.md"),
    ]
    sparse = encoder.encode_documents([chunk.text for chunk in chunks])
    for index, (chunk, sparse_vector) in enumerate(zip(chunks, sparse, strict=True)):
        store.replace_document([chunk], [_vector(index)], [sparse_vector])

    results = HybridRetriever(FakeEmbedder(_vector(0)), encoder, store).search(
        "E03",
        limit=2,
        filters={},
    )

    assert {item.document_id for item in results} == {"doc-semantic", "doc-code"}


def test_sparse_and_hybrid_queries_preserve_namespace_isolation() -> None:
    client = QdrantClient(":memory:")
    encoder = Bm25SparseEncoder()
    first = QdrantStore(collection="test_sparse_namespace", client=client, namespace="user-a")
    second = QdrantStore(collection="test_sparse_namespace", client=client, namespace="user-b")
    first_chunk = _chunk("doc-shared", "chunk-shared", "E03 first user", "shared.md")
    second_chunk = _chunk("doc-shared", "chunk-shared", "E03 second user", "shared.md")
    first.replace_document([first_chunk], [_vector(0)], encoder.encode_documents([first_chunk.text]))
    second.replace_document([second_chunk], [_vector(0)], encoder.encode_documents([second_chunk.text]))

    sparse_results = Bm25Retriever(encoder, first).search("E03", limit=5, filters={})
    hybrid_results = HybridRetriever(FakeEmbedder(_vector(0)), encoder, second).search(
        "E03",
        limit=5,
        filters={},
    )

    assert [item.text for item in sparse_results] == ["E03 first user"]
    assert [item.text for item in hybrid_results] == ["E03 second user"]


def _vector(axis: int) -> list[float]:
    vector = [0.0] * DENSE_VECTOR_SIZE
    vector[axis] = 1.0
    return vector


def _chunk(document_id: str, chunk_id: str, text: str, source: str) -> DocumentChunk:
    return DocumentChunk(
        document_id=document_id,
        chunk_id=chunk_id,
        text=text,
        title=Path(source).stem,
        source=source,
        metadata={"kind": "test"},
    )
