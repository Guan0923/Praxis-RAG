"""Phase-4 tests for bounded lazy Cross Encoder reranking."""

from __future__ import annotations

import pytest
from rag_mcp.config import RagConfig, RagConfigError
from rag_mcp.contracts import Candidate, RagOperationError
from rag_mcp.rerankers.cross_encoder import CrossEncoderReranker


class FakeModel:
    """Record bounded pairs and return controlled normalized scores."""

    def __init__(self, scores: object) -> None:
        self.scores = scores
        self.calls: list[tuple[list[tuple[str, str]], dict[str, object]]] = []

    def compute_score(self, pairs: list[tuple[str, str]], **kwargs: object) -> object:
        self.calls.append((pairs, kwargs))
        return self.scores


def test_cross_encoder_lazily_rescores_sorts_and_preserves_retrieval_score() -> None:
    fake = FakeModel([0.1, 0.9])
    factory_calls: list[dict[str, object]] = []

    def factory(_model_name: str, **kwargs: object) -> object:
        factory_calls.append(kwargs)
        return fake

    reranker = CrossEncoderReranker(
        "test-reranker",
        device="cpu",
        batch_size=2,
        max_candidates=2,
        max_text_chars=256,
        model_factory=factory,
    )
    candidates = [
        _candidate("first", score=0.8, text="第一条"),
        _candidate("second", score=0.2, text="第二条"),
    ]

    assert factory_calls == []
    assert reranker.is_loaded is False
    results = reranker.rerank("测试问题", candidates)

    assert reranker.is_loaded is True
    assert [item.document_id for item in results] == ["second", "first"]
    assert results[0].score == pytest.approx(0.9)
    assert results[0].metadata["retrieval_score"] == 0.2
    assert factory_calls == [{"device": "cpu", "batch_size": 2, "max_length": 512}]
    assert fake.calls[0][1] == {"batch_size": 2, "max_length": 512, "normalize": True}


def test_cross_encoder_bounds_candidate_count_and_passage_characters() -> None:
    fake = FakeModel([0.7])
    reranker = CrossEncoderReranker(
        "test-reranker",
        max_candidates=1,
        max_text_chars=256,
        model_factory=lambda *_args, **_kwargs: fake,
    )
    candidates = [
        _candidate("first", score=0.8, text="甲" * 1_000),
        _candidate("tail", score=0.6, text="不会发送给模型"),
    ]

    results = reranker.rerank("问题", candidates)

    assert [item.document_id for item in results] == ["first", "tail"]
    assert len(fake.calls[0][0]) == 1
    assert len(fake.calls[0][0][0][1]) == 256


@pytest.mark.parametrize("scores", [[], [float("nan")], "invalid"])
def test_cross_encoder_rejects_invalid_model_scores(scores: object) -> None:
    reranker = CrossEncoderReranker(
        "test-reranker",
        model_factory=lambda *_args, **_kwargs: FakeModel(scores),
    )

    with pytest.raises(RagOperationError, match="score"):
        reranker.rerank("问题", [_candidate("doc", score=0.4, text="证据")])


def test_reranker_environment_configuration_is_bounded(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_RERANKER_MODEL", "local/reranker")
    monkeypatch.setenv("RAG_RERANKER_DEVICE", "cpu")
    monkeypatch.setenv("RAG_RERANKER_BATCH_SIZE", "8")
    monkeypatch.setenv("RAG_RERANKER_MAX_CANDIDATES", "20")
    monkeypatch.setenv("RAG_RERANKER_MAX_LENGTH", "768")
    monkeypatch.setenv("RAG_RERANKER_MAX_TEXT_CHARS", "6000")
    monkeypatch.setenv("RAG_PRELOAD_RERANKER", "true")

    config = RagConfig.from_env()

    assert config.reranker_model == "local/reranker"
    assert config.reranker_device == "cpu"
    assert config.reranker_batch_size == 8
    assert config.reranker_max_candidates == 20
    assert config.reranker_max_length == 768
    assert config.reranker_max_text_chars == 6000
    assert config.preload_reranker is True

    monkeypatch.setenv("RAG_RERANKER_BATCH_SIZE", "0")
    with pytest.raises(RagConfigError, match="RAG_RERANKER_BATCH_SIZE"):
        RagConfig.from_env()


def _candidate(document_id: str, *, score: float, text: str) -> Candidate:
    return Candidate(
        document_id=document_id,
        chunk_id=f"chunk-{document_id}",
        title=f"title-{document_id}",
        source=f"{document_id}.md",
        text=text,
        score=score,
        metadata={"kind": "test"},
    )
