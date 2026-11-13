"""Structured logging: correlation ids, redaction and log configuration."""

from __future__ import annotations

import asyncio
import logging
import re

import pytest
from fastapi.exceptions import RequestValidationError
from fastapi.testclient import TestClient
from starlette.requests import Request

from conftest import MINIMAL_PDF, parse_files
from app.main import (
    LOG_FORMAT_ENV,
    LOG_LEVEL_ENV,
    REQUEST_ID_HEADER,
    _log_format_from_env,
    _log_level_from_env,
    create_app,
)


class FailingConverter:
    name = "failing"

    def convert(self, source, output_dir, scratch_dir, *, options=None):
        raise RuntimeError("internal boom: SECRET-DOC-TEXT")


def _request_with_id(request_id: str) -> Request:
    request = Request({"type": "http", "method": "POST", "path": "/v1/parse", "headers": []})
    request.state.request_id = request_id
    return request


def test_validation_logging_never_echoes_input_values(caplog: pytest.LogCaptureFixture) -> None:
    app = create_app()
    handler = app.exception_handlers[RequestValidationError]
    request = _request_with_id("fixed-request-id")
    errors = [
        {
            "type": "value_error",
            "loc": ("body", "file"),
            "msg": "bad input",
            "input": {"api_key": "SECRET-API-KEY", "text": "SECRET-DOC-TEXT"},
        }
    ]
    with caplog.at_level(logging.WARNING, logger="praxis-rag-pdf-service"):
        asyncio.run(handler(request, RequestValidationError(errors)))

    logged = caplog.text
    assert "SECRET-API-KEY" not in logged
    assert "SECRET-DOC-TEXT" not in logged
    assert "request_id=fixed-request-id" in logged
    assert "error_count=1" in logged
    assert "fields=body.file" in logged
    assert "kinds=value_error" in logged


def test_validation_endpoint_logs_request_id_without_payload(
    fake_client: TestClient, caplog: pytest.LogCaptureFixture
) -> None:
    with caplog.at_level(logging.WARNING, logger="praxis-rag-pdf-service"):
        response = fake_client.post(
            "/v1/parse",
            files={"pdf": ("SECRET-FILENAME.pdf", MINIMAL_PDF, "application/pdf")},
        )
    assert response.status_code == 422
    logged = caplog.text
    assert "SECRET-FILENAME.pdf" not in logged
    assert re.search(r"request_id=[0-9a-f]{32}", logged)
    assert "fields=" in logged


def test_request_id_header_echoes_valid_inbound_value(fake_client: TestClient) -> None:
    response = fake_client.get("/healthz", headers={REQUEST_ID_HEADER: "proxy-abc.123"})
    assert response.headers[REQUEST_ID_HEADER] == "proxy-abc.123"


def test_request_id_header_is_generated_for_invalid_values(fake_client: TestClient) -> None:
    for inbound in (None, "bad id!"):
        headers = {} if inbound is None else {REQUEST_ID_HEADER: inbound}
        response = fake_client.get("/healthz", headers=headers)
        generated = response.headers[REQUEST_ID_HEADER]
        assert re.fullmatch(r"[0-9a-f]{32}", generated), generated


def test_unexpected_conversion_failure_logs_class_only(
    caplog: pytest.LogCaptureFixture,
) -> None:
    client = TestClient(create_app(converter=FailingConverter()))
    with caplog.at_level(logging.ERROR, logger="praxis-rag-pdf-service"):
        response = client.post("/v1/parse", files=parse_files(MINIMAL_PDF))

    assert response.status_code == 500
    logged = caplog.text
    assert "RuntimeError" in logged
    assert "SECRET-DOC-TEXT" not in logged
    assert "boom" not in logged
    assert "Traceback" not in logged
    assert "request_id=" in logged


def test_log_level_env_parsing(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv(LOG_LEVEL_ENV, raising=False)
    monkeypatch.delenv("RAG_DEBUG_TRACE", raising=False)
    assert _log_level_from_env() == logging.INFO

    monkeypatch.setenv(LOG_LEVEL_ENV, "warning")
    assert _log_level_from_env() == logging.WARNING

    monkeypatch.delenv(LOG_LEVEL_ENV)
    monkeypatch.setenv("RAG_DEBUG_TRACE", "true")
    assert _log_level_from_env() == logging.DEBUG

    monkeypatch.setenv(LOG_LEVEL_ENV, "verbose")
    with pytest.raises(ValueError):
        _log_level_from_env()


def test_log_format_env_parsing(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv(LOG_FORMAT_ENV, raising=False)
    assert _log_format_from_env() == "text"
    monkeypatch.setenv(LOG_FORMAT_ENV, "json")
    assert _log_format_from_env() == "json"
    monkeypatch.setenv(LOG_FORMAT_ENV, "yaml")
    with pytest.raises(ValueError):
        _log_format_from_env()
