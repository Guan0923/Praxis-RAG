# Praxis-RAG LightRAG sidecar.
#
# Build context is this directory:
#   docker build -t praxis-rag-lightrag-service python-workers/lightrag-service
#
# The image installs requirements.txt, which pins lightrag-hku to the exact
# release the sidecar was written against. The service binds 0.0.0.0 inside the
# container, so a bearer token is mandatory: pass
#   -e RAG_LIGHTRAG_SERVICE_TOKEN=<secret>
# or the process refuses to start.
#
# No model weights are baked in: embeddings and chat completions go to the
# OpenAI-compatible endpoints supplied per workspace in POST /v1/workspaces.
#
# The healthcheck uses Python's standard library on purpose: the slim image
# contains no curl/wget.

ARG BASE_IMAGE=python:3.11-slim
FROM ${BASE_IMAGE} AS runtime

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    RAG_LIGHTRAG_SERVICE_HOST=0.0.0.0 \
    RAG_LIGHTRAG_SERVICE_PORT=8093 \
    RAG_LIGHTRAG_SERVICE_WORKING_DIR=/data \
    RAG_LIGHTRAG_SERVICE_CONCURRENCY=2 \
    RAG_LIGHTRAG_SERVICE_TIMEOUT=900

WORKDIR /app

# Install dependencies before copying the app so docker layer caching keeps the
# dependency layer when only application code changes.
COPY requirements.txt ./

ARG PIP_INDEX_URL=https://pypi.org/simple
ARG PIP_RETRIES=5
ARG PIP_TIMEOUT=120
RUN python -m pip install --upgrade pip \
    && python -m pip install --retries "${PIP_RETRIES}" --timeout "${PIP_TIMEOUT}" \
        --index-url "${PIP_INDEX_URL}" -r requirements.txt

COPY app ./app

# Run as an unprivileged user; LightRAG only needs the working directory.
RUN useradd --create-home --uid 10003 --shell /usr/sbin/nologin lightragsvc \
    && mkdir -p /data \
    && chown lightragsvc:lightragsvc /data

USER lightragsvc

EXPOSE 8093

HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=3 \
    CMD python -c "import os, sys, urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:' + os.environ.get('RAG_LIGHTRAG_SERVICE_PORT', '8093') + '/healthz', timeout=5).status == 200 else 1)"

CMD ["python", "-m", "app.main"]
