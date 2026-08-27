"""Remote service transport tests for the standalone stdio MCP adapter."""

from __future__ import annotations

import httpx
import pytest
from rag_mcp.contracts import RagOperationError, SearchRequest
from rag_mcp.remote import RemoteRagOrchestrator, RemoteRagSettings


def test_remote_adapter_forwards_validated_search_without_exposing_namespace(monkeypatch) -> None:
    captured: dict[str, object] = {}

    def post(url, **kwargs):
        captured.update({"url": url, **kwargs})
        return httpx.Response(
            200,
            json={"evidence_status": "no_evidence", "results": []},
            request=httpx.Request("POST", url),
        )

    monkeypatch.setattr("rag_mcp.remote.httpx.post", post)
    adapter = RemoteRagOrchestrator(RemoteRagSettings("http://127.0.0.1:8011", namespace="manuals", token="test-token"))

    result = adapter.search(SearchRequest.from_arguments({"query": "E03"}))

    assert result["evidence_status"] == "no_evidence"
    assert captured["url"] == "http://127.0.0.1:8011/v1/search"
    assert captured["headers"]["X-RAG-Namespace"] == "manuals"
    assert "namespace" not in captured["json"]


def test_remote_settings_require_token_away_from_loopback(monkeypatch) -> None:
    monkeypatch.setenv("RAG_SERVICE_URL", "https://rag.example.test")
    monkeypatch.delenv("RAG_SERVICE_TOKEN", raising=False)

    with pytest.raises(RagOperationError, match="TOKEN"):
        RemoteRagSettings.from_env()
