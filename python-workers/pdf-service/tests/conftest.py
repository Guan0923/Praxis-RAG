"""Shared pytest fixtures for the PDF service tests.

The suite is fully offline: no vision endpoint is contacted (vision calls use an
injected fake client or ``httpx.MockTransport``) and test PDFs are generated on
the fly with PyMuPDF. Only FastAPI, httpx, PyMuPDF, pytest and the standard
library are required.
"""

from __future__ import annotations

import json
import os
import sys
import threading
from pathlib import Path
from typing import Any

import pymupdf
import pytest
from fastapi.testclient import TestClient

SERVICE_ROOT = Path(__file__).resolve().parents[1]
if str(SERVICE_ROOT) not in sys.path:
    sys.path.insert(0, str(SERVICE_ROOT))

from app.errors import VisionError  # noqa: E402  (after sys.path setup)
from app.main import create_app  # noqa: E402

#: Smallest possible PDF that satisfies the service header check. The fake
#: converter never parses it, it only demonstrates that ``%PDF-`` sniffing and
#: the multipart plumbing work.
MINIMAL_PDF = (
    b"%PDF-1.4\n"
    b"1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n"
    b"2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n"
    b"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\n"
    b"trailer<</Root 1 0 R>>\n"
    b"%%EOF\n"
)


def build_pdf(*, pages: int = 1) -> bytes:
    """Generate a real PDF with PyMuPDF.

    Text, a filled-outlined rectangle (a picture region) and a horizontal line (a
    formula-like region) are drawn so page renders and crops contain real ink.
    """
    document = pymupdf.open()
    try:
        for index in range(pages):
            page = document.new_page(width=400, height=600)
            page.insert_text((40, 80), f"Page {index + 1}", fontsize=16)
            page.insert_text((40, 110), "Praxis-RAG vision parse fixture.", fontsize=11)
            page.draw_rect(pymupdf.Rect(40, 200, 320, 320), color=(0.1, 0.2, 0.6), width=2)
            page.draw_line((40, 350), (300, 350), color=(0.0, 0.0, 0.0), width=1)
        return document.tobytes()
    finally:
        document.close()


@pytest.fixture(autouse=True)
def clean_service_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    """Isolate each test from ambient service/VLM configuration."""
    for name in list(os.environ):
        if name.startswith("RAG_PDF_") or name.startswith("RAG_VLM_"):
            monkeypatch.delenv(name, raising=False)


@pytest.fixture
def fake_app(monkeypatch: pytest.MonkeyPatch):
    """App built with ``RAG_PDF_SERVICE_CONVERTER=fake``."""
    monkeypatch.setenv("RAG_PDF_SERVICE_CONVERTER", "fake")
    return create_app()


@pytest.fixture
def fake_client(fake_app) -> TestClient:
    with TestClient(fake_app) as client:
        yield client


@pytest.fixture
def sample_pdf() -> bytes:
    """Header-only PDF for the fake converter (never parsed)."""
    return MINIMAL_PDF


@pytest.fixture
def real_pdf() -> bytes:
    """A real two-page PDF rendered by PyMuPDF."""
    return build_pdf(pages=2)


def parse_files(
    content: bytes = MINIMAL_PDF,
    *,
    name: str = "sample.pdf",
    content_type: str = "application/pdf",
):
    """Build the multipart ``files=`` mapping for ``/v1/parse``."""
    return {"file": (name, content, content_type)}


def parse_options(**payload) -> dict[str, str]:
    """Build the multipart ``data=`` mapping carrying the ``options`` JSON."""
    return {"options": json.dumps(payload, ensure_ascii=False)}


def raw_options(text: str) -> dict[str, str]:
    """Build ``data=`` for a deliberately malformed ``options`` value."""
    return {"options": text}


class ScriptedVisionClient:
    """A vision client double with scripted answers and call recording.

    ``page_answers``/``table_answers`` are consumed in call order (the last one
    repeats); ``description_answer`` is returned for free-text description calls.
    ``failures`` makes that many leading calls raise :class:`VisionError`, which
    exercises the one-retry policy.
    """

    def __init__(
        self,
        *,
        page_answers: list[dict[str, Any]],
        table_answers: list[dict[str, Any]] | None = None,
        description_answer: str = "A diagram of the retrieval pipeline",
        failures: int = 0,
    ) -> None:
        self.page_answers = list(page_answers)
        self.table_answers = list(table_answers or [])
        self.description_answer = description_answer
        self.failures = int(failures)
        self.calls: list[dict[str, Any]] = []
        self.closed = False
        self._page_index = 0
        self._table_index = 0
        self._lock = threading.Lock()

    @staticmethod
    def kind_of(prompt: str) -> str:
        from app.vision import PAGE_PROMPT, TABLE_PROMPT

        if prompt == PAGE_PROMPT:
            return "page"
        if prompt == TABLE_PROMPT:
            return "table"
        return "description"

    def _enter(self, kind: str, image_jpeg: bytes | None) -> None:
        """Record one call (lock must be held) and apply the failure script."""
        self.calls.append(
            {"kind": kind, "image": image_jpeg, "thread": threading.current_thread().name}
        )
        if self.failures:
            self.failures -= 1
            raise VisionError("scripted failure")

    def complete_json(self, prompt: str, image_jpeg: bytes | None = None, *, max_tokens=None):
        kind = self.kind_of(prompt)
        with self._lock:
            self._enter(kind, image_jpeg)
            if kind == "page":
                answer = self.page_answers[min(self._page_index, len(self.page_answers) - 1)]
                self._page_index += 1
                return answer
            if kind == "table":
                if not self.table_answers:
                    raise VisionError("no table script")
                answer = self.table_answers[min(self._table_index, len(self.table_answers) - 1)]
                self._table_index += 1
                return answer
            raise VisionError("unexpected json call")

    def complete(self, prompt: str, image_jpeg: bytes | None = None, *, max_tokens=None) -> str:
        with self._lock:
            self._enter("description", image_jpeg)
            return self.description_answer

    def close(self) -> None:
        self.closed = True


def install_scripted_vision(monkeypatch: pytest.MonkeyPatch, client: ScriptedVisionClient) -> None:
    """Make the real vision converter use ``client`` instead of a real HTTP client."""
    from app import parser as parser_module

    monkeypatch.setattr(parser_module, "VisionClient", lambda settings: client)
