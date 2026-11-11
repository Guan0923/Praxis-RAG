"""Health and service metadata endpoints."""

from __future__ import annotations

import importlib.util

import pytest
from fastapi.testclient import TestClient

from app import SERVICE_NAME, __version__
from app.main import create_app
from app.parser import SERVICE_PARSER_VERSION


def test_healthz_reports_fake_converter(fake_client: TestClient) -> None:
    response = fake_client.get("/healthz")

    assert response.status_code == 200
    body = response.json()
    assert body["status"] == "ok"
    assert body["service"] == SERVICE_NAME
    assert body["version"] == __version__
    assert body["parser_version"] == SERVICE_PARSER_VERSION
    assert body["converter"] == "fake"
    assert body["converter_requested"] == "fake"
    assert body["config_error"] is None
    assert isinstance(body["docling_installed"], bool)


def test_root_describes_the_service(fake_client: TestClient) -> None:
    response = fake_client.get("/")

    assert response.status_code == 200
    body = response.json()
    assert body["service"] == SERVICE_NAME
    assert body["converter"] == "fake"
    assert body["max_upload_bytes"] > 0
    assert body["timeout_seconds"] > 0
    assert body["concurrency"] >= 1


def test_settings_defaults(monkeypatch: pytest.MonkeyPatch) -> None:
    settings = create_app().state.settings

    assert settings.host == "0.0.0.0"
    assert settings.port == 8091
    assert settings.timeout == 900.0
    assert settings.concurrency == 1
    assert settings.converter == "docling"
    assert settings.models_path is None
    assert settings.vlm is None
    assert settings.config_error is None


def test_settings_read_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_HOST", "127.0.0.1")
    monkeypatch.setenv("PORT", "9123")
    monkeypatch.setenv("RAG_PDF_SERVICE_TIMEOUT", "42.5")
    monkeypatch.setenv("RAG_PDF_SERVICE_CONCURRENCY", "3")
    monkeypatch.setenv("RAG_PDF_MODELS_PATH", "/models")
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")
    monkeypatch.setenv("RAG_VLM_MODEL", "qwen-vl")
    monkeypatch.setenv("RAG_VLM_API_KEY", "secret-key")
    monkeypatch.setenv("RAG_VLM_TIMEOUT", "300")
    monkeypatch.setenv("RAG_VLM_CONCURRENCY", "2")

    settings = create_app().state.settings

    assert settings.host == "127.0.0.1"
    assert settings.port == 9123
    assert settings.timeout == 42.5
    assert settings.concurrency == 3
    assert settings.models_path is not None
    assert settings.models_path.name == "models"
    assert settings.vlm is not None
    assert settings.vlm.chat_url == "http://vlm.example/v1/chat/completions"
    assert settings.vlm.model == "qwen-vl"
    assert settings.vlm.concurrency == 2


def test_partial_vlm_configuration_is_a_config_error(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")

    settings = create_app().state.settings

    assert settings.vlm is None
    assert settings.config_error is not None
    assert "RAG_VLM_MODEL" in settings.config_error


def test_invalid_converter_name_is_reported_not_crashing(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "tensorrt")

    app = create_app()
    health = TestClient(app).get("/healthz")

    assert health.status_code == 200
    assert health.json()["status"] == "degraded"
    assert health.json()["converter"] == "unavailable"


def test_docling_is_not_imported_by_the_http_layer() -> None:
    """The service must import cleanly without docling/torch (test/fake mode)."""
    if importlib.util.find_spec("docling") is not None:
        pytest.skip("docling is installed in this environment; lazy-import covered by design")
    import app.main  # noqa: F401
    import app.parser  # noqa: F401
    import sys

    assert "docling" not in sys.modules
    assert "torch" not in sys.modules
