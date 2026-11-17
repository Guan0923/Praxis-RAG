# Praxis-RAG Docling parsing microservice.
#
# Build context is this directory:
#   docker build -t praxis-rag-docling-service python-workers/docling-service
#
# The image parses DOCX and static HTML only: docling-slim is installed with the
# convert-core / format-docx / format-html extras, so no layout model, OCR model,
# torch or docling-ibm-models is baked in. The Docling version is pinned in
# requirements.txt and validated by the service tests.

ARG BASE_IMAGE=python:3.11-slim
FROM ${BASE_IMAGE} AS runtime

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PORT=8092 \
    RAG_DOCLING_SERVICE_HOST=0.0.0.0 \
    RAG_DOCLING_SERVICE_TIMEOUT=300 \
    RAG_DOCLING_SERVICE_PARSE_TIMEOUT=120 \
    RAG_DOCLING_SERVICE_CONCURRENCY=1 \
    RAG_DOCLING_SERVICE_WORKERS=1

WORKDIR /app

COPY requirements.txt ./

ARG PIP_INDEX_URL=https://pypi.org/simple
ARG PIP_RETRIES=5
ARG PIP_TIMEOUT=120
RUN python -m pip install --upgrade pip \
    && python -m pip install --retries "${PIP_RETRIES}" --timeout "${PIP_TIMEOUT}" \
        --index-url "${PIP_INDEX_URL}" -r requirements.txt

COPY app ./app

# Run as an unprivileged user; Docling and its dependencies need no writable
# system path, only the temporary directory used for one conversion.
RUN useradd --create-home --uid 10002 --shell /usr/sbin/nologin doclingsvc \
    && mkdir -p /tmp/rag-docling-service \
    && chown doclingsvc:doclingsvc /tmp/rag-docling-service
ENV TMPDIR=/tmp/rag-docling-service

USER doclingsvc

EXPOSE 8092

# The healthcheck uses Python's standard library on purpose: the slim image
# contains no curl/wget.
HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=3 \
    CMD python -c "import os, sys, urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:' + os.environ.get('PORT', '8092') + '/healthz', timeout=5).status == 200 else 1)"

CMD ["python", "-m", "app.main"]
