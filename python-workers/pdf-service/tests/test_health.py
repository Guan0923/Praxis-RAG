"""Health, service metadata and process settings."""

from __future__ import annotations

import sys

import pytest
from fastapi.testclient import TestClient

from app import SERVICE_NAME, __version__
from app.main import create_app
from app.parser import SERVICE_PARSER_VERSION

HEALTH_FIELDS = {
    "service",
    "version",
    "parser_version",
    "converter",
    "converter_requested",
    "vision_configured",
    "vision_model",
    "max_upload_bytes",
    "timeout_seconds",
    "parse_timeout_seconds",
    "concurrency",
    "config_error",
    "status",
}


def test_healthz_reports_fake_converter(fake_client: TestClient) -> None:
    response = fake_client.get("/healthz")

    assert response.status_code == 200
    body = response.json()
    assert set(body) == HEALTH_FIELDS
    assert body["status"] == "ok"
    assert body["service"] == SERVICE_NAME
    assert body["version"] == __version__
    assert body["parser_version"] == SERVICE_PARSER_VERSION
    assert body["converter"] == "fake"
    assert body["converter_requested"] == "fake"
    assert body["vision_configured"] is False
    assert body["vision_model"] is None
    assert body["config_error"] is None
    assert body["max_upload_bytes"] > 0
    assert body["timeout_seconds"] > 0
    assert body["parse_timeout_seconds"] > 0
    assert body["concurrency"] >= 1
    # Local-inference probe fields are gone.
    assert "docling_installed" not in body
    assert "models_configured" not in body


def test_root_describes_the_service(fake_client: TestClient) -> None:
    response = fake_client.get("/")

    assert response.status_code == 200
    body = response.json()
    assert set(body) == HEALTH_FIELDS
    assert body["service"] == SERVICE_NAME
    assert body["converter"] == "fake"


def test_settings_defaults() -> None:
    settings = create_app().state.settings

    assert settings.host == "0.0.0.0"
    assert settings.port == 8091
    assert settings.timeout == 900.0
    assert settings.parse_timeout == 120.0
    assert settings.workers == 1
    assert settings.concurrency == 1
    assert settings.converter == "vision"
    assert settings.converter_requested == "vision"
    assert settings.page_dpi == 200
    assert settings.max_tokens == 4096
    assert settings.prompt_version == "vision-v1"
    assert settings.vlm is None
    assert settings.config_error is None
    # The local models volume is gone for good.
    assert not hasattr(settings, "models_path")


def test_settings_read_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_HOST", "127.0.0.1")
    monkeypatch.setenv("PORT", "9123")
    monkeypatch.setenv("RAG_PDF_SERVICE_TIMEOUT", "42.5")
    monkeypatch.setenv("RAG_PDF_SERVICE_PARSE_TIMEOUT", "30")
    monkeypatch.setenv("RAG_PDF_SERVICE_WORKERS", "2")
    monkeypatch.setenv("RAG_PDF_SERVICE_CONCURRENCY", "3")
    monkeypatch.setenv("RAG_PDF_PAGE_DPI", "150")
    monkeypatch.setenv("RAG_VLM_PROMPT_VERSION", "vision-v2")
    monkeypatch.setenv("RAG_VLM_MAX_TOKENS", "2048")
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")
    monkeypatch.setenv("RAG_VLM_MODEL", "qwen-vl")
    monkeypatch.setenv("RAG_VLM_API_KEY", "secret-key")
    monkeypatch.setenv("RAG_VLM_TIMEOUT", "300")
    monkeypatch.setenv("RAG_VLM_CONCURRENCY", "2")
    # Deprecated: the local models path is ignored, never read.
    monkeypatch.setenv("RAG_PDF_MODELS_PATH", "/models")

    settings = create_app().state.settings

    assert settings.host == "127.0.0.1"
    assert settings.port == 9123
    assert settings.timeout == 42.5
    assert settings.parse_timeout == 30.0
    assert settings.workers == 2
    assert settings.concurrency == 3
    assert settings.page_dpi == 150
    assert settings.prompt_version == "vision-v2"
    assert settings.max_tokens == 2048
    assert settings.vlm is not None
    assert settings.vlm.chat_url == "http://vlm.example/v1/chat/completions"
    assert settings.vlm.model == "qwen-vl"
    assert settings.vlm.api_key == "secret-key"
    assert settings.vlm.timeout == 300.0
    assert settings.vlm.concurrency == 2


def test_settings_reject_parse_timeout_not_smaller_than_total(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_TIMEOUT", "120")
    monkeypatch.setenv("RAG_PDF_SERVICE_PARSE_TIMEOUT", "120")

    settings = create_app().state.settings

    assert settings.config_error is not None
    assert "PARSE_TIMEOUT" in settings.config_error


def test_healthz_never_exposes_the_api_key(
    monkeypatch: pytest.MonkeyPatch, fake_client: TestClient
) -> None:
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")
    monkeypatch.setenv("RAG_VLM_MODEL", "qwen-vl")
    monkeypatch.setenv("RAG_VLM_API_KEY", "sk-health-secret")

    body = TestClient(create_app()).get("/healthz")

    assert body.status_code == 200
    assert "sk-health-secret" not in body.text


def test_partial_vlm_configuration_is_a_config_error(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")

    settings = create_app().state.settings

    assert settings.vlm is None
    assert settings.config_error is not None
    assert "RAG_VLM_MODEL" in settings.config_error


def test_partial_vlm_configuration_degrades_health(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")

    response = TestClient(create_app()).get("/healthz")

    assert response.status_code == 200
    body = response.json()
    assert body["status"] == "degraded"
    assert "RAG_VLM_MODEL" in body["config_error"]


def test_invalid_converter_name_is_reported_not_crashing(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "tensorrt")

    app = create_app()
    health = TestClient(app).get("/healthz")

    assert health.status_code == 200
    assert health.json()["status"] == "degraded"
    assert health.json()["converter"] == "unavailable"
    assert health.json()["converter_requested"] == "tensorrt"


def test_legacy_docling_converter_is_a_vision_alias(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "docling")
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")
    monkeypatch.setenv("RAG_VLM_MODEL", "qwen-vl")

    app = create_app()
    health = TestClient(app).get("/healthz")

    assert health.json()["converter"] == "vision"
    assert health.json()["converter_requested"] == "docling"
    assert health.json()["status"] == "ok"
    assert health.json()["vision_configured"] is True
    assert health.json()["vision_model"] == "qwen-vl"


@pytest.mark.parametrize("dpi", ["50", "400", "invalid"])
def test_invalid_page_dpi_is_reported(
    monkeypatch: pytest.MonkeyPatch, dpi: str
) -> None:
    monkeypatch.setenv("RAG_PDF_PAGE_DPI", dpi)

    settings = create_app().state.settings

    assert settings.config_error is not None
    assert "RAG_PDF_PAGE_DPI" in settings.config_error


def test_no_local_inference_stack_is_imported() -> None:
    """The service must not pull in docling/torch/easyocr anymore."""
    import app.main  # noqa: F401
    import app.parser  # noqa: F401
    import app.pdfreader  # noqa: F401
    import app.vision  # noqa: F401

    assert "docling" not in sys.modules
    assert "torch" not in sys.modules
    assert "easyocr" not in sys.modules
    assert "pytesseract" not in sys.modules
    assert "rapidocr_onnxruntime" not in sys.modules
