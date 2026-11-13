"""Praxis-RAG vision-backed PDF parsing microservice.

The HTTP app lives in :mod:`app.main`, the conversion pipeline in
:mod:`app.parser`, local PDF rendering in :mod:`app.pdfreader` and the remote
OpenAI-compatible vision client in :mod:`app.vision`.

No model weights are loaded in this process: PyMuPDF reads/renders/crops the PDF
locally and a remote vision endpoint performs layout analysis, OCR, table
structure, formula LaTeX and picture descriptions.
"""

from __future__ import annotations

__all__ = ["SERVICE_NAME", "__version__"]

SERVICE_NAME = "praxis-rag-pdf-service"
__version__ = "2.0.0"
