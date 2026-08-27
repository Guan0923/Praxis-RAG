"""Bounded lazy FlagEmbedding Cross Encoder reranking."""

from __future__ import annotations

import math
import os
import sys
from collections.abc import Callable, Sequence
from dataclasses import replace
from numbers import Real
from threading import Lock
from typing import Any

from ..contracts import Candidate, RagOperationError

DEFAULT_RERANKER_MODEL = "BAAI/bge-reranker-base"


class CrossEncoderReranker:
    """Score bounded query/passage pairs with one lazily loaded model."""

    def __init__(
        self,
        model_name: str = DEFAULT_RERANKER_MODEL,
        *,
        device: str | None = None,
        batch_size: int = 4,
        max_candidates: int = 30,
        max_length: int = 512,
        max_text_chars: int = 4_000,
        model_factory: Callable[..., object] | None = None,
    ) -> None:
        if not model_name.strip():
            raise ValueError("Reranker model name must not be empty")
        if not 1 <= batch_size <= 64:
            raise ValueError("Reranker batch size must be between 1 and 64")
        if not 1 <= max_candidates <= 100:
            raise ValueError("Reranker max candidates must be between 1 and 100")
        if not 64 <= max_length <= 8_192:
            raise ValueError("Reranker max length must be between 64 and 8192")
        if not 256 <= max_text_chars <= 100_000:
            raise ValueError("Reranker max text characters must be between 256 and 100000")
        self.model_name = model_name.strip()
        self.device = device
        self.batch_size = batch_size
        self.max_candidates = max_candidates
        self.max_length = max_length
        self.max_text_chars = max_text_chars
        self._model_factory = model_factory
        self._model: object | None = None
        self._load_lock = Lock()
        self._inference_lock = Lock()

    def warmup(self) -> None:
        """Load weights without running model inference."""

        self._model_instance()

    @property
    def is_loaded(self) -> bool:
        """Report process-local load state without triggering a model download."""

        return self._model is not None

    def rerank(self, query: str, candidates: list[Candidate]) -> list[Candidate]:
        """Replace retrieval scores for a bounded prefix and preserve the tail."""

        if not candidates:
            return []
        selected = candidates[: self.max_candidates]
        pairs = [(query, _passage(candidate, max_chars=self.max_text_chars)) for candidate in selected]
        try:
            with self._inference_lock:
                raw_scores = self._model_instance().compute_score(  # type: ignore[attr-defined]
                    pairs,
                    batch_size=self.batch_size,
                    max_length=self.max_length,
                    normalize=True,
                )
            scores = _scores(raw_scores, expected=len(selected))
        except RagOperationError:
            raise
        except Exception as exc:
            raise RagOperationError(f"Cross-encoder reranking failed: {exc}") from exc

        rescored = [
            replace(
                candidate,
                score=score,
                metadata={**candidate.metadata, "retrieval_score": round(candidate.score, 6)},
            )
            for candidate, score in zip(selected, scores, strict=True)
        ]
        rescored.sort(key=lambda item: (-item.score, item.document_id, item.chunk_id))
        return [*rescored, *candidates[self.max_candidates :]]

    def _model_instance(self) -> object:
        if self._model is not None:
            return self._model
        with self._load_lock:
            if self._model is None:
                _trace(f"reranker model loading: {self.model_name}")
                factory = self._model_factory or _flag_reranker_factory
                self._model = factory(
                    self.model_name,
                    device=self.device,
                    batch_size=self.batch_size,
                    max_length=self.max_length,
                )
                _trace("reranker model ready")
        return self._model


def _flag_reranker_factory(
    model_name: str,
    *,
    device: str | None,
    batch_size: int,
    max_length: int,
) -> object:
    try:
        from FlagEmbedding import FlagReranker
    except ImportError as exc:
        raise RagOperationError("FlagEmbedding is required for cross-encoder reranking") from exc
    return FlagReranker(
        model_name,
        use_fp16=False,
        devices=device,
        batch_size=batch_size,
        max_length=max_length,
    )


def _passage(candidate: Candidate, *, max_chars: int) -> str:
    title = candidate.title.strip()
    combined = f"{title}\n{candidate.text}" if title else candidate.text
    return combined[:max_chars]


def _scores(value: Any, *, expected: int) -> list[float]:
    if hasattr(value, "tolist"):
        value = value.tolist()
    if isinstance(value, Real) and not isinstance(value, bool):
        values: Sequence[object] = [value]
    elif isinstance(value, Sequence) and not isinstance(value, str | bytes | bytearray):
        values = value
    else:
        raise RagOperationError("Cross-encoder returned invalid scores")
    if len(values) != expected:
        raise RagOperationError("Cross-encoder score count does not match candidate count")
    scores: list[float] = []
    for item in values:
        if not isinstance(item, Real) or isinstance(item, bool) or not math.isfinite(float(item)):
            raise RagOperationError("Cross-encoder returned a non-finite score")
        scores.append(float(item))
    return scores


def _trace(message: str) -> None:
    if os.getenv("RAG_DEBUG_TRACE", "").strip().lower() in {"1", "true", "yes"}:
        print(f"[rag-mcp] {message}", file=sys.stderr, flush=True)
