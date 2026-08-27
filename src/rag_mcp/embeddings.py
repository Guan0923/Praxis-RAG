"""Lazy BGE-M3 dense embedding adapter with strict dimension checks."""

from __future__ import annotations

import os
import sys
from collections.abc import Sequence
from threading import Lock
from typing import Any, Protocol

from .config import DENSE_VECTOR_SIZE


class EmbeddingError(RuntimeError):
    """Raised when the embedding model cannot produce valid dense vectors."""


class Embedder(Protocol):
    """Minimal embedding port used by ingestion and retrieval."""

    def embed_documents(self, texts: Sequence[str]) -> list[list[float]]: ...

    def embed_query(self, text: str) -> list[float]: ...


class BgeM3Embedder:
    """Load FlagEmbedding's BGE-M3 model only on the first encode request."""

    def __init__(
        self,
        model_name: str = "BAAI/bge-m3",
        *,
        device: str | None = None,
        vector_size: int = DENSE_VECTOR_SIZE,
        batch_size: int = 8,
    ) -> None:
        self.model_name = model_name
        self.device = device
        self.vector_size = vector_size
        self.batch_size = batch_size
        self._model: Any | None = None
        self._lock = Lock()

    def embed_documents(self, texts: Sequence[str]) -> list[list[float]]:
        """Encode document chunks and validate every output vector."""

        if not texts:
            return []
        return self._encode(list(texts))

    def embed_query(self, text: str) -> list[float]:
        """Encode one query using the same dense BGE-M3 representation."""

        if not text.strip():
            raise EmbeddingError("Cannot embed an empty query")
        return self._encode([text])[0]

    def warmup(self) -> None:
        """Load model weights without performing a retrieval request."""

        self._get_model()

    @property
    def is_loaded(self) -> bool:
        """Report process-local load state without triggering a model download."""

        return self._model is not None

    def _encode(self, texts: list[str]) -> list[list[float]]:
        _trace("embedding model requested")
        model = self._get_model()
        _trace("embedding encode started")
        try:
            result = model.encode(
                texts,
                batch_size=self.batch_size,
                return_dense=True,
                return_sparse=False,
                return_colbert_vecs=False,
            )
            raw_vectors = result["dense_vecs"]
            vectors = [[float(value) for value in vector] for vector in raw_vectors]
        except Exception as exc:
            raise EmbeddingError(f"BGE-M3 encoding failed: {exc}") from exc
        _trace("embedding encode completed")
        validate_vectors(vectors, expected_count=len(texts), vector_size=self.vector_size)
        return vectors

    def _get_model(self) -> Any:
        if self._model is not None:
            return self._model
        with self._lock:
            if self._model is None:
                try:
                    _trace("FlagEmbedding import started")
                    from FlagEmbedding import BGEM3FlagModel

                    _trace("FlagEmbedding import completed")
                except ImportError as exc:
                    raise EmbeddingError("BGE-M3 dependencies are missing; run: uv sync --extra dense") from exc
                kwargs: dict[str, object] = {"use_fp16": self.device != "cpu"}
                if self.device:
                    kwargs["devices"] = self.device
                try:
                    _trace("BGE-M3 construction started")
                    self._model = BGEM3FlagModel(self.model_name, **kwargs)
                    _trace("BGE-M3 construction completed")
                except Exception as exc:
                    raise EmbeddingError(f"Could not load BGE-M3 model {self.model_name}: {exc}") from exc
        return self._model


def validate_vectors(vectors: Sequence[Sequence[float]], *, expected_count: int, vector_size: int) -> None:
    """Reject count or dimension mismatches before data reaches Qdrant."""

    if len(vectors) != expected_count:
        raise EmbeddingError(f"Expected {expected_count} vectors, received {len(vectors)}")
    for index, vector in enumerate(vectors):
        if len(vector) != vector_size:
            raise EmbeddingError(f"Vector {index} has {len(vector)} dimensions; expected {vector_size}")


def _trace(message: str) -> None:
    if os.getenv("RAG_DEBUG_TRACE", "").strip().lower() in {"1", "true", "yes"}:
        print(f"[rag-mcp] {message}", file=sys.stderr, flush=True)
