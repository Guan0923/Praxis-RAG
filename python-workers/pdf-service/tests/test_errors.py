"""Error handling: safe JSON details, status codes and configuration failures."""

from __future__ import annotations

import time

import pytest
from fastapi.testclient import TestClient

from conftest import MINIMAL_PDF, parse_files
from app.main import (
    EMPTY_FILE_MESSAGE,
    INVALID_PDF_MESSAGE,
    Settings,
    create_app,
)
from app.parser import ParserConversionError, fake_manifest


class FailingConverter:
    name = "failing"

    def convert(self, source, output_dir, scratch_dir, *, options=None):
        raise RuntimeError("internal boom: SECRET-DOC-TEXT")


class RaisingParserConverter:
    name = "raising"

    def convert(self, source, output_dir, scratch_dir, *, options=None):
        raise ParserConversionError("公式为空，文档未索引")


class SlowConverter:
    name = "slow"

    def convert(self, source, output_dir, scratch_dir, *, options=None):
        time.sleep(0.3)
        return fake_manifest()


def test_missing_file_field_returns_422_with_string_detail(fake_client: TestClient) -> None:
    response = fake_client.post("/v1/parse")

    assert response.status_code == 422
    body = response.json()
    assert isinstance(body["detail"], str)
    assert "file" in body["detail"]


def test_wrong_field_name_returns_422(fake_client: TestClient) -> None:
    response = fake_client.post(
        "/v1/parse", files={"pdf": ("sample.pdf", MINIMAL_PDF, "application/pdf")}
    )

    assert response.status_code == 422
    assert isinstance(response.json()["detail"], str)


def test_empty_file_returns_400(fake_client: TestClient) -> None:
    response = fake_client.post("/v1/parse", files=parse_files(b""))

    assert response.status_code == 400
    assert response.json()["detail"] == EMPTY_FILE_MESSAGE


def test_non_pdf_payload_returns_400(fake_client: TestClient) -> None:
    response = fake_client.post("/v1/parse", files=parse_files(b"not a pdf at all"))

    assert response.status_code == 400
    assert response.json()["detail"] == INVALID_PDF_MESSAGE


def test_oversized_upload_returns_413(monkeypatch: pytest.MonkeyPatch, sample_pdf: bytes) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "fake")
    monkeypatch.setenv("RAG_PDF_SERVICE_MAX_BYTES", "32")
    client = TestClient(create_app())

    response = client.post("/v1/parse", files=parse_files(sample_pdf))

    assert response.status_code == 413
    assert "上限" in response.json()["detail"]


def test_unexpected_converter_error_is_hidden() -> None:
    client = TestClient(create_app(converter=FailingConverter()))

    response = client.post("/v1/parse", files=parse_files(MINIMAL_PDF))

    assert response.status_code == 500
    detail = response.json()["detail"]
    assert "SECRET-DOC-TEXT" not in detail
    assert "boom" not in detail


def test_parser_conversion_error_is_returned_safely() -> None:
    client = TestClient(create_app(converter=RaisingParserConverter()))

    response = client.post("/v1/parse", files=parse_files(MINIMAL_PDF))

    assert response.status_code == 500
    assert response.json()["detail"] == "公式为空，文档未索引"


def test_unconfigured_converter_returns_503_with_readable_config_error(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "docling")
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")
    client = TestClient(create_app())

    response = client.post("/v1/parse", files=parse_files(MINIMAL_PDF))

    assert response.status_code == 503
    detail = response.json()["detail"]
    assert "RAG_VLM_MODEL" in detail


def test_docling_mode_without_docling_returns_readable_503(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    import importlib.util

    if importlib.util.find_spec("docling") is not None:
        pytest.skip("docling is installed; this asserts the missing-dependency message")
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "docling")
    monkeypatch.setenv("RAG_PDF_MODELS_PATH", "/nonexistent-models")
    client = TestClient(create_app())

    response = client.post("/v1/parse", files=parse_files(MINIMAL_PDF))

    assert response.status_code == 503
    detail = response.json()["detail"]
    assert "Docling is not installed" in detail


def test_conversion_timeout_returns_504() -> None:
    settings = Settings(converter="docling", timeout=0.05)
    client = TestClient(create_app(settings=settings, converter=SlowConverter()))

    response = client.post("/v1/parse", files=parse_files(MINIMAL_PDF))

    assert response.status_code == 504
    assert "超时" in response.json()["detail"]
