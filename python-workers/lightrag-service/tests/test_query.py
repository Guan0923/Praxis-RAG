"""Context-only query contract tests."""

from __future__ import annotations

from app.engine import (
    EngineChunk,
    EngineEntity,
    EngineQueryResult,
    EngineRelation,
    custom_chunk_key,
)

from helpers import ingest_document, query_payload, register_workspace


def _query(client, **kwargs):
    payload = query_payload(**kwargs)
    response = client.post("/v1/workspaces/legal/query", json=payload)
    assert response.status_code == 200, response.text
    return response.json()


def test_query_returns_context_and_never_an_answer(client):
    register_workspace(client)
    ingest_document(client)

    body = _query(client)
    assert set(body) == {"contexts", "debug"}
    for forbidden in ("answer", "response", "llm_response", "generated_text"):
        assert forbidden not in body
    assert body["debug"] == {"mode": "mix", "entities": 2, "relations": 2, "chunks": 2}

    context = body["contexts"][0]
    assert set(context) == {
        "chunk_id",
        "document_id",
        "document_name",
        "page",
        "heading",
        "text",
        "score",
        "entities",
        "relations",
    }
    assert context["chunk_id"] == "c-1"
    assert context["document_id"] == "doc-1"
    assert context["document_name"] == "民法典.pdf"
    assert context["page"] == 3
    assert context["heading"] == "第一编"
    assert context["text"] == "用户画像包含用户的偏好与历史行为。"
    assert context["score"] == 1.0
    assert context["entities"] == ["E-c-1"]
    assert context["relations"] == [
        {"source": "A-c-1", "target": "B-c-1", "description": "relation"}
    ]


def test_contexts_carry_rank_scores(client):
    register_workspace(client)
    ingest_document(client)
    body = _query(client)
    assert [context["score"] for context in body["contexts"]] == [1.0, 0.5]


def test_include_entities_false_drops_per_context_evidence(client):
    register_workspace(client)
    ingest_document(client)
    body = _query(client, include_entities=False)
    assert len(body["contexts"]) == 2
    for context in body["contexts"]:
        assert context["entities"] == []
        assert context["relations"] == []
    assert body["debug"]["entities"] == 2
    assert body["debug"]["relations"] == 2


def test_document_filter_is_applied_after_mapping(client):
    register_workspace(client)
    ingest_document(client, document_id="doc-1", document_name="第一份.pdf")
    ingest_document(client, document_id="doc-2", document_name="第二份.pdf")

    body = _query(client, filters={"document_ids": ["doc-2"]})
    assert body["contexts"]
    assert {context["document_id"] for context in body["contexts"]} == {"doc-2"}
    assert {context["document_name"] for context in body["contexts"]} == {"第二份.pdf"}


def test_top_k_truncates_contexts(client):
    register_workspace(client)
    ingest_document(client)
    body = _query(client, top_k=1)
    assert len(body["contexts"]) == 1
    assert body["debug"]["chunks"] == 1


def test_unknown_engine_chunk_is_skipped(client, fake_engine):
    register_workspace(client)
    ingest_document(client)
    fake_engine.query_result = EngineQueryResult(
        chunks=(EngineChunk(chunk_id="chunk-unknown", text="完全未知的文本"),)
    )
    body = _query(client)
    assert body["contexts"] == []
    assert body["debug"]["chunks"] == 0


def test_content_hash_fallback_maps_the_chunk(client, fake_engine):
    register_workspace(client)
    ingest_document(client)
    fake_engine.query_result = EngineQueryResult(
        chunks=(
            EngineChunk(
                chunk_id="chunk-unknown",
                text="用户画像包含用户的偏好与历史行为。",
            ),
        ),
        entities=(EngineEntity(name="用户画像", chunk_ids=("chunk-unknown",)),),
        relations=(
            EngineRelation(
                source="用户画像",
                target="历史行为",
                description="包含",
                chunk_ids=("chunk-unknown",),
            ),
        ),
    )
    body = _query(client)
    assert len(body["contexts"]) == 1
    context = body["contexts"][0]
    assert context["chunk_id"] == "c-1"
    assert context["document_name"] == "民法典.pdf"
    assert context["entities"] == ["用户画像"]
    assert context["relations"] == [
        {"source": "用户画像", "target": "历史行为", "description": "包含"}
    ]


def test_upstream_score_wins_when_present(client, fake_engine):
    register_workspace(client)
    ingest_document(client)
    fake_engine.query_result = EngineQueryResult(
        chunks=(
            EngineChunk(
                chunk_id=custom_chunk_key("doc-1", "用户画像包含用户的偏好与历史行为。"),
                text="用户画像包含用户的偏好与历史行为。",
                score=0.87,
            ),
        )
    )
    body = _query(client)
    assert body["contexts"][0]["score"] == 0.87


def test_query_rejects_invalid_parameters(client):
    register_workspace(client)
    cases = [
        query_payload(mode="bypass"),
        query_payload(mode="MIX"),
        query_payload(top_k=101),
        query_payload(query="ab"),
        query_payload(query="   "),
    ]
    for payload in cases:
        response = client.post("/v1/workspaces/legal/query", json=payload)
        assert response.status_code == 400, payload

    missing = client.post("/v1/workspaces/legal/query", json={"mode": "mix"})
    assert missing.status_code == 400
    assert "ab" not in missing.text


def test_query_does_not_echo_query_text_on_validation_error(client):
    register_workspace(client)
    secret_query = "秘密查询文本"
    response = client.post(
        "/v1/workspaces/legal/query",
        json={"query": secret_query, "mode": "unknown-mode"},
    )
    assert response.status_code == 400
    assert secret_query not in response.text
