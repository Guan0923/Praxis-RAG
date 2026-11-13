# The default BuildKit frontend is enough: avoid fetching the docker/dockerfile
# frontend image from Docker Hub on restricted networks.
#
# Praxis-RAG vision-backed PDF parsing microservice.
#
# Build context is this directory:
#   docker build -t praxis-rag-pdf-service python-workers/pdf-service
#
# No model weights are baked into the image and there is no /models volume:
# PyMuPDF reads, renders and crops the PDF locally, while a remote
# OpenAI-compatible vision endpoint performs layout analysis, OCR, table
# structure, formula LaTeX and picture descriptions. Configure that endpoint per
# knowledge base in the Console (the Go backend sends it in options.vision) or
# set RAG_VLM_BASE_URL/RAG_VLM_MODEL/RAG_VLM_API_KEY as the service fallback.
#
# The healthcheck uses Python's standard library on purpose: the slim image
# contains no curl/wget.

# BASE_IMAGE raises a cached/mirrored python base for restricted networks.
ARG BASE_IMAGE=python:3.11-slim
FROM ${BASE_IMAGE} AS runtime

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PORT=8091 \
    RAG_PDF_SERVICE_HOST=0.0.0.0 \
    RAG_PDF_SERVICE_TIMEOUT=900 \
    RAG_PDF_SERVICE_CONCURRENCY=1

WORKDIR /app

# Install dependencies before copying the app so docker layer caching keeps the
# dependency layer when only application code changes.
COPY requirements.txt ./

# PyMuPDF ships manylinux wheels that bundle MuPDF, so no compiler or system
# graphics libraries are needed. APT_MIRROR is kept as an accepted build
# argument for existing compose files/scripts; the runtime stage no longer
# installs any apt package.
ARG APT_MIRROR=http://deb.debian.org
ARG PIP_INDEX_URL=https://pypi.org/simple
ARG PIP_RETRIES=5
ARG PIP_TIMEOUT=120
RUN python -m pip install --upgrade pip \
    && python -m pip install --retries "${PIP_RETRIES}" --timeout "${PIP_TIMEOUT}" \
        --index-url "${PIP_INDEX_URL}" -r requirements.txt

COPY app ./app

# Run as an unprivileged user.
RUN useradd --create-home --uid 10001 --shell /usr/sbin/nologin pdfservice \
    && mkdir -p /tmp/rag-pdf-service \
    && chown pdfservice:pdfservice /tmp/rag-pdf-service
ENV TMPDIR=/tmp/rag-pdf-service

USER pdfservice

EXPOSE 8091

HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=3 \
    CMD python -c "import os, sys, urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:' + os.environ.get('PORT', '8091') + '/healthz', timeout=5).status == 200 else 1)"

# Uvicorn manages RAG_PDF_SERVICE_WORKERS processes on the same port. Each has
# its own RAG_PDF_SERVICE_CONCURRENCY conversion slots.
ENV RAG_PDF_SERVICE_WORKERS=1
CMD ["python", "-m", "app.main"]
