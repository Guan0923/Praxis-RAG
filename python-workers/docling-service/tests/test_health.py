from __future__ import annotations


def test_health_reports_pinned_docling(app_client):
    for path in ("/healthz", "/health", "/"):
        response = app_client.get(path)
        assert response.status_code == 200
        body = response.json()
        assert body["status"] == "ok"
        assert body["service"] == "praxis-rag-docling-service"
        assert body["contract"] == "docling-service-v1"
        assert body["formats"] == [".docx", ".html", ".htm"]
        assert body["docling_version"] == "2.150.1"
        assert body["config_error"] is None


def test_health_echoes_safe_request_id(app_client):
    response = app_client.get("/healthz", headers={"X-Request-ID": "abc-123"})
    assert response.headers["X-Request-ID"] == "abc-123"
    response = app_client.get("/healthz", headers={"X-Request-ID": "bad value!"})
    assert response.headers["X-Request-ID"] != "bad value!"
