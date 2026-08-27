"""Small deterministic knowledge fixture used by the phase-1 retriever."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class KnowledgeChunk:
    """One source chunk in the temporary in-memory knowledge base."""

    document_id: str
    chunk_id: str
    title: str
    source: str
    text: str
    metadata: dict[str, object]


DEFAULT_KNOWLEDGE: tuple[KnowledgeChunk, ...] = (
    KnowledgeChunk(
        document_id="doc-rrf",
        chunk_id="doc-rrf:1",
        title="RRF 排名融合",
        source="knowledge/rrf.md",
        text=(
            "RRF（Reciprocal Rank Fusion，倒数排名融合）用于合并多组检索排名。"
            "它主要依赖文档在每组结果中的名次，而不是要求不同检索器的原始分数处于同一尺度。"
            "Hybrid RAG 可以使用 RRF 融合 BM25 稀疏检索和 Dense 向量检索。"
        ),
        metadata={"topic": "retrieval", "phase": "mock"},
    ),
    KnowledgeChunk(
        document_id="doc-agentic-rag",
        chunk_id="doc-agentic-rag:1",
        title="Agentic RAG 职责边界",
        source="knowledge/agentic-rag.md",
        text=(
            "Agentic RAG 中，Agent 决定是否检索、如何改写查询以及是否再次检索。"
            "RAG 服务只负责执行一次检索动作，包括召回候选、重排和返回证据。"
            "检索次数属于 Agent Runtime 的循环，不应该成为 search_times 参数。"
        ),
        metadata={"topic": "agent", "phase": "mock"},
    ),
    KnowledgeChunk(
        document_id="doc-qdrant",
        chunk_id="doc-qdrant:1",
        title="Qdrant Hybrid Collection",
        source="knowledge/qdrant.md",
        text=(
            "Qdrant collection 可以为同一个 Point 保存命名 Dense Vector 和 Sparse Vector。"
            "BGE-M3 Dense Vector 的维度是 1024；BM25 可以表示为 Sparse Vector。"
            "在线查询时可以分别召回 dense 与 sparse 候选，再使用 RRF 进行融合。"
        ),
        metadata={"topic": "storage", "phase": "mock"},
    ),
)
