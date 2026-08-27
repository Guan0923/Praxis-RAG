"""Phase-2 tests for deterministic chunking and Qdrant dense retrieval."""

from __future__ import annotations

from pathlib import Path

import pytest
from qdrant_client import QdrantClient
from rag_mcp.chunking import DocumentChunk, chunk_document
from rag_mcp.config import DENSE_VECTOR_SIZE, RagConfig, RagConfigError
from rag_mcp.embeddings import BgeM3Embedder, EmbeddingError, validate_vectors
from rag_mcp.retrievers.dense import DenseRetriever
from rag_mcp.storage.qdrant_store import QdrantStore


class FakeEmbedder:
    """Return controlled vectors without downloading a model in tests."""

    def __init__(self, query_vector: list[float]) -> None:
        self.query_vector = query_vector

    def embed_documents(self, texts: list[str]) -> list[list[float]]:
        return [_vector(index) for index, _text in enumerate(texts)]

    def embed_query(self, text: str) -> list[float]:
        assert text
        return self.query_vector


def test_chunk_ids_and_overlap_are_stable(tmp_path: Path) -> None:
    document = tmp_path / "guide.md"
    document.write_text("# 标题\n" + "甲" * 1_400, encoding="utf-8")

    first = chunk_document(document, source_root=tmp_path, chunk_size=700, overlap=100)
    second = chunk_document(document, source_root=tmp_path, chunk_size=700, overlap=100)

    assert first == second
    assert len(first) == 3
    assert first[0].document_id == first[1].document_id
    assert first[0].chunk_id != first[1].chunk_id
    assert first[0].text[-100:] == first[1].text[:100]
    assert first[0].source == "guide.md"


def test_dense_vector_dimension_is_strict() -> None:
    validate_vectors([_vector(0)], expected_count=1, vector_size=DENSE_VECTOR_SIZE)

    with pytest.raises(EmbeddingError, match="dimensions"):
        validate_vectors([[0.0] * 10], expected_count=1, vector_size=DENSE_VECTOR_SIZE)


def test_embedding_load_state_is_read_without_loading_model() -> None:
    assert BgeM3Embedder(device="cpu").is_loaded is False


def test_qdrant_import_is_idempotent_and_dense_filter_is_applied() -> None:
    client = QdrantClient(":memory:")
    store = QdrantStore(collection="test_dense", client=client)
    chunks = [
        _chunk("doc-a", "chunk-a", "A 文档", "a.md"),
        _chunk("doc-a", "chunk-b", "A 文档第二段", "a.md"),
    ]
    vectors = [_vector(0), _vector(1)]

    assert store.replace_document(chunks, vectors) == 2
    assert store.replace_document(chunks, vectors) == 2
    assert client.count("test_dense", exact=True).count == 2

    store.replace_document([_chunk("doc-b", "chunk-c", "B 文档", "b.md")], [_vector(0)])
    points = store.search(_vector(0), limit=10, filters={"document_id": "doc-b"})

    assert len(points) == 1
    assert points[0].payload["chunk_id"] == "chunk-c"


def test_dense_retriever_projects_citable_evidence() -> None:
    client = QdrantClient(":memory:")
    store = QdrantStore(collection="test_projection", client=client)
    store.replace_document([_chunk("doc-a", "chunk-a", "可引用证据", "knowledge/a.md")], [_vector(0)])

    results = DenseRetriever(FakeEmbedder(_vector(0)), store).search("证据", limit=5, filters={})

    assert results[0].document_id == "doc-a"
    assert results[0].chunk_id == "chunk-a"
    assert results[0].source == "knowledge/a.md"
    assert results[0].text == "可引用证据"


def test_qdrant_namespace_prevents_cross_user_search_and_delete() -> None:
    client = QdrantClient(":memory:")
    first = QdrantStore(collection="test_namespace", client=client, namespace="user-a")
    second = QdrantStore(collection="test_namespace", client=client, namespace="user-b")
    shared = _chunk("doc-shared", "chunk-shared", "first user", "shared.md")

    first.replace_document([shared], [_vector(0)])
    second.replace_document(
        [_chunk("doc-shared", "chunk-shared", "second user", "shared.md")],
        [_vector(0)],
    )

    assert client.count("test_namespace", exact=True).count == 2
    assert first.search(_vector(0), limit=5, filters={})[0].payload["text"] == "first user"
    assert second.search(_vector(0), limit=5, filters={})[0].payload["text"] == "second user"

    first.delete_document("doc-shared")

    assert client.count("test_namespace", exact=True).count == 1
    assert second.search(_vector(0), limit=5, filters={})[0].payload["text"] == "second user"


def test_qdrant_config_defaults_to_http_and_rejects_wrong_dimension(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_BACKEND", "qdrant")
    monkeypatch.delenv("QDRANT_URL", raising=False)
    monkeypatch.delenv("QDRANT_PATH", raising=False)

    assert RagConfig.from_env().qdrant_url == "http://localhost:6333"

    monkeypatch.setenv("RAG_DENSE_VECTOR_SIZE", "384")
    with pytest.raises(RagConfigError, match="1024"):
        RagConfig.from_env()


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
