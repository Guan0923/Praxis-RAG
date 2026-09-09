"""Focused tests for the phase-1 RAG action contract."""

from __future__ import annotations

import json

import pytest
from rag_mcp.contracts import SearchRequest, SearchValidationError
from rag_mcp.mock import MockReranker
from rag_mcp.orchestrator import RagOrchestrator, build_default_orchestrator
from rag_mcp.server import invoke_search, search_tool_definition


def test_default_search_returns_citable_rrf_evidence() -> None:
    request = SearchRequest.from_arguments({"query": "RRF 怎么融合 BM25 和 Dense？"})

    result = build_default_orchestrator().search(request)

    assert result["retrieval"] == "hybrid"
    assert result["rerank"] == "cross_encoder"
    assert result["debug"]["implementation"] == "mock-v1"
    assert request.candidate_k == 10
    first = result["results"][0]
    assert first["document_id"] == "doc-rrf"
    assert first["chunk_id"] == "doc-rrf:1"
    assert first["source"] == "knowledge/rrf.md"


def test_search_filter_is_applied_before_ranking() -> None:
    request = SearchRequest.from_arguments(
        {
            "query": "Agent Qdrant RRF",
            "filters": {"document_id": "doc-qdrant"},
        }
    )

    result = build_default_orchestrator().search(request)

    assert [item["document_id"] for item in result["results"]] == ["doc-qdrant"]


@pytest.mark.parametrize(
    ("arguments", "message"),
    [
        ({}, "query"),
        ({"query": "x", "top_k": 6, "candidate_k": 5}, "top_k"),
        ({"query": "x", "retrieval": "sql"}, "retrieval"),
        ({"query": "x", "rerank": "bi_encoder"}, "rerank"),
        ({"query": "x", "filters": {"raw_qdrant": "unsafe"}}, "Unsupported filters"),
        ({"query": "x", "unknown": True}, "Unknown search arguments"),
    ],
)
def test_invalid_search_arguments_fail_at_the_mcp_boundary(arguments: dict[str, object], message: str) -> None:
    with pytest.raises(SearchValidationError, match=message):
        SearchRequest.from_arguments(arguments)


def test_mcp_definition_and_result_keep_the_public_contract() -> None:
    definition = search_tool_definition()
    response = invoke_search(build_default_orchestrator(), {"query": "Agentic RAG"})

    assert definition.name == "search"
    assert definition.input_schema["required"] == ["query"]
    assert definition.input_schema["properties"]["candidate_k"]["default"] == 10
    assert definition.input_schema["properties"]["rerank"]["enum"] == ["none", "cross_encoder"]
    assert definition.annotations.read_only_hint is True
    assert definition.annotations.destructive_hint is False
    assert "exact codes/names" in definition.description
    assert response.is_error is not True
    body = json.loads(response.content[0].text)
    assert body["results"][0]["document_id"] == "doc-agentic-rag"
    assert body["evidence_status"] == "found"
    assert "source plus chunk_id" in body["answer_guidance"]


def test_empty_retrieval_explicitly_requires_an_insufficient_evidence_response() -> None:
    class EmptyRetriever:
        def search(self, query, *, limit, filters):
            del query, limit, filters
            return []

    empty = EmptyRetriever()
    orchestrator = RagOrchestrator(
        {"bm25": empty, "embedding": empty, "hybrid": empty},
        {"cross_encoder": MockReranker("cross_encoder")},
        implementation="empty-test",
    )

    result = orchestrator.search(SearchRequest.from_arguments({"query": "不存在的内部知识", "rerank": "none"}))

    assert result["evidence_status"] == "no_evidence"
    assert result["results"] == []
    assert "do not guess" in result["answer_guidance"]


def test_mcp_validation_error_is_a_tool_error_not_a_server_crash() -> None:
    response = invoke_search(build_default_orchestrator(), {"query": ""})

    assert response.is_error is True
    assert "query" in response.content[0].text


def test_removed_reranker_is_rejected_by_mcp_and_cli() -> None:
    from rag_mcp.debug import parse_args

    response = invoke_search(build_default_orchestrator(), {"query": "test", "rerank": "bi_encoder"})
    assert response.is_error is True
    assert "rerank must be none or cross_encoder" in response.content[0].text
    with pytest.raises(SystemExit) as error:
        parse_args(["test", "--rerank", "bi_encoder"])
    assert error.value.code == 2
