# The default BuildKit frontend is enough: avoid fetching the docker/dockerfile
# frontend image from Docker Hub on restricted networks.
#
# Praxis-RAG Docling PDF parsing microservice.
#
# Build context is this directory:
#   docker build -t praxis-rag-pdf-service python-workers/pdf-service
#
# The Docling model weights are NOT baked into the image. Mount them on /models
# (see README.md) and point RAG_PDF_MODELS_PATH at that directory, or set
# RAG_VLM_* to use a remote OpenAI-compatible VLM for formula recognition.
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
    RAG_PDF_MODELS_PATH=/models \
    RAG_PDF_SERVICE_TIMEOUT=900 \
    RAG_PDF_SERVICE_CONCURRENCY=1

# Docling pulls OpenCV/EasyOCR (libGL, libglib) and PyTorch (libgomp) native
# dependencies. Keep the image free of compilers and build tooling.
# APT_MIRROR overrides deb.debian.org for restricted/slow networks.
ARG APT_MIRROR=http://deb.debian.org
RUN if [ "${APT_MIRROR}" != "http://deb.debian.org" ]; then \
        sed -i "s|http://deb.debian.org|${APT_MIRROR}|g" /etc/apt/sources.list.d/*.sources; \
    fi \
    && apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=60 update \
    && apt-get -o Acquire::Retries=5 -o Acquire::http::Timeout=60 install -y --no-install-recommends \
        libgl1 \
        libglib2.0-0 \
        libgomp1 \
        libgfortran5 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Install dependencies before copying the app so docker layer caching keeps
# torch and docling when only application code changes.
COPY requirements.txt ./

# Install the CPU-only torch wheel first: PyPI's default linux wheel bundles the
# CUDA runtime, which would balloon the image and is never used on CPU hosts.
# Override TORCH_INDEX_URL/PIP_INDEX_URL for restricted networks.
ARG PIP_INDEX_URL=https://pypi.org/simple
ARG TORCH_INDEX_URL=https://download.pytorch.org/whl/cpu
# Direct wheel URLs win over the index: useful on slow mirrors where the
# Pytorch index is unreachable but the wheel file is mirrored elsewhere.
ARG TORCH_WHEEL_URL=
ARG TORCHVISION_WHEEL_URL=
ARG PIP_RETRIES=5
ARG PIP_TIMEOUT=120
RUN python -m pip install --upgrade pip \
    && if [ -n "${TORCH_WHEEL_URL}" ]; then \
        pip install --retries "${PIP_RETRIES}" --timeout "${PIP_TIMEOUT}" "${TORCH_WHEEL_URL}"; \
    else \
        pip install --retries "${PIP_RETRIES}" --timeout "${PIP_TIMEOUT}" \
            --index-url "${TORCH_INDEX_URL}" --extra-index-url "${PIP_INDEX_URL}" "torch>=2.0"; \
    fi \
    && if [ -n "${TORCHVISION_WHEEL_URL}" ]; then \
        pip install --retries "${PIP_RETRIES}" --timeout "${PIP_TIMEOUT}" "${TORCHVISION_WHEEL_URL}"; \
    else \
        pip install --retries "${PIP_RETRIES}" --timeout "${PIP_TIMEOUT}" \
            --index-url "${TORCH_INDEX_URL}" --extra-index-url "${PIP_INDEX_URL}" "torchvision>=0"; \
    fi \
    && pip install --retries "${PIP_RETRIES}" --timeout "${PIP_TIMEOUT}" \
        --index-url "${PIP_INDEX_URL}" -r requirements.txt

COPY app ./app

# Run as an unprivileged user. /models is a mounted volume; the service only
# reads it, so read permission is enough (chmod -R a+rX on the host if needed).
RUN useradd --create-home --uid 10001 --shell /usr/sbin/nologin pdfservice \
    && mkdir -p /models /tmp/rag-pdf-service \
    && chown pdfservice:pdfservice /models /tmp/rag-pdf-service
ENV TMPDIR=/tmp/rag-pdf-service

USER pdfservice

EXPOSE 8091

HEALTHCHECK --interval=30s --timeout=10s --start-period=60s --retries=3 \
    CMD python -c "import os, sys, urllib.request; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:' + os.environ.get('PORT', '8091') + '/healthz', timeout=5).status == 200 else 1)"

# Uvicorn manages RAG_PDF_SERVICE_WORKERS processes on the same port. Each has
# its own models and RAG_PDF_SERVICE_CONCURRENCY conversion slots.
ENV RAG_PDF_SERVICE_WORKERS=1
CMD ["python", "-m", "app.main"]
