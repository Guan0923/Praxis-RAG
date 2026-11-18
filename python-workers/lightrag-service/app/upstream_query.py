"""Mapping of LightRAG ``aquery_data`` payloads to the engine result.

``aquery_data`` returns a JSON-ish dictionary whose exact keys are an upstream
implementation detail (entity rows use ``entity_name``, relationship rows use
``src_id``/``tgt_id``, chunk rows use ``chunk_id``/``content`` and ``source_id``
joins chunk ids with ``<SEP>``). Keeping the parser in its own module makes that
assumption visible and testable, and keeps the engine adapter focused on
lifecycle and storage.

The parser is deliberately forgiving where it can be (missing optional fields,
list-or-mapping collections, unknown extra keys) and strict where it matters
(a payload that is not a dictionary, or a rich payload with a non-dict ``data``,
raises :class:`~app.errors.ProcessingError` so the HTTP layer returns 500
instead of silently dropping contexts).
"""

from __future__ import annotations

from typing import Any

from .engine import EngineChunk, EngineEntity, EngineQueryResult, EngineRelation
from .errors import ProcessingError

#: Keys a future LightRAG release might use for a per-chunk similarity.
_SCORE_KEYS = ("score", "similarity", "relevance", "rerank_score")


def parse_query_payload(payload: Any, graph_field_sep: str) -> EngineQueryResult:
    """Map an ``aquery_data`` payload to a context-only engine result.

    Args:
        payload: Raw return value of ``LightRAG.aquery_data``.
        graph_field_sep: Separator LightRAG uses inside ``source_id``.

    Returns:
        Context-only result. A ``status="failure"`` payload (no results) maps
        to an empty result, not an error.

    Raises:
        ProcessingError: The payload shape is not a dictionary, or ``data`` is
            present but not a dictionary.
    """

    if not isinstance(payload, dict):
        raise ProcessingError("LightRAG 查询返回格式无法识别")
    if payload.get("status") == "failure":
        return EngineQueryResult()
    data = payload.get("data")
    if data is None:
        return EngineQueryResult()
    if not isinstance(data, dict):
        raise ProcessingError("LightRAG 查询返回格式无法识别")

    chunks: list[EngineChunk] = []
    seen: set[str] = set()
    for raw in data.get("chunks") or []:
        if not isinstance(raw, dict):
            continue
        chunk_id = raw.get("chunk_id") or raw.get("id")
        content = raw.get("content")
        if not isinstance(chunk_id, str) or not chunk_id or not isinstance(content, str):
            continue
        if chunk_id in seen:
            continue
        seen.add(chunk_id)
        chunks.append(EngineChunk(chunk_id=chunk_id, text=content, score=extract_score(raw)))

    entities: list[EngineEntity] = []
    for raw in iter_entries(data.get("entities")):
        name = raw.get("entity_name") or raw.get("name") or raw.get("entity")
        if not isinstance(name, str) or not name:
            continue
        entities.append(
            EngineEntity(name=name, chunk_ids=split_sources(raw.get("source_id"), graph_field_sep))
        )

    relations: list[EngineRelation] = []
    for raw in iter_entries(data.get("relationships") or data.get("relations")):
        source = raw.get("src_id") or raw.get("source") or raw.get("entity1")
        target = raw.get("tgt_id") or raw.get("target") or raw.get("entity2")
        if not isinstance(source, str) or not source or not isinstance(target, str) or not target:
            continue
        description = raw.get("description")
        relations.append(
            EngineRelation(
                source=source,
                target=target,
                description=description if isinstance(description, str) else "",
                chunk_ids=split_sources(raw.get("source_id"), graph_field_sep),
            )
        )

    return EngineQueryResult(
        chunks=tuple(chunks), entities=tuple(entities), relations=tuple(relations)
    )


def split_sources(value: Any, separator: str) -> tuple[str, ...]:
    """Split a LightRAG ``source_id`` field into chunk ids.

    Args:
        value: ``<SEP>``-joined string or an already-split list.
        separator: Separator to use for strings.

    Returns:
        De-duplicated, non-empty chunk ids in first-seen order.
    """

    if isinstance(value, (list, tuple)):
        parts = [str(entry) for entry in value]
    else:
        parts = str(value or "").split(separator)
    seen: set[str] = set()
    chunk_ids: list[str] = []
    for part in parts:
        candidate = part.strip()
        if candidate and candidate not in seen:
            seen.add(candidate)
            chunk_ids.append(candidate)
    return tuple(chunk_ids)


def iter_entries(value: Any) -> list[dict]:
    """Normalize a LightRAG list-or-mapping collection into a list of dicts.

    Args:
        value: ``None``, a list of dictionaries or a mapping of dictionaries.

    Returns:
        Dictionary entries; any other value becomes an empty list.
    """

    if isinstance(value, dict):
        return [entry for entry in value.values() if isinstance(entry, dict)]
    if isinstance(value, list):
        return [entry for entry in value if isinstance(entry, dict)]
    return []


def extract_score(raw: dict) -> float | None:
    """Read an optional per-chunk similarity from an upstream chunk dictionary.

    The pinned 1.5.7 payload has no score, so ``None`` is the normal result and
    the service applies its documented rank score. A future release that adds
    one of :data:`_SCORE_KEYS` (or ``distance``) is picked up automatically.

    Args:
        raw: Chunk dictionary returned by ``aquery_data``.

    Returns:
        Similarity in ``[0, 1]`` when present and numeric; ``None`` otherwise.
    """

    for key in _SCORE_KEYS:
        value = raw.get(key)
        if isinstance(value, (int, float)) and not isinstance(value, bool):
            return min(1.0, max(0.0, float(value)))
    distance = raw.get("distance")
    if isinstance(distance, (int, float)) and not isinstance(distance, bool):
        return 1.0 / (1.0 + max(float(distance), 0.0))
    return None


def is_query_validation_error(error: BaseException) -> bool:
    """Report whether an exception is LightRAG's query-text validation failure.

    Args:
        error: Exception raised by ``aquery_data``.

    Returns:
        ``True`` when the query text itself was rejected (maps to HTTP 400).
    """

    module = type(error).__module__
    name = type(error).__name__
    return module.startswith("lightrag.query_validation") or name in {
        "QueryValidationError",
        "EmptyQueryError",
        "RAGQueryTooShortError",
    }
