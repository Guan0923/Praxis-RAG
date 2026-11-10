# Praxis-RAG Go backend build/packaging tasks.
#
# The shell/pwsh scripts own the logic so Windows and POSIX behave the same:
#   scripts/build-go.ps1  (Windows PowerShell 5.1+ / PowerShell 7+)
#   scripts/build-go.sh   (POSIX, used by these targets)
#
# Typical use:
#   make build     # frontend + Go -> dist/web + dist/bin
#   make smoke     # start dist/bin/rag-service and check /health + /console
#   make test      # go test ./... (serial: one shared MySQL test schema)
#   make test-frontend    # frontend unit/contract tests (vitest)
#   make check     # vet + Go tests + frontend typecheck/tests/build
#
# On Windows without make, run: pwsh -File scripts/build-go.ps1

SHELL := /bin/bash

.PHONY: all frontend go build test test-frontend check vet smoke clean help

all: build

help:
	@echo "Targets: frontend go build test test-frontend check vet smoke clean"
	@echo "  build         build Console (dist/web) and Go binaries (dist/bin)"
	@echo "  test          go test ./... (serial, needs RAG_TEST_MYSQL_URL)"
	@echo "  test-frontend frontend unit/contract tests (vitest)"
	@echo "  check         vet + go tests + frontend typecheck/tests/build"
	@echo "  smoke         start the built service on a free port and check /health + /console"
	@echo "  clean         remove dist/ and the generated build/web"

frontend:
	@bash scripts/build-go.sh frontend

go:
	@bash scripts/build-go.sh go

build:
	@bash scripts/build-go.sh build

test:
	@bash scripts/build-go.sh test

test-frontend:
	@cd frontend && npm test

check: vet test test-frontend
	@cd frontend && npm run typecheck && npm run build

vet:
	@go vet ./...

smoke:
	@bash scripts/build-go.sh smoke

clean:
	@bash scripts/build-go.sh clean

