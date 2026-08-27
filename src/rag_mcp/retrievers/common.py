"""Shared projection from untrusted Qdrant payloads to evidence candidates."""

from __future__ import annotations

from collections.abc import Iterable
from typing import Any

from ..contracts import Candidate, RagOperationError


def project_candidates(points: Iterable[Any]) -> list[Candidate]:
    """Validate citation fields before returning storage results to an Agent."""

    candidates: list[Candidate] = []
    for point in points:
        payload = dict(point.payload or {})
        metadata = payload.get("metadata", {})
        if not isinstance(metadata, dict):
            metadata = {}
        try:
            candidates.append(
                Candidate(
                    document_id=str(payload["document_id"]),
                    chunk_id=str(payload["chunk_id"]),
                    title=str(payload.get("title", "")),
                    source=str(payload["source"]),
                    text=str(payload["text"]),
                    score=float(point.score),
                    metadata=dict(metadata),
                )
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise RagOperationError(f"Qdrant point has an invalid evidence payload: {exc}") from exc
    return candidates
