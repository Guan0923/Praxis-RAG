"""Shared pytest fixtures for the PDF service tests.

The suite must run on a machine without docling/torch: every fixture here only
uses FastAPI, Pillow and the standard library, and the service is exercised
through its ``fake`` converter or through injected converter doubles.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

SERVICE_ROOT = Path(__file__).resolve().parents[1]
if str(SERVICE_ROOT) not in sys.path:
    sys.path.insert(0, str(SERVICE_ROOT))

from app.main import create_app  # noqa: E402  (after sys.path setup)

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
    return MINIMAL_PDF


def parse_files(content: bytes = MINIMAL_PDF, *, name: str = "sample.pdf", content_type: str = "application/pdf"):
    """Build the multipart ``files=`` mapping for ``/v1/parse``."""
    return {"file": (name, content, content_type)}
