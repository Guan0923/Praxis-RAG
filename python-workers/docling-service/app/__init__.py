"""Praxis-RAG Docling parsing service.

A standalone FastAPI microservice that parses static HTML and DOCX files with a
pinned Docling release and returns a versioned ZIP manifest the Go backend can
map to word/HTML evidence. The service is stateless: one HTTP call parses one
file, and the response is self-contained.
"""

from __future__ import annotations

SERVICE_NAME = "praxis-rag-docling-service"
__version__ = "1.0.0"
