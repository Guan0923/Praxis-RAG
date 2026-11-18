"""Workspace lifecycle tests: create, idempotency, rebuild and persistence."""

from __future__ import annotations

from app.engine import ChunkInput, DocumentInput

from helpers import (
    ALTERNATE_FINGERPRINT,
    FakeEngine,
    chunks_payload,
    ingest_document,
    query_payload,
    register_workspace,
    workspace_payload,
)


def test_create_workspace_reports_created_and_binds_models(client, fake_engine):
    body = register_workspace(client)
    assert body == {
        "namespace": "legal",
        "workspace": "praxis_legal",
        "created": True,
        "rebuilt": False,
    }
    binding = fake_engine.workspaces["praxis_legal"]
    assert binding.namespace == "legal"
    assert binding.language == "Chinese"
    assert binding.embedding.model == "bge-m3"
    assert binding.embedding.dimension == 1024
    assert binding.extract_llm.model == "qwen2.5:7b"
    assert binding.extract_llm.max_output_tokens == 9000
    assert binding.keyword_llm.timeout_seconds == 60.0


def test_repeat_create_is_idempotent_and_does_not_clear(client, fake_engine):
    register_workspace(client)
    body = register_workspace(client)
    assert body["created"] is False
    assert body["rebuilt"] is False
    assert body["workspace"] == "praxis_legal"
    assert fake_engine.cleared == []


def test_fingerprint_change_rebuilds_and_wipes_data(client, fake_engine):
    register_workspace(client)
    ingest_document(client)
    assert fake_engine.documents["praxis_legal"]

    body = register_workspace(client, fingerprint=ALTERNATE_FINGERPRINT)
    assert body["created"] is False
    assert body["rebuilt"] is True
    assert fake_engine.cleared == ["praxis_legal"]
    assert not fake_engine.documents.get("praxis_legal")

    # The namespace still exists, but its chunk metadata is gone: a query
    # returns no contexts and stale evidence cannot be served.
    response = client.post("/v1/workspaces/legal/query", json=query_payload())
    assert response.status_code == 200
    assert response.json()["contexts"] == []
    assert client.get("/healthz").json()["workspaces"] == 1


def test_binding_and_chunk_metadata_survive_a_restart(client_factory):
    first = client_factory(engine=FakeEngine())
    register_workspace(first)
    ingest_document(first)

    restarted_engine = FakeEngine()
    restarted = client_factory(engine=restarted_engine)
    assert restarted.get("/healthz").json()["workspaces"] == 1

    body = register_workspace(restarted)
    assert body["created"] is False
    assert body["rebuilt"] is False
    assert "praxis_legal" in restarted_engine.workspaces

    # Seed the fresh engine with the already-ingested document to prove that the
    # persisted registry still maps chunk ids back to document metadata.
    restarted_engine.documents["praxis_legal"] = {
        "doc-1": DocumentInput(
            document_id="doc-1",
            document_name="民法典.pdf",
            chunks=(
                ChunkInput(
                    chunk_id="c-1",
                    text="用户画像包含用户的偏好与历史行为。",
                    page=3,
                    heading="第一编",
                    upload_version=2,
                ),
            ),
        )
    }
    response = restarted.post("/v1/workspaces/legal/query", json=query_payload())
    assert response.status_code == 200
    contexts = response.json()["contexts"]
    assert len(contexts) == 1
    assert contexts[0]["chunk_id"] == "c-1"
    assert contexts[0]["document_name"] == "民法典.pdf"
    assert contexts[0]["page"] == 3
    assert contexts[0]["heading"] == "第一编"


def test_create_rejects_invalid_namespace(client):
    for namespace in ("legal space", "legal/../x", "", "a" * 65, "-legal", "legal.", "con"):
        response = client.post(
            "/v1/workspaces", json=workspace_payload(namespace=namespace)
        )
        assert response.status_code == 400, namespace
        assert "detail" in response.json()


def test_create_rejects_missing_embedding_binding(client):
    payload = workspace_payload()
    payload.pop("embedding")
    response = client.post("/v1/workspaces", json=payload)
    assert response.status_code == 400
    assert "embedding" in response.json()["detail"]


def test_create_rejects_bad_fingerprint(client):
    payload = workspace_payload()
    payload["config_fingerprint"] = "bad fingerprint!"
    response = client.post("/v1/workspaces", json=payload)
    assert response.status_code == 400


def test_create_rejects_plain_http_url_free_base_url(client):
    payload = workspace_payload()
    payload["embedding"]["base_url"] = "ftp://example.invalid/v1"
    response = client.post("/v1/workspaces", json=payload)
    assert response.status_code == 400


def test_unknown_namespace_returns_404(client):
    chunks = client.post("/v1/workspaces/legal/chunks", json=chunks_payload())
    assert chunks.status_code == 404

    query = client.post("/v1/workspaces/legal/query", json=query_payload())
    assert query.status_code == 404

    deleted = client.delete("/v1/workspaces/legal/documents/doc-1")
    assert deleted.status_code == 404

    cleared = client.delete("/v1/workspaces/legal")
    assert cleared.status_code == 404
