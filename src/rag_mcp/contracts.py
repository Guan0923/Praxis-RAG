"""Provider-neutral contracts shared by the RAG orchestration layers."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Literal, Protocol

RetrievalMode = Literal["bm25", "embedding", "hybrid"]
RerankMode = Literal["none", "cross_encoder", "bi_encoder"]

RETRIEVAL_MODES = frozenset({"bm25", "embedding", "hybrid"})
RERANK_MODES = frozenset({"none", "cross_encoder", "bi_encoder"})
ALLOWED_FILTERS = frozenset({"document_id", "source", "title"})


class SearchValidationError(ValueError):
    """Raised when untrusted MCP arguments violate the search contract."""


class RagOperationError(RuntimeError):
    """Raised when a configured retrieval component cannot complete a request."""


@dataclass(frozen=True)
class Candidate:
    """One retrievable evidence chunk before final response projection."""

    document_id: str
    chunk_id: str
    title: str
    source: str
    text: str
    score: float
    metadata: dict[str, object]
    matched_terms: tuple[str, ...] = ()

    def to_result(self, rank: int) -> dict[str, object]:
        """Return the bounded public representation sent back to the Agent."""

        return {
            "rank": rank,
            "document_id": self.document_id,
            "chunk_id": self.chunk_id,
            "title": self.title,
            "source": self.source,
            "text": self.text,
            "score": round(self.score, 6),
            "metadata": dict(self.metadata),
        }


@dataclass(frozen=True)
class SearchRequest:
    """Validated input for exactly one retrieval action."""

    query: str
    retrieval: RetrievalMode = "hybrid"
    rerank: RerankMode = "cross_encoder"
    candidate_k: int = 10
    top_k: int = 5
    filters: dict[str, str | int | float | bool] | None = None

    @classmethod
    def from_arguments(cls, arguments: dict[str, Any]) -> SearchRequest:
        """Validate MCP input without exposing storage-specific parameters."""

        allowed = {"query", "retrieval", "rerank", "candidate_k", "top_k", "filters"}
        unknown = sorted(set(arguments) - allowed)
        if unknown:
            raise SearchValidationError(f"Unknown search arguments: {', '.join(unknown)}")

        query = arguments.get("query")
        if not isinstance(query, str) or not query.strip():
            raise SearchValidationError("query must be a non-empty string")
        query = query.strip()
        if len(query) > 2_000:
            raise SearchValidationError("query must contain at most 2000 characters")

        retrieval = arguments.get("retrieval", "hybrid")
        if retrieval not in RETRIEVAL_MODES:
            raise SearchValidationError("retrieval must be bm25, embedding, or hybrid")

        rerank = arguments.get("rerank", "cross_encoder")
        if rerank not in RERANK_MODES:
            raise SearchValidationError("rerank must be none, cross_encoder, or bi_encoder")

        candidate_k = _bounded_integer(arguments.get("candidate_k", 10), "candidate_k", minimum=1, maximum=100)
        top_k = _bounded_integer(arguments.get("top_k", 5), "top_k", minimum=1, maximum=10)
        if top_k > candidate_k:
            raise SearchValidationError("top_k must not be greater than candidate_k")

        filters = _filters(arguments.get("filters", {}))
        return cls(
            query=query,
            retrieval=retrieval,
            rerank=rerank,
            candidate_k=candidate_k,
            top_k=top_k,
            filters=filters,
        )


class Retriever(Protocol):
    """Candidate generator selected by retrieval strategy."""

    def search(
        self,
        query: str,
        *,
        limit: int,
        filters: dict[str, str | int | float | bool],
    ) -> list[Candidate]: ...


class Reranker(Protocol):
    """Second-stage relevance scorer selected by rerank strategy."""

    def rerank(self, query: str, candidates: list[Candidate]) -> list[Candidate]: ...


def _bounded_integer(value: object, name: str, *, minimum: int, maximum: int) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or not minimum <= value <= maximum:
        raise SearchValidationError(f"{name} must be an integer between {minimum} and {maximum}")
    return value


def _filters(value: object) -> dict[str, str | int | float | bool]:
    if not isinstance(value, dict):
        raise SearchValidationError("filters must be an object")
    unknown = sorted(set(value) - ALLOWED_FILTERS)
    if unknown:
        raise SearchValidationError(f"Unsupported filters: {', '.join(unknown)}")
    result: dict[str, str | int | float | bool] = {}
    for key, item in value.items():
        if not isinstance(item, str | int | float | bool):
            raise SearchValidationError(f"filters.{key} must be a string, number, or boolean")
        result[str(key)] = item
    return result
