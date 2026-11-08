#!/usr/bin/env bash
# POSIX counterpart of scripts/build-go.ps1 (used by the Makefile).
#
#   ./scripts/build-go.sh frontend   # npm ci (when needed) + npm run build -> dist/web
#   ./scripts/build-go.sh go         # build every cmd/*/main.go -> dist/bin
#   ./scripts/build-go.sh build      # frontend + go
#   ./scripts/build-go.sh test       # go test ./...
#   ./scripts/build-go.sh smoke      # start the built service and check /health + /console
#   ./scripts/build-go.sh clean      # remove dist/ (and the generated build/web)
#
# The smoke check needs curl and a free loopback port; it uses a temporary config
# file, a temporary data root and SQLite, and never touches ~/.praxis-rag.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
BIN_DIR="$DIST/bin"
WEB_DIR="$DIST/web"
FRONTEND_DIR="$ROOT/frontend"
WEB_BUILD_DIR="$ROOT/build/web"
SMOKE_TIMEOUT="${SMOKE_TIMEOUT_SECONDS:-30}"

case "$(uname -s 2>/dev/null || echo unknown)" in
    MINGW*|MSYS*|CYGWIN*) EXE_SUFFIX=".exe" ;;
    *) EXE_SUFFIX="" ;;
esac

say() { printf '%s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "Required tool '$1' is not on PATH. $2"; }

build_frontend() {
    need npm "Install Node.js (>=20.19) and rerun."
    (
        cd "$FRONTEND_DIR"
        if [ ! -d node_modules ]; then
            say "[run] npm ci (frontend)"
            npm ci --no-audit --no-fund
        else
            say "[skip] frontend/node_modules already present"
        fi
        say "[run] npm run build (vite -> build/web)"
        npm run build
    )
    [ -f "$WEB_BUILD_DIR/index.html" ] || fail "Frontend build did not produce $WEB_BUILD_DIR/index.html"
    rm -rf "$WEB_DIR"
    mkdir -p "$WEB_DIR"
    cp -R "$WEB_BUILD_DIR"/. "$WEB_DIR"/
    say "[ok] frontend -> $WEB_DIR"
}

build_go() {
    need go "Install Go 1.27+ and rerun."
    rm -rf "$BIN_DIR"
    mkdir -p "$BIN_DIR"
    local found_rag_service=0
    local dir name
    for dir in "$ROOT"/cmd/*/; do
        [ -f "${dir}main.go" ] || continue
        name="$(basename "$dir")"
        if [ "$name" = "rag-service" ]; then found_rag_service=1; fi
        say "[run] go build ./cmd/$name"
        CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags="-s -w" \
            -o "$BIN_DIR/${name}${EXE_SUFFIX}" "./cmd/${name}"
        say "[ok] go -> $BIN_DIR/${name}${EXE_SUFFIX}"
    done
    [ "$found_rag_service" -eq 1 ] || fail "cmd/rag-service/main.go is missing"
}

run_tests() {
    need go "Install Go 1.27+ and rerun."
    say "[run] go test ./..."
    go test ./...
}

smoke() {
    need curl "Install curl and rerun."
    # MSYS/Git Bash must hand native Windows paths to the Windows service binary.
    to_native() {
        if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf "%s" "$1"; fi
    }
    local binary="$BIN_DIR/rag-service${EXE_SUFFIX}"
    [ -f "$binary" ] || fail "Missing $binary; run build first"
    [ -f "$WEB_DIR/index.html" ] || fail "Missing $WEB_DIR/index.html; run build first"
    local port="${SMOKE_PORT:-$((18000 + RANDOM % 1000))}"
    # Keep the smoke workspace inside the repository (gitignored .test-tmp).
    local temp
    mkdir -p "$ROOT/.test-tmp"
    temp="$(mktemp -d "$ROOT/.test-tmp/smoke-XXXXXX")"
    # Globals so the EXIT trap keeps working after this function returns.
    SMOKE_PID=""
    SMOKE_TEMP="$temp"
    cleanup() {
        if [ -n "${SMOKE_PID:-}" ] && kill -0 "$SMOKE_PID" 2>/dev/null; then
            kill "$SMOKE_PID" 2>/dev/null || true
            wait "$SMOKE_PID" 2>/dev/null || true
        fi
        rm -rf "${SMOKE_TEMP:-}"
    }
    trap cleanup EXIT
    printf '[environment]\n' > "$temp/config.toml"
    mkdir -p "$temp/data"
    say "[run] smoke: $binary on http://127.0.0.1:$port"
    (
        unset RAG_MYSQL_HOST RAG_MYSQL_PORT RAG_MYSQL_USER RAG_MYSQL_DATABASE \
              RAG_STATE_URL RAG_STATE_ENCRYPTION_KEY RAG_SERVICE_TOKEN \
              RAG_MCP_ALLOWED_HOSTS RAG_MCP_ALLOWED_ORIGINS QDRANT_PATH
        export RAG_CONFIG_FILE="$(to_native "$temp/config.toml")"
        export RAG_BACKEND="qdrant"
        export RAG_SERVICE_HOST="127.0.0.1"
        export RAG_SERVICE_PORT="$port"
        export RAG_SERVICE_DATA_ROOT="$(to_native "$temp/data")"
        export RAG_WEB_ROOT="$(to_native "$WEB_DIR")"
        export QDRANT_URL="http://127.0.0.1:6333"
        exec "$binary" >"$temp/service.log" 2>"$temp/service.err.log"
    ) &
    SMOKE_PID=$!
    local pid="$SMOKE_PID"
    local deadline=$((SECONDS + SMOKE_TIMEOUT)) health=""
    while [ "$SECONDS" -lt "$deadline" ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            break
        fi
        if health="$(curl -fsS --max-time 2 "http://127.0.0.1:$port/health" 2>/dev/null)"; then
            case "$health" in
                *'"status":"ok"'*|*'"status": "ok"'*) break ;;
            esac
        fi
        health=""
        sleep 0.2
    done
    if [ -z "$health" ]; then
        [ -f "$temp/service.err.log" ] && tail -n 20 "$temp/service.err.log" >&2 || true
        [ -f "$temp/service.log" ] && tail -n 20 "$temp/service.log" >&2 || true
        fail "Smoke check failed: /health did not report status=ok on port $port"
    fi
    curl -fsS --max-time 5 "http://127.0.0.1:$port/console" > "$temp/console.html" ||
        fail "Smoke check failed: GET /console"
    grep -q 'id="root"' "$temp/console.html" ||
        fail "Smoke check failed: /console did not serve index.html"
    grep -q '/console/assets/' "$temp/console.html" ||
        fail "Smoke check failed: /console index does not reference built assets"
    curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:$port/" ||
        fail "Smoke check failed: GET /"
    say "[ok] smoke: /health 200 status=ok; /console serves index.html; / 200"
}

clean() {
    rm -rf "$DIST" "$WEB_BUILD_DIR"
    say "[ok] removed $DIST and $WEB_BUILD_DIR"
}

target="${1:-build}"
case "$target" in
    frontend) build_frontend ;;
    go) build_go ;;
    build|all) build_frontend; build_go ;;
    test) run_tests ;;
    smoke) smoke ;;
    clean) clean ;;
    *)
        fail "Unknown target '$target' (expected frontend|go|build|test|smoke|clean)"
        ;;
esac

