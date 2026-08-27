"""Deterministic phase-1 retrieval and reranking implementations."""

from __future__ import annotations

import re

from .contracts import Candidate
from .knowledge import DEFAULT_KNOWLEDGE, KnowledgeChunk

_LATIN_TERM = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]*")
_CJK_RUN = re.compile(r"[\u3400-\u9fff]+")


class MockRetriever:
    """Search a fixed knowledge fixture while preserving the final Retriever API."""

    def __init__(self, chunks: tuple[KnowledgeChunk, ...] = DEFAULT_KNOWLEDGE) -> None:
        self._chunks = chunks

    def search(
        self,
        query: str,
        *,
        limit: int,
        filters: dict[str, str | int | float | bool],
    ) -> list[Candidate]:
        query_terms = _terms(query)
        candidates: list[Candidate] = []
        for chunk in self._chunks:
            if not _matches_filters(chunk, filters):
                continue
            title_terms = _terms(chunk.title)
            body_terms = _terms(chunk.text)
            title_matches = query_terms & title_terms
            body_matches = query_terms & body_terms
            matched = title_matches | body_matches
            if not matched:
                continue
            denominator = max(1, len(query_terms))
            score = (len(body_matches) + 1.5 * len(title_matches)) / denominator
            candidates.append(
                Candidate(
                    document_id=chunk.document_id,
                    chunk_id=chunk.chunk_id,
                    title=chunk.title,
                    source=chunk.source,
                    text=chunk.text,
                    score=score,
                    metadata=dict(chunk.metadata),
                    matched_terms=tuple(sorted(matched)),
                )
            )
        candidates.sort(key=lambda item: (-item.score, item.document_id, item.chunk_id))
        return candidates[:limit]


class MockReranker:
    """Keep deterministic ordering while exercising the pluggable reranker seam."""

    def __init__(self, name: str) -> None:
        self.name = name

    def rerank(self, query: str, candidates: list[Candidate]) -> list[Candidate]:
        del query
        return sorted(candidates, key=lambda item: (-item.score, item.document_id, item.chunk_id))


def _terms(value: str) -> set[str]:
    normalized = value.casefold()
    terms = {match.group(0) for match in _LATIN_TERM.finditer(normalized)}
    for match in _CJK_RUN.finditer(normalized):
        run = match.group(0)
        if len(run) == 1:
            terms.add(run)
        else:
            terms.update(run[index : index + 2] for index in range(len(run) - 1))
    return terms


def _matches_filters(chunk: KnowledgeChunk, filters: dict[str, str | int | float | bool]) -> bool:
    values: dict[str, object] = {
        "document_id": chunk.document_id,
        "source": chunk.source,
        "title": chunk.title,
    }
    return all(values.get(key) == value for key, value in filters.items())
