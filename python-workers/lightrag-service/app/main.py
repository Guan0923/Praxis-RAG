"""FastAPI application for the Praxis-RAG LightRAG sidecar.

Endpoints (all under ``http://127.0.0.1:8093`` by default)
----------------------------------------------------------
``GET /healthz`` (alias ``/``)
    Liveness and configuration probe. Always answers 200, even when the
    deployment is broken; ``config_error`` explains the problem and
    ``upstream.installed`` reports the runtime LightRAG version (or ``null``).

``POST /v1/workspaces``
    Idempotently bind a Praxis namespace to a LightRAG workspace. A changed
    ``config_fingerprint`` wipes and rebuilds the workspace.

``POST /v1/workspaces/{namespace}/chunks``
    Ingest chunks that the Go backend already parsed and split. Re-posting the
    same ``document_id`` replaces the document.

``DELETE /v1/workspaces/{namespace}/documents/{document_id}``
    Delete one document and its derived graph rows.

``POST /v1/workspaces/{namespace}/query``
    Context-only retrieval. The response contains evidence (chunks, entities,
    relations) and never generated answer text.

``DELETE /v1/workspaces/{namespace}``
    Clear a workspace and forget the namespace binding.

Cross-cutting rules: every ``/v1/*`` request requires ``Authorization: Bearer
<token>`` when ``RAG_LIGHTRAG_SERVICE_TOKEN`` is set; request bodies are limited
to ``RAG_LIGHTRAG_SERVICE_MAX_BYTES``; concurrent engine work is bounded by
``RAG_LIGHTRAG_SERVICE_CONCURRENCY``; logs never contain query text, chunk text,
document names or credentials.

Failures return ``{"detail": "<safe text>"}`` with 400 (invalid parameter), 401
(missing/invalid token), 404 (unknown namespace), 503 (upstream/engine failure)
or 500 (mapping failure).
"""

from __future__ import annotations

import asyncio
import json
import logging
import secrets
import sys
from typing import Any
from uuid import uuid4

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, Response

from . import LIGHTRAG_PACKAGE, LIGHTRAG_PIN, SERVICE_NAME, __version__
from .config import (
    Settings,
    validate_document_id,
    validate_namespace_name,
)
from .engine import GraphEngine
from .errors import ConfigError, ServiceError
from .limits import ConcurrencyGate
from .models import ChunksRequest, QueryRequest, WorkspaceRequest
from .registry import ChunkRegistry
from .service import GraphService
from .workspaces import WorkspaceStore

LOGGER = logging.getLogger(SERVICE_NAME)

REQUEST_ID_HEADER = "X-Request-ID"


class _JsonLogFormatter(logging.Formatter):
    """Render one JSON object per log line without request payloads."""

    def format(self, record: logging.LogRecord) -> str:
        """Format a record as a single-line JSON object.

        Args:
            record: Log record.

        Returns:
            JSON string with time, level, logger and message.
        """

        return json.dumps(
            {
                "time": self.formatTime(record),
                "level": record.levelname,
                "logger": record.name,
                "message": record.getMessage(),
            },
            ensure_ascii=False,
        )


def configure_logging(settings: Settings) -> None:
    """Install one process-wide handler matching the other Praxis services.

    Args:
        settings: Parsed configuration carrying level and format.

    Returns:
        ``None``.
    """

    handler = logging.StreamHandler(stream=sys.stderr)
    if settings.log_format == "json":
        handler.setFormatter(_JsonLogFormatter())
    else:
        handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(name)s %(message)s"))
    root = logging.getLogger()
    for existing in list(root.handlers):
        root.removeHandler(existing)
    root.addHandler(handler)
    root.setLevel(settings.log_level.upper())


def _request_id_from_header(value: str | None) -> str:
    """Accept a caller correlation id when it is short and safe.

    Args:
        value: Raw ``X-Request-ID`` header.

    Returns:
        The caller id, or a fresh hex id.
    """

    candidate = (value or "").strip()
    if candidate and len(candidate) <= 64 and candidate.isascii():
        if all(character.isalnum() or character in "._:-" for character in candidate):
            return candidate
    return uuid4().hex


def _request_id(request: Request) -> str:
    """Return the correlation id stored on the request.

    Args:
        request: Incoming request.

    Returns:
        Correlation id or ``"-"`` when the middleware did not run.
    """

    return getattr(request.state, "request_id", "") or "-"


def _error_response(
    status_code: int,
    message: str,
    request_id: str | None = None,
    *,
    authenticate: bool = False,
) -> JSONResponse:
    """Build the shared ``{"detail": ...}`` error response.

    Args:
        status_code: HTTP status code.
        message: Safe, non-sensitive detail text.
        request_id: Optional correlation id echoed in the header.
        authenticate: Add ``WWW-Authenticate: Bearer`` for 401 responses.

    Returns:
        JSON response object.
    """

    headers: dict[str, str] = {}
    if request_id:
        headers[REQUEST_ID_HEADER] = request_id
    if authenticate:
        headers["WWW-Authenticate"] = "Bearer"
    return JSONResponse(status_code=status_code, content={"detail": message}, headers=headers)


def _authorized(header_value: str | None, token: str) -> bool:
    """Check a ``Authorization: Bearer`` header in constant time.

    Args:
        header_value: Raw Authorization header.
        token: Expected token.

    Returns:
        ``True`` when the header carries exactly the configured token.
    """

    parts = (header_value or "").split(None, 1)
    if len(parts) != 2 or parts[0].lower() != "bearer":
        return False
    return secrets.compare_digest(parts[1], token)


def _handle_failure(request: Request, action: str, error: BaseException) -> JSONResponse:
    """Translate a raised failure into the safe HTTP error contract.

    Exception messages are deliberately not logged: an upstream exception can
    embed query text or document text, and the repository rule is that logs never
    carry payloads. Only the action, status and category are recorded.

    Args:
        request: Incoming request (for the correlation id).
        action: Short operation name for the log line.
        error: The exception that escaped the route.

    Returns:
        JSON error response.
    """

    request_id = _request_id(request)
    if isinstance(error, ServiceError):
        LOGGER.warning(
            "%s failed request_id=%s status=%d category=%s",
            action,
            request_id,
            error.status_code,
            type(error).__name__,
        )
        return _error_response(error.status_code, str(error), request_id)
    if isinstance(error, asyncio.TimeoutError):
        LOGGER.warning("%s timeout request_id=%s", action, request_id)
        return _error_response(503, "LightRAG 操作超时", request_id)
    LOGGER.error(
        "%s internal error request_id=%s category=%s",
        action,
        request_id,
        type(error).__name__,
    )
    return _error_response(500, "LightRAG 服务内部错误", request_id)


def _health(app: FastAPI) -> dict[str, Any]:
    """Build the ``/healthz`` payload.

    Args:
        app: Application holding the settings and service.

    Returns:
        Health dictionary; the caller always returns HTTP 200.
    """

    settings: Settings = app.state.settings
    config_error = settings.config_error or app.state.availability_error or app.state.workdir_error
    try:
        workspaces = app.state.service.workspace_count()
    except Exception:
        workspaces = 0
        config_error = config_error or "工作目录不可用"
    return {
        "status": "ok",
        "service": SERVICE_NAME,
        "version": __version__,
        "upstream": {
            "package": LIGHTRAG_PACKAGE,
            "pinned": LIGHTRAG_PIN,
            "installed": app.state.installed_version,
        },
        "workspaces": workspaces,
        "config_error": config_error,
    }


def _default_engine(settings: Settings) -> GraphEngine:
    """Build the production engine without importing LightRAG at module load.

    Args:
        settings: Parsed configuration.

    Returns:
        A :class:`~app.lightrag_engine.LightRAGEngine` instance.
    """

    from .lightrag_engine import LightRAGEngine

    return LightRAGEngine(settings.workdir)


def create_app(settings: Settings | None = None, engine: GraphEngine | None = None) -> FastAPI:
    """Build the ASGI application.

    ``settings``/``engine`` exist for tests: the module-level ``app`` used by
    uvicorn reads the environment and the real engine.

    Args:
        settings: Optional pre-parsed settings.
        engine: Optional engine implementation (the test suite injects a fake).

    Returns:
        Configured FastAPI application.
    """

    settings = settings or Settings.from_env()
    workdir_error: str | None = None
    try:
        settings.workdir.mkdir(parents=True, exist_ok=True)
    except OSError:
        workdir_error = "工作目录不可用"

    selected_engine = engine or _default_engine(settings)
    installed_version: str | None = None
    availability_error: str | None = None
    try:
        installed_version = selected_engine.installed_version()
        availability_error = selected_engine.availability_error()
    except Exception:
        availability_error = "LightRAG 引擎状态检查失败"

    service = GraphService(
        engine=selected_engine,
        registry=ChunkRegistry(settings.workdir),
        store=WorkspaceStore(settings.workdir),
    )

    app = FastAPI(
        title="Praxis-RAG LightRAG service",
        summary="Controlled LightRAG sidecar: graph indexing and context-only retrieval",
        version=__version__,
    )
    app.state.settings = settings
    app.state.engine = selected_engine
    app.state.service = service
    app.state.gate = ConcurrencyGate(settings.concurrency)
    app.state.installed_version = installed_version
    app.state.availability_error = availability_error
    app.state.workdir_error = workdir_error

    @app.middleware("http")
    async def guard(request: Request, call_next):
        """Apply correlation ids, bearer auth and the body-size limit.

        Args:
            request: Incoming request.
            call_next: Downstream ASGI handler.

        Returns:
            The downstream response with the correlation id header attached.
        """

        request_id = _request_id_from_header(request.headers.get(REQUEST_ID_HEADER))
        request.state.request_id = request_id
        if request.url.path.startswith("/v1/"):
            if app.state.settings.auth_required and not _authorized(
                request.headers.get("authorization"), app.state.settings.token
            ):
                return _error_response(
                    401,
                    "缺少或无效的 Bearer token",
                    request_id,
                    authenticate=True,
                )
            if request.method in {"POST", "PUT", "PATCH"}:
                limit = app.state.settings.max_body_bytes
                declared = request.headers.get("content-length", "")
                if declared.isdigit() and int(declared) > limit:
                    return _error_response(400, "请求体超过服务端大小限制", request_id)
                try:
                    body = await request.body()
                except Exception:
                    return _error_response(400, "请求体读取失败", request_id)
                if len(body) > limit:
                    return _error_response(400, "请求体超过服务端大小限制", request_id)
        response = await call_next(request)
        response.headers[REQUEST_ID_HEADER] = request_id
        return response

    @app.get("/")
    async def root(request: Request) -> JSONResponse:
        """Serve the health payload at the root path."""

        return JSONResponse(_health(request.app))

    @app.get("/healthz")
    async def healthz(request: Request) -> JSONResponse:
        """Serve the liveness/configuration payload; always HTTP 200."""

        return JSONResponse(_health(request.app))

    @app.post("/v1/workspaces")
    async def create_workspace(body: WorkspaceRequest, request: Request) -> Response:
        """Create, re-bind or rebuild the workspace of a namespace."""

        app_state = request.app.state
        try:
            async with app_state.gate.slot():
                payload = await asyncio.wait_for(
                    app_state.service.ensure_workspace(body),
                    timeout=app_state.settings.operation_timeout,
                )
        except Exception as error:  # noqa: BLE001 - mapped to the error contract
            return _handle_failure(request, "workspace", error)
        LOGGER.info(
            "workspace ensured request_id=%s namespace=%s created=%s rebuilt=%s",
            _request_id(request),
            payload["namespace"],
            payload["created"],
            payload["rebuilt"],
        )
        return JSONResponse(payload)

    @app.post("/v1/workspaces/{namespace}/chunks")
    async def insert_chunks(namespace: str, body: ChunksRequest, request: Request) -> Response:
        """Ingest one document's already-split chunks (replace on re-post)."""

        app_state = request.app.state
        try:
            validate_namespace_name(namespace)
            async with app_state.gate.slot():
                payload = await asyncio.wait_for(
                    app_state.service.insert_chunks(namespace, body),
                    timeout=app_state.settings.operation_timeout,
                )
        except Exception as error:  # noqa: BLE001 - mapped to the error contract
            return _handle_failure(request, "chunks", error)
        LOGGER.info(
            "chunks ingested request_id=%s namespace=%s document_id=%s accepted=%d skipped=%d entities=%d relations=%d",
            _request_id(request),
            namespace,
            payload["document_id"],
            payload["accepted"],
            payload["skipped"],
            payload["entities"],
            payload["relations"],
        )
        return JSONResponse(payload)

    @app.delete("/v1/workspaces/{namespace}/documents/{document_id}")
    async def delete_document(namespace: str, document_id: str, request: Request) -> Response:
        """Delete one document and its derived graph rows."""

        app_state = request.app.state
        try:
            validate_namespace_name(namespace)
            validate_document_id(document_id)
            async with app_state.gate.slot():
                payload = await asyncio.wait_for(
                    app_state.service.delete_document(namespace, document_id),
                    timeout=app_state.settings.operation_timeout,
                )
        except Exception as error:  # noqa: BLE001 - mapped to the error contract
            return _handle_failure(request, "delete_document", error)
        LOGGER.info(
            "document deleted request_id=%s namespace=%s document_id=%s deleted=%s",
            _request_id(request),
            namespace,
            payload["document_id"],
            payload["deleted"],
        )
        return JSONResponse(payload)

    @app.post("/v1/workspaces/{namespace}/query")
    async def query(namespace: str, body: QueryRequest, request: Request) -> Response:
        """Return context only; no generated answer text is ever produced."""

        app_state = request.app.state
        try:
            validate_namespace_name(namespace)
            async with app_state.gate.slot():
                payload = await asyncio.wait_for(
                    app_state.service.query(namespace, body),
                    timeout=app_state.settings.operation_timeout,
                )
        except Exception as error:  # noqa: BLE001 - mapped to the error contract
            return _handle_failure(request, "query", error)
        LOGGER.info(
            "query served request_id=%s namespace=%s mode=%s contexts=%d entities=%d relations=%d",
            _request_id(request),
            namespace,
            payload["debug"]["mode"],
            payload["debug"]["chunks"],
            payload["debug"]["entities"],
            payload["debug"]["relations"],
        )
        return JSONResponse(payload)

    @app.delete("/v1/workspaces/{namespace}")
    async def clear_workspace(namespace: str, request: Request) -> Response:
        """Clear a workspace and forget its namespace binding."""

        app_state = request.app.state
        try:
            validate_namespace_name(namespace)
            async with app_state.gate.slot():
                payload = await asyncio.wait_for(
                    app_state.service.clear_workspace(namespace),
                    timeout=app_state.settings.operation_timeout,
                )
        except Exception as error:  # noqa: BLE001 - mapped to the error contract
            return _handle_failure(request, "clear_workspace", error)
        LOGGER.info(
            "workspace cleared request_id=%s namespace=%s", _request_id(request), namespace
        )
        return JSONResponse(payload)

    @app.exception_handler(RequestValidationError)
    async def on_validation_error(
        request: Request, exc: RequestValidationError
    ) -> JSONResponse:
        """Render schema violations as 400 without echoing submitted values."""

        locations = sorted({str(error.get("loc", ("-",))[-1]) for error in exc.errors()})
        return _error_response(
            400,
            "请求格式无效：" + ", ".join(locations),
            _request_id(request),
        )

    return app


_BOOT_SETTINGS = Settings.from_env()
configure_logging(_BOOT_SETTINGS)
app = create_app(_BOOT_SETTINGS)


def main() -> None:
    """Run uvicorn, refusing to start when the deployment is unsafe.

    The process is single-worker on purpose: every workspace owns a file-backed
    LightRAG store, and multiple processes would not share it. Concurrency is
    bounded inside the process by ``RAG_LIGHTRAG_SERVICE_CONCURRENCY``.

    Returns:
        ``None``.

    Raises:
        SystemExit: The configuration is unusable (for example a non-loopback
            bind address without a bearer token).
    """

    settings = Settings.from_env()
    try:
        settings.require_startup_security()
    except ConfigError as error:
        sys.stderr.write(f"{SERVICE_NAME}: startup refused: {error}\n")
        raise SystemExit(2) from None
    configure_logging(settings)
    import uvicorn

    uvicorn.run(
        create_app(settings),
        host=settings.host,
        port=settings.port,
        workers=1,
        log_level=settings.log_level,
    )


if __name__ == "__main__":
    main()
