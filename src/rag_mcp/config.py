"""Environment-backed configuration for the RAG MCP implementation."""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

from .rerankers.cross_encoder import DEFAULT_RERANKER_MODEL

BackendName = Literal["mock", "qdrant"]
DENSE_VECTOR_SIZE = 1024


class RagConfigError(ValueError):
    """Raised when deployment configuration is incomplete or inconsistent."""


@dataclass(frozen=True)
class RagConfig:
    """Validated service settings kept outside the Agent-visible tool schema."""

    backend: BackendName = "mock"
    qdrant_url: str | None = None
    qdrant_path: Path | None = None
    collection: str = "mini_agent_knowledge"
    embedding_model: str = "BAAI/bge-m3"
    embedding_device: str | None = None
    vector_size: int = DENSE_VECTOR_SIZE
    preload_model: bool = False
    namespace: str | None = None
    reranker_model: str = DEFAULT_RERANKER_MODEL
    reranker_device: str | None = None
    reranker_batch_size: int = 4
    reranker_max_candidates: int = 30
    reranker_max_length: int = 512
    reranker_max_text_chars: int = 4_000
    preload_reranker: bool = False

    @classmethod
    def from_env(cls) -> RagConfig:
        """Load configuration without connecting to Qdrant or loading a model."""

        raw_backend = os.getenv("RAG_BACKEND", "mock").strip().lower()
        if raw_backend not in {"mock", "qdrant"}:
            raise RagConfigError("RAG_BACKEND must be mock or qdrant")

        qdrant_url = _optional_env("QDRANT_URL")
        raw_path = _optional_env("QDRANT_PATH")
        if qdrant_url and raw_path:
            raise RagConfigError("Set only one of QDRANT_URL and QDRANT_PATH")
        qdrant_path = Path(raw_path).expanduser().resolve() if raw_path else None
        if raw_backend == "qdrant" and not qdrant_url and not qdrant_path:
            qdrant_url = "http://localhost:6333"

        collection = os.getenv("RAG_COLLECTION", "mini_agent_knowledge").strip()
        if not collection:
            raise RagConfigError("RAG_COLLECTION must not be empty")

        raw_size = os.getenv("RAG_DENSE_VECTOR_SIZE", str(DENSE_VECTOR_SIZE)).strip()
        try:
            vector_size = int(raw_size)
        except ValueError as exc:
            raise RagConfigError("RAG_DENSE_VECTOR_SIZE must be an integer") from exc
        if vector_size != DENSE_VECTOR_SIZE:
            raise RagConfigError(f"BGE-M3 dense vectors must have {DENSE_VECTOR_SIZE} dimensions")

        return cls(
            backend=raw_backend,  # type: ignore[arg-type]
            qdrant_url=qdrant_url,
            qdrant_path=qdrant_path,
            collection=collection,
            embedding_model=os.getenv("RAG_EMBEDDING_MODEL", "BAAI/bge-m3").strip() or "BAAI/bge-m3",
            embedding_device=_optional_env("RAG_EMBEDDING_DEVICE"),
            vector_size=vector_size,
            preload_model=_boolean_env("RAG_PRELOAD_MODEL", default=False),
            namespace=_optional_env("RAG_NAMESPACE"),
            reranker_model=os.getenv("RAG_RERANKER_MODEL", DEFAULT_RERANKER_MODEL).strip() or DEFAULT_RERANKER_MODEL,
            reranker_device=_optional_env("RAG_RERANKER_DEVICE") or _optional_env("RAG_EMBEDDING_DEVICE"),
            reranker_batch_size=_integer_env("RAG_RERANKER_BATCH_SIZE", 4, minimum=1, maximum=64),
            reranker_max_candidates=_integer_env("RAG_RERANKER_MAX_CANDIDATES", 30, minimum=1, maximum=100),
            reranker_max_length=_integer_env("RAG_RERANKER_MAX_LENGTH", 512, minimum=64, maximum=8_192),
            reranker_max_text_chars=_integer_env("RAG_RERANKER_MAX_TEXT_CHARS", 4_000, minimum=256, maximum=100_000),
            preload_reranker=_boolean_env("RAG_PRELOAD_RERANKER", default=False),
        )


def _optional_env(name: str) -> str | None:
    value = os.getenv(name)
    return value.strip() if value and value.strip() else None


def _boolean_env(name: str, *, default: bool) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default
    normalized = raw.strip().lower()
    if normalized in {"1", "true", "yes", "on"}:
        return True
    if normalized in {"0", "false", "no", "off"}:
        return False
    raise RagConfigError(f"{name} must be true or false")


def _integer_env(name: str, default: int, *, minimum: int, maximum: int) -> int:
    raw = os.getenv(name, str(default)).strip()
    try:
        value = int(raw)
    except ValueError as exc:
        raise RagConfigError(f"{name} must be an integer") from exc
    if not minimum <= value <= maximum:
        raise RagConfigError(f"{name} must be between {minimum} and {maximum}")
    return value
