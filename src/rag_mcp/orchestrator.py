"""Execute one retrieval action without owning the surrounding Agent loop."""

from __future__ import annotations

from collections.abc import Callable, Mapping
from time import perf_counter

from .config import RagConfig
from .contracts import Candidate, Reranker, Retriever, SearchRequest
from .mock import MockReranker, MockRetriever


class RagOrchestrator:
    """Select one Retriever and optional Reranker for a validated request."""

    def __init__(
        self,
        retrievers: Mapping[str, Retriever],
        rerankers: Mapping[str, Reranker],
        *,
        implementation: str,
        limitations: tuple[str, ...] = (),
        warmup: Callable[[], None] | None = None,
        projector: Callable[[list[Candidate]], list[dict[str, object]]] | None = None,
    ) -> None:
        self._retrievers = dict(retrievers)
        self._rerankers = dict(rerankers)
        self._implementation = implementation
        self._limitations = limitations
        self._warmup = warmup
        self._projector = projector

    def warmup(self) -> None:
        """Prepare optional heavyweight dependencies before serving requests."""

        if self._warmup is not None:
            self._warmup()

    def search(self, request: SearchRequest) -> dict[str, object]:
        """Return bounded evidence and debug metadata for one tool observation."""

        retriever = self._retrievers[request.retrieval]
        retrieval_started = perf_counter()
        candidates = retriever.search(
            request.query,
            limit=request.candidate_k,
            filters=request.filters or {},
        )
        retrieval_ms = (perf_counter() - retrieval_started) * 1_000
        candidate_count = len(candidates)
        rerank_ms = 0.0
        if request.rerank != "none":
            rerank_started = perf_counter()
            candidates = self._rerankers[request.rerank].rerank(request.query, candidates)
            rerank_ms = (perf_counter() - rerank_started) * 1_000
        selected = _unique_assets(candidates, request.top_k)
        evidence_status = "found" if selected else "no_evidence"
        results = (
            self._projector(selected)
            if self._projector is not None
            else [candidate.to_result(rank) for rank, candidate in enumerate(selected, start=1)]
        )
        return {
            "query": request.query,
            "retrieval": request.retrieval,
            "rerank": request.rerank,
            "candidate_k": request.candidate_k,
            "top_k": request.top_k,
            "evidence_status": evidence_status,
            "answer_guidance": (
                "Answer from the returned evidence and cite source plus chunk_id."
                if selected
                else "No supporting local evidence was found; do not guess or present unsupported knowledge claims."
            ),
            "results": results,
            "debug": {
                "implementation": self._implementation,
                "candidate_count": candidate_count,
                "returned_count": len(results),
                "retrieval_ms": round(retrieval_ms, 3),
                "rerank_ms": round(rerank_ms, 3),
                "limitations": list(self._limitations),
            },
        }


def _unique_assets(candidates, limit: int):
    selected = []
    seen = set()
    for candidate in candidates:
        asset_id = candidate.metadata.get("asset_id")
        key = ("asset", str(asset_id)) if asset_id else ("chunk", candidate.chunk_id)
        if key in seen:
            continue
        seen.add(key)
        selected.append(candidate)
        if len(selected) == limit:
            break
    return selected


def build_default_orchestrator(config: RagConfig | None = None) -> RagOrchestrator:
    """Build a lazy registry from environment configuration."""

    resolved = config or RagConfig.from_env()
    if resolved.backend == "qdrant":
        return _build_qdrant_orchestrator(resolved)

    retriever = MockRetriever()
    return RagOrchestrator(
        retrievers={"bm25": retriever, "embedding": retriever, "hybrid": retriever},
        rerankers={
            "cross_encoder": MockReranker("cross_encoder"),
        },
        implementation="mock-v1",
        limitations=("All retrieval modes and rerankers are deterministic Phase-1 mocks.",),
    )


def build_qdrant_dependencies(config: RagConfig):
    """Construct Dense, BM25 Sparse, and Qdrant dependencies."""

    from .embeddings import BgeM3Embedder
    from .sparse import Bm25SparseEncoder
    from .storage.qdrant_store import QdrantStore

    embedder = BgeM3Embedder(
        config.embedding_model,
        device=config.embedding_device,
        vector_size=config.vector_size,
    )
    store = QdrantStore(
        collection=config.collection,
        url=config.qdrant_url,
        path=config.qdrant_path,
        vector_size=config.vector_size,
        namespace=config.namespace,
    )
    return embedder, Bm25SparseEncoder(), store


def _build_qdrant_orchestrator(config: RagConfig) -> RagOrchestrator:
    from .rerankers.cross_encoder import CrossEncoderReranker
    from .retrievers.dense import DenseRetriever
    from .retrievers.hybrid import Bm25Retriever, HybridRetriever

    embedder, sparse_encoder, store = build_qdrant_dependencies(config)
    dense = DenseRetriever(embedder, store)
    bm25 = Bm25Retriever(sparse_encoder, store)
    hybrid = HybridRetriever(embedder, sparse_encoder, store)
    cross_encoder = CrossEncoderReranker(
        config.reranker_model,
        device=config.reranker_device,
        batch_size=config.reranker_batch_size,
        max_candidates=config.reranker_max_candidates,
        max_length=config.reranker_max_length,
        max_text_chars=config.reranker_max_text_chars,
    )

    def warmup() -> None:
        embedder.warmup()
        if config.preload_reranker:
            cross_encoder.warmup()

    return RagOrchestrator(
        retrievers={
            "bm25": bm25,
            "embedding": dense,
            "hybrid": hybrid,
        },
        rerankers={
            "cross_encoder": cross_encoder,
        },
        implementation="qdrant-hybrid-rerank-v1",
        warmup=warmup,
    )
