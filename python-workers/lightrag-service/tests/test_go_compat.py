"""Compatibility tests for the exact JSON shapes the Go client sends.

The Go client is defined in ``internal/lightrag/types.go``. Go structs without
``omitempty`` serialize zero values (``null`` slices, ``""``, ``0``) and the
knowledge-base namespace format allows uppercase letters and dots, so the
sidecar accepts and defaults those shapes instead of returning 400. These tests
pin that tolerance so a stricter contract edit cannot silently break ingestion.
"""

from __future__ import annotations

from helpers import (
    ingest_document,
    query_payload,
    register_workspace,
    workspace_payload,
)


def test_plain_sha256_fingerprint_and_language_code(client, fake_engine):
    payload = workspace_payload(namespace="MyKB", fingerprint="a" * 64)
    payload["language"] = "zh"
    response = client.post("/v1/workspaces", json=payload)
    assert response.status_code == 200, response.text
    body = response.json()
    assert body["workspace"] == "praxis_MyKB"
    assert body["created"] is True
    assert fake_engine.workspaces["praxis_MyKB"].language == "Chinese"


def test_dotted_and_uppercase_namespace_round_trips(client):
    body = register_workspace(client, namespace="Team.Legal")
    assert body["workspace"] == "praxis_Team.Legal"

    ingest_document(client, namespace="Team.Legal")
    response = client.post("/v1/workspaces/Team.Legal/query", json=query_payload())
    assert response.status_code == 200, response.text
    assert len(response.json()["contexts"]) == 2


def test_null_chunk_list_is_an_empty_replace(client):
    register_workspace(client)
    response = client.post(
        "/v1/workspaces/legal/chunks",
        json={"document_id": "doc-1", "document_name": "a.pdf", "chunks": None},
    )
    assert response.status_code == 200, response.text
    assert response.json() == {
        "document_id": "doc-1",
        "accepted": 0,
        "skipped": 0,
        "entities": 0,
        "relations": 0,
    }


def test_empty_document_name_is_accepted(client):
    register_workspace(client)
    ingest_document(client, document_name="")
    response = client.post("/v1/workspaces/legal/query", json=query_payload())
    assert response.status_code == 200, response.text
    assert response.json()["contexts"][0]["document_name"] == ""


def test_query_zero_values_are_defaulted(client):
    register_workspace(client)
    ingest_document(client)
    response = client.post(
        "/v1/workspaces/legal/query",
        json={
            "query": "用户画像如何生成",
            "mode": "",
            "top_k": 0,
            "filters": {"document_ids": None},
            "include_entities": False,
        },
    )
    assert response.status_code == 200, response.text
    body = response.json()
    assert body["debug"]["mode"] == "mix"
    assert body["debug"]["chunks"] == 2
    assert body["contexts"][0]["entities"] == []


def test_query_without_filters_is_accepted(client):
    register_workspace(client)
    ingest_document(client)
    response = client.post(
        "/v1/workspaces/legal/query", json={"query": "用户画像如何生成"}
    )
    assert response.status_code == 200, response.text
    assert response.json()["debug"]["mode"] == "mix"
    assert response.json()["contexts"][0]["entities"] == ["E-c-1"]
