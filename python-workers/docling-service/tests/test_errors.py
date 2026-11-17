from __future__ import annotations

import io

from app import main as main_module
from app.main import Settings, create_app
from app.parser import MANIFEST_VERSION_BY_SUFFIX, SUPPORTED_SUFFIXES, validate_upload
from app.errors import ParserInputError, ParserUnsupportedError

import pytest


def test_suffix_contract():
    assert SUPPORTED_SUFFIXES == (".docx", ".html", ".htm")
    assert MANIFEST_VERSION_BY_SUFFIX == {
        ".docx": "docling-word-v1",
        ".html": "docling-html-v1",
        ".htm": "docling-html-v1",
    }


def test_validate_rejects_unsupported_suffix():
    with pytest.raises(ParserUnsupportedError):
        validate_upload(b"<html></html>", "manual.pdf")
    with pytest.raises(ParserUnsupportedError):
        validate_upload(b"%PDF", "manual.doc")


def test_validate_rejects_empty_and_invalid_html():
    with pytest.raises(ParserInputError):
        validate_upload(b"", "manual.html")
    with pytest.raises(ParserInputError):
        validate_upload(b"<html>\xff</html>", "manual.html")
    with pytest.raises(ParserInputError):
        validate_upload(b"   \n", "manual.html")


def test_validate_rejects_broken_docx():
    with pytest.raises(ParserInputError):
        validate_upload(b"not a zip", "manual.docx")


def test_unwrapped_parse_errors_are_safe(app_client):
    response = app_client.post("/v1/parse", files={"file": ("manual.pdf", b"%PDF-1.7")})
    assert response.status_code == 415
    assert "manual.pdf" not in response.text
    response = app_client.post("/v1/parse", files={"file": ("manual.docx", b"nope")})
    assert response.status_code == 400
    assert "nope" not in response.text


def test_parse_timeout_returns_504(monkeypatch, html_bytes):
    from fastapi.testclient import TestClient

    def slow_parse(*args, **kwargs):
        import time

        time.sleep(0.5)
        raise AssertionError("unreachable")

    monkeypatch.setattr(main_module, "parse_document", slow_parse)
    app = create_app(Settings(parse_timeout=0.05, timeout=1.0))
    with TestClient(app) as client:
        response = client.post("/v1/parse", files={"file": ("manual.html", html_bytes)})
    assert response.status_code == 504
    assert response.json()["detail"] == "Docling 解析超时"


def test_oversized_upload_is_rejected(html_bytes):
    from fastapi.testclient import TestClient

    app = create_app(Settings(max_upload_bytes=100))
    with TestClient(app) as client:
        response = client.post("/v1/parse", files={"file": ("manual.html", html_bytes)})
    assert response.status_code == 400
    assert "大小限制" in response.json()["detail"]
