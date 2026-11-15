# syntax=docker/dockerfile:1
#
# Praxis-RAG Go backend image.
#
# Stage 1 builds the React Console with the existing vite configuration. The
# vite outDir is ../build/web relative to frontend/, i.e. /repo/build/web
# here; the runtime stage copies that directory to /app/web and the service reads
# it through RAG_WEB_ROOT.
#
# Stage 2 builds every cmd/*/main.go entrypoint (rag-service today, the async
# workers as they land) as static Linux binaries.
#
# Stage 3 is a minimal non-root runtime image. It contains no Python, no
# credentials and no local data: pass RAG_SERVICE_TOKEN and the state/Qdrant
# settings at run time. Docker is not available in the agent environment where
# this file was authored, so the image build itself is unverified there.

FROM node:22-alpine AS web
# Override the registry for restricted networks, for example:
#   docker build --build-arg NPM_REGISTRY=https://registry.npmmirror.com .
ARG NPM_REGISTRY=https://registry.npmjs.org
WORKDIR /repo
COPY frontend/package.json frontend/package-lock.json ./frontend/
RUN cd frontend && npm ci --no-audit --no-fund --registry="${NPM_REGISTRY}"
COPY frontend ./frontend
RUN cd frontend && npm run build

FROM golang:1.27-alpine AS build
# Override the module proxy for restricted networks, for example:
#   docker build --build-arg GOPROXY=https://goproxy.cn,direct .
ARG GOPROXY=https://proxy.golang.org,direct
WORKDIR /repo
RUN apk add --no-cache ca-certificates && update-ca-certificates
COPY go.mod go.sum ./
RUN go env -w GOPROXY="${GOPROXY}" && go mod download
COPY . .
RUN set -eux; \
    mkdir -p /out; \
    for dir in ./cmd/*/; do \
        name="$(basename "$dir")"; \
        CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-s -w" -o "/out/${name}" "./${dir}"; \
    done

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S rag \
    && adduser -S -D -G rag rag

WORKDIR /app
COPY --from=build /out/ /app/bin/
COPY --from=web /repo/build/web /app/web
COPY deploy/config.container.toml /app/config.toml
# Connector templates for the one-shot `rag-connect register` service
# (connect-init) in docker-compose.yml.
COPY deploy/kafka-connect /app/connect
RUN mkdir -p /app/data && chown -R rag:rag /app

# RAG_SERVICE_HOST is non-loopback in the container, so a service token and the
# MCP allowlists from config.container.toml are mandatory at run time.
ENV HOME=/app \
    RAG_CONFIG_FILE=/app/config.toml \
    RAG_WEB_ROOT=/app/web

USER rag
EXPOSE 8011

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8011/health >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/bin/rag-service"]

