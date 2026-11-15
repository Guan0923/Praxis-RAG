"""Error handling: safe JSON details, status codes and configuration failures."""

from __future__ import annotations

import asyncio
from pathlib import Path
import threading
import time

import pytest
from fastapi import HTTPException
from fastapi.testclient import TestClient

from conftest import MINIMAL_PDF, parse_files
from app.main import (
    EMPTY_FILE_MESSAGE,
    INVALID_PDF_MESSAGE,
    UNKNOWN_CONVERTER_MESSAGE,
    Settings,
    _run_conversion,
    create_app,
)
from app.parser import (
    NOT_CONFIGURED_MESSAGE,
    FakeConverter,
    ParserConversionError,
    fake_manifest,
)


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


class BlockingFirstFakeConverter:
    """Blocks the first conversion and counts concurrent conversions."""

    name = "blocking-first"

    def __init__(self, *, delay: float = 0.3) -> None:
        self.delay = delay
        self.started = threading.Event()
        self.calls = 0
        self.active = 0
        self.max_active = 0
        self._lock = threading.Lock()

    def convert(self, source, output_dir, scratch_dir, *, options=None):
        with self._lock:
            self.calls += 1
            call = self.calls
            self.active += 1
            self.max_active = max(self.max_active, self.active)
        try:
            if call == 1:
                self.started.set()
                time.sleep(self.delay)
            return FakeConverter().convert(
                source, output_dir, scratch_dir, options=options
            )
        finally:
            with self._lock:
                self.active -= 1


def _workdir(tmp_path: Path, name: str) -> Path:
    path = tmp_path / name
    path.mkdir()
    return path


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


def test_vision_converter_without_any_endpoint_returns_503() -> None:
    """No options.vision and no RAG_VLM_* -> the documented Chinese 503."""
    client = TestClient(create_app())

    response = client.post("/v1/parse", files=parse_files(MINIMAL_PDF))

    assert response.status_code == 503
    assert response.json()["detail"] == NOT_CONFIGURED_MESSAGE


def test_partial_vlm_configuration_returns_503_with_readable_config_error(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("RAG_VLM_BASE_URL", "http://vlm.example/v1")
    client = TestClient(create_app())

    response = client.post("/v1/parse", files=parse_files(MINIMAL_PDF))

    assert response.status_code == 503
    detail = response.json()["detail"]
    assert "RAG_VLM_MODEL" in detail


def test_invalid_converter_is_a_readable_503(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "onnx")
    client = TestClient(create_app())

    response = client.post("/v1/parse", files=parse_files(MINIMAL_PDF))

    assert response.status_code == 503
    detail = response.json()["detail"]
    assert detail.startswith(UNKNOWN_CONVERTER_MESSAGE)
    assert "RAG_PDF_SERVICE_CONVERTER" in detail


def test_conversion_timeout_returns_504() -> None:
    settings = Settings(converter="vision", timeout=0.2, parse_timeout=0.05)
    client = TestClient(create_app(settings=settings, converter=SlowConverter()))

    response = client.post("/v1/parse", files=parse_files(MINIMAL_PDF))

    assert response.status_code == 504
    assert "超时" in response.json()["detail"]


def test_queue_wait_does_not_consume_parse_timeout_and_waits_for_orphan(
    tmp_path: Path,
) -> None:
    settings = Settings(converter="fake", timeout=5.0, parse_timeout=0.1, concurrency=1)
    converter = BlockingFirstFakeConverter(delay=0.3)
    app = create_app(settings=settings, converter=converter)

    async def scenario() -> None:
        source_a = tmp_path / "a.pdf"
        source_a.write_bytes(MINIMAL_PDF)
        source_b = tmp_path / "b.pdf"
        source_b.write_bytes(MINIMAL_PDF + b"\n")
        # A occupies the only slot and times out after 0.1s while its thread
        # keeps running for 0.3s.
        task_a = asyncio.create_task(
            _run_conversion(app, source_a, _workdir(tmp_path, "a"), fingerprint="a")
        )
        assert await asyncio.to_thread(converter.started.wait, 2)
        # B waits for the slot much longer than its own parse window. Waiting
        # must not consume that window, so B is still pending at 0.25s.
        task_b = asyncio.create_task(
            _run_conversion(app, source_b, _workdir(tmp_path, "b"), fingerprint="b")
        )
        with pytest.raises(HTTPException) as timeout_error:
            await task_a
        assert timeout_error.value.status_code == 504
        await asyncio.sleep(0.15)
        assert not task_b.done(), "queue wait consumed the parse timeout"
        payload = await asyncio.wait_for(task_b, 5)
        assert payload

    asyncio.run(scenario())


def test_same_pdf_retry_waits_until_previous_thread_stops(tmp_path: Path) -> None:
    settings = Settings(converter="fake", timeout=5.0, parse_timeout=0.1, concurrency=2)
    converter = BlockingFirstFakeConverter(delay=0.3)
    app = create_app(settings=settings, converter=converter)

    async def scenario() -> None:
        source = tmp_path / "same.pdf"
        source.write_bytes(MINIMAL_PDF)
        task_a = asyncio.create_task(
            _run_conversion(app, source, _workdir(tmp_path, "a"), fingerprint="same")
        )
        assert await asyncio.to_thread(converter.started.wait, 2)
        with pytest.raises(HTTPException) as timeout_error:
            await task_a
        assert timeout_error.value.status_code == 504

        task_b = asyncio.create_task(
            _run_conversion(app, source, _workdir(tmp_path, "b"), fingerprint="same")
        )
        await asyncio.sleep(0.1)
        assert not task_b.done(), "the same PDF took a second slot while the first thread ran"
        assert converter.max_active == 1
        payload = await asyncio.wait_for(task_b, 5)
        assert payload
        assert converter.max_active == 1

    asyncio.run(scenario())


def test_different_pdf_uses_another_parse_slot(tmp_path: Path) -> None:
    settings = Settings(converter="fake", timeout=5.0, parse_timeout=1.0, concurrency=2)
    converter = BlockingFirstFakeConverter(delay=0.6)
    app = create_app(settings=settings, converter=converter)

    async def scenario() -> None:
        source_a = tmp_path / "a.pdf"
        source_a.write_bytes(MINIMAL_PDF)
        source_b = tmp_path / "b.pdf"
        source_b.write_bytes(MINIMAL_PDF + b"\n")
        task_a = asyncio.create_task(
            _run_conversion(app, source_a, _workdir(tmp_path, "a"), fingerprint="a")
        )
        assert await asyncio.to_thread(converter.started.wait, 2)
        task_b = asyncio.create_task(
            _run_conversion(app, source_b, _workdir(tmp_path, "b"), fingerprint="b")
        )
        payload_b = await asyncio.wait_for(task_b, 0.5)
        assert payload_b
        assert not task_a.done()
        payload_a = await asyncio.wait_for(task_a, 5)
        assert payload_a
        assert converter.max_active == 2

    asyncio.run(scenario())
