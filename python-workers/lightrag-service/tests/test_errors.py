"""Authentication, upstream failure, body-limit and error-safety tests."""

from __future__ import annotations

from app.errors import EngineUnavailableError, ProcessingError

from helpers import (
    TOKEN,
    auth_headers,
    chunks_payload,
    ingest_document,
    query_payload,
    register_workspace,
    workspace_payload,
)


def test_missing_token_is_rejected(auth_client):
    response = auth_client.get("/healthz")
    assert response.status_code == 200

    rejected = auth_client.post("/v1/workspaces", json={"namespace": "legal"})
    assert rejected.status_code == 401
    assert rejected.headers["www-authenticate"] == "Bearer"
    assert "detail" in rejected.json()


def test_wrong_or_malformed_token_is_rejected(auth_client):
    for header in (
        {"Authorization": f"Bearer {TOKEN}-wrong"},
        {"Authorization": TOKEN},
        {"Authorization": f"Basic {TOKEN}"},
    ):
        response = auth_client.post(
            "/v1/workspaces", json={"namespace": "legal"}, headers=header
        )
        assert response.status_code == 401, header


def test_valid_token_is_accepted(auth_client):
    response = auth_client.post(
        "/v1/workspaces",
        json=workspace_payload(),
        headers=auth_headers(),
    )
    assert response.status_code == 200
    assert response.json()["namespace"] == "legal"


def test_engine_unavailable_maps_to_503(client, fake_engine):
    register_workspace(client)
    ingest_document(client)

    fake_engine.failure = EngineUnavailableError("LightRAG 查询失败")
    response = client.post("/v1/workspaces/legal/query", json=query_payload())
    assert response.status_code == 503
    assert response.json() == {"detail": "LightRAG 查询失败"}
    assert "用户画像" not in response.text


def test_engine_unavailable_during_ingest_maps_to_503(client, fake_engine):
    register_workspace(client)
    fake_engine.failure = EngineUnavailableError("LightRAG 写入失败")
    response = client.post("/v1/workspaces/legal/chunks", json=chunks_payload())
    assert response.status_code == 503


def test_engine_unavailable_during_workspace_create_maps_to_503(client, fake_engine):
    fake_engine.failure = EngineUnavailableError("LightRAG workspace 初始化失败")
    response = client.post(
        "/v1/workspaces",
        json=workspace_payload(),
    )
    assert response.status_code == 503


def test_unexpected_engine_error_maps_to_500(client, fake_engine):
    register_workspace(client)
    fake_engine.failure = ValueError("boom: chunk text leaked here")
    response = client.post("/v1/workspaces/legal/query", json=query_payload())
    assert response.status_code == 500
    assert response.json() == {"detail": "LightRAG 服务内部错误"}
    assert "boom" not in response.text


def test_processing_error_maps_to_500(client, fake_engine):
    register_workspace(client)
    ingest_document(client)
    fake_engine.failure = ProcessingError("LightRAG 查询返回格式无法识别")
    response = client.post("/v1/workspaces/legal/query", json=query_payload())
    assert response.status_code == 500
    assert response.json()["detail"] == "LightRAG 查询返回格式无法识别"


def test_oversized_body_is_rejected(client_factory):
    limited = client_factory(max_body_bytes=2048)
    huge_text = "长" * 4000
    payload = chunks_payload(chunks=[{"chunk_id": "c-big", "text": huge_text}])
    response = limited.post("/v1/workspaces/legal/chunks", json=payload)
    assert response.status_code == 400
    assert "大小" in response.json()["detail"]


def test_request_id_is_echoed_on_success_and_failure(client):
    response = client.get("/healthz", headers={"X-Request-ID": "abc-123"})
    assert response.headers["X-Request-ID"] == "abc-123"

    response = client.post(
        "/v1/workspaces", json={"namespace": "legal"}, headers={"X-Request-ID": "bad value!"}
    )
    assert response.status_code == 400
    assert response.headers["X-Request-ID"] != "bad value!"


def test_unknown_route_keeps_the_json_error_shape(client):
    response = client.get("/v1/unknown")
    assert response.status_code == 404
    assert "detail" in response.json()
