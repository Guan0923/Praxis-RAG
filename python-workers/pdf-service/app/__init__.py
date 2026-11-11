"""Praxis-RAG Docling PDF parsing microservice.

The HTTP app lives in :mod:`app.main` and the conversion pipeline in
:mod:`app.parser`. Importing either module never imports docling; the deep
learning stack is loaded lazily by the ``docling`` converter only.
"""

from __future__ import annotations

__all__ = ["SERVICE_NAME", "__version__"]

SERVICE_NAME = "praxis-rag-pdf-service"
__version__ = "1.0.0"
