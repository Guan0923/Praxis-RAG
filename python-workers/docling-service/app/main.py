"""FastAPI application for the Praxis-RAG Docling parsing microservice.

Endpoints
---------
``GET /healthz`` (and ``/health``)
    Liveness/configuration probe. Always answers 200 so a container healthcheck
    stays useful while the operator fixes a dependency problem; the body reports
    the pinned Docling version, the manifest contract and any ``config_error``.

``POST /v1/parse``
    ``multipart/form-data`` field ``file`` carrying one ``.docx``, ``.html`` or
    ``.htm`` file. On success the response is ``application/zip`` with
    ``manifest.json`` (the versioned contract the Go backend ingests),
    ``document.json`` (serialized DoclingDocument for audit) and every picture
    at its manifest-relative path (for example ``images/picture-1.png``).
    Failures are JSON ``{"detail": "<safe text>"}`` that never quotes document
    content. Invalid input is 400, an unsupported format 415, a deployment
    problem 503, a conversion failure 500 and an expired conversion window 504.

The Go backend unpacks the ZIP into its parser staging directory and feeds
``manifest.json`` to ``document.BuildWordContent``; the ZIP layout is therefore
part of the contract and is validated by :func:`app.parser.build_manifest`.
"""

from __future__ import annotations

import asyncio
import io
import json
import logging
import os
import re
import sys
import tempfile
import threading
import time
import zipfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from uuid import uuid4

from fastapi import FastAPI, File, Form, Request, UploadFile
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, Response

from . import SERVICE_NAME, __version__
from .errors import (
    ParserConfigError,
    ParserConversionError,
    ParserError,
    ParserInputError,
    ParserUnsupportedError,
)
from .parser import (
    MANIFEST_VERSION_BY_SUFFIX,
    SERVICE_CONTRACT_VERSION,
    SUPPORTED_SUFFIXES,
    DoclingConverter,
    parse_document,
    validate_upload,
)

LOGGER = logging.getLogger(SERVICE_NAME)

DEFAULT_HOST = "0.0.0.0"
DEFAULT_PORT = 8092
DEFAULT_TIMEOUT = 300.0
DEFAULT_PARSE_TIMEOUT = 120.0
DEFAULT_CONCURRENCY = 1
DEFAULT_MAX_UPLOAD_BYTES = 50 * 1024 * 1024
UPLOAD_CHUNK_BYTES = 1024 * 1024

LOG_LEVEL_ENV = "RAG_DOCLING_SERVICE_LOG_LEVEL"
LOG_FORMAT_ENV = "RAG_DOCLING_SERVICE_LOG_FORMAT"
DEBUG_TRACE_ENV = "RAG_DEBUG_TRACE"
REQUEST_ID_HEADER = "X-Request-ID"

_REQUEST_ID_PATTERN = re.compile(r"^[A-Za-z0-9._:-]{1,64}$")
_TRUTHY_VALUES = {"1", "true", "yes", "on"}
_LOG_LEVELS = {
    "debug": logging.DEBUG,
    "info": logging.INFO,
    "warn": logging.WARNING,
    "warning": logging.WARNING,
    "error": logging.ERROR,
}


class _JsonLogFormatter(logging.Formatter):
    """One JSON object per line; no request payloads or document text."""

    def format(self, record: logging.LogRecord) -> str:
        return json.dumps(
            {
                "time": self.formatTime(record),
                "level": record.levelname,
                "logger": record.name,
                "message": record.getMessage(),
            },
            ensure_ascii=False,
        )


def _truthy(value: str | None) -> bool:
    return (value or "").strip().lower() in _TRUTHY_VALUES


def _log_level_from_env() -> int:
    raw = os.environ.get(LOG_LEVEL_ENV, "").strip().lower()
    if not raw:
        return logging.DEBUG if _truthy(os.environ.get(DEBUG_TRACE_ENV)) else logging.INFO
    level = _LOG_LEVELS.get(raw)
    if level is None:
        raise ValueError(f"{LOG_LEVEL_ENV} must be one of debug, info, warning, error (got {raw!r})")
    return level


def _log_format_from_env() -> str:
    raw = os.environ.get(LOG_FORMAT_ENV, "").strip().lower()
    if not raw:
        return "text"
    if raw not in {"text", "json"}:
        raise ValueError(f"{LOG_FORMAT_ENV} must be one of text, json (got {raw!r})")
    return raw


def configure_logging() -> None:
    """Install one process-wide handler matching the Go service conventions."""
    level = _log_level_from_env()
    log_format = _log_format_from_env()
    handler = logging.StreamHandler(stream=sys.stderr)
    if log_format == "json":
        handler.setFormatter(_JsonLogFormatter())
    else:
        handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(name)s %(message)s"))
    root = logging.getLogger()
    for existing in list(root.handlers):
        root.removeHandler(existing)
    root.addHandler(handler)
    root.setLevel(level)


def _request_id_from_header(value: str | None) -> str:
    candidate = (value or "").strip()
    if _REQUEST_ID_PATTERN.fullmatch(candidate):
        return candidate
    return uuid4().hex


def _request_id(request: Request) -> str:
    return getattr(request.state, "request_id", "") or "-"


@dataclass(frozen=True)
class Settings:
    """Runtime configuration read from the environment."""

    host: str = DEFAULT_HOST
    port: int = DEFAULT_PORT
    timeout: float = DEFAULT_TIMEOUT
    parse_timeout: float = DEFAULT_PARSE_TIMEOUT
    concurrency: int = DEFAULT_CONCURRENCY
    workers: int = 1
    max_upload_bytes: int = DEFAULT_MAX_UPLOAD_BYTES
    config_error: str | None = None

    @classmethod
    def from_env(cls) -> "Settings":
        errors: list[str] = []
        host = os.environ.get("RAG_DOCLING_SERVICE_HOST", DEFAULT_HOST).strip() or DEFAULT_HOST
        port = _int_env("PORT", DEFAULT_PORT, minimum=1, errors=errors)
        timeout = _float_env("RAG_DOCLING_SERVICE_TIMEOUT", DEFAULT_TIMEOUT, minimum=0.001, errors=errors)
        parse_timeout = _float_env(
            "RAG_DOCLING_SERVICE_PARSE_TIMEOUT", DEFAULT_PARSE_TIMEOUT, minimum=0.001, errors=errors
        )
        if parse_timeout >= timeout:
            errors.append(
                "RAG_DOCLING_SERVICE_PARSE_TIMEOUT must be smaller than "
                "RAG_DOCLING_SERVICE_TIMEOUT so the request can finish within the "
                "client's total deadline"
            )
        concurrency = _int_env("RAG_DOCLING_SERVICE_CONCURRENCY", DEFAULT_CONCURRENCY, minimum=1, errors=errors)
        workers = _int_env("RAG_DOCLING_SERVICE_WORKERS", 1, minimum=1, errors=errors)
        max_upload_bytes = _int_env(
            "RAG_DOCLING_SERVICE_MAX_BYTES", DEFAULT_MAX_UPLOAD_BYTES, minimum=1, errors=errors
        )
        return cls(
            host=host,
            port=port,
            timeout=timeout,
            parse_timeout=parse_timeout,
            concurrency=concurrency,
            workers=workers,
            max_upload_bytes=max_upload_bytes,
            config_error="; ".join(errors) or None,
        )


def _int_env(name: str, default: int, *, minimum: int, errors: list[str]) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError:
        errors.append(f"{name} must be an integer")
        return default
    if value < minimum:
        errors.append(f"{name} must be >= {minimum}")
        return default
    return value


def _float_env(name: str, default: float, *, minimum: float, errors: list[str]) -> float:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError:
        errors.append(f"{name} must be a number of seconds")
        return default
    if value < minimum:
        errors.append(f"{name} must be >= {minimum}")
        return default
    return value


def _docling_import_error() -> str | None:
    """Return a safe description of a missing Docling install, or None."""
    try:
        from docling.document_converter import DocumentConverter  # noqa: F401
        from docling_core.types.doc import DoclingDocument  # noqa: F401
    except Exception:
        return "Docling 未正确安装：无法导入 docling"
    return None


def _manifest_version(filename: str) -> str:
    suffix = Path(filename or "").suffix.lower()
    return MANIFEST_VERSION_BY_SUFFIX.get(suffix, "")


def _health(app: FastAPI) -> dict[str, Any]:
    settings: Settings = app.state.settings
    return {
        "status": "ok",
        "service": SERVICE_NAME,
        "version": __version__,
        "contract": SERVICE_CONTRACT_VERSION,
        "docling_version": app.state.converter.version(),
        "formats": list(SUPPORTED_SUFFIXES),
        "config_error": settings.config_error or app.state.docling_error,
    }


def _error_response(status_code: int, message: str) -> JSONResponse:
    return JSONResponse(status_code=status_code, content={"detail": message})


def _zip_response(output: Any) -> Response:
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, mode="w", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.writestr(
            "manifest.json",
            json.dumps(output.manifest, ensure_ascii=False, indent=2).encode("utf-8"),
        )
        archive.writestr(
            "document.json",
            json.dumps(output.document_json, ensure_ascii=False).encode("utf-8"),
        )
        # Images are keyed by asset id in memory; the archive member must use
        # the manifest-relative image_file path the Go client reads.
        for asset in output.manifest.get("assets") or []:
            relative = asset.get("image_file")
            if not relative:
                continue
            data = output.images.get(asset.get("asset_id"))
            if data is None:
                continue
            archive.writestr(relative, data)
    return Response(
        content=buffer.getvalue(),
        media_type="application/zip",
        headers={"Content-Disposition": 'attachment; filename="docling-parse.zip"'},
    )


def _convert(
    settings: Settings,
    semaphore: threading.BoundedSemaphore,
    converter: DoclingConverter,
    data: bytes,
    filename: str,
    suffix: str,
) -> Any:
    """Run one conversion inside a bounded worker thread.

    The semaphore is released by the thread itself, so a request that times out
    does not free its slot while the conversion is still running; concurrent
    uploads stay bounded even under client-side retries.
    """
    with semaphore:
        with tempfile.TemporaryDirectory(prefix="docling-parse-") as directory:
            source = Path(directory) / f"source{suffix}"
            source.write_bytes(data)
            return parse_document(converter, source, suffix)


async def _handle_parse(
    request: Request, file: UploadFile, options: str | None
) -> Response:
    app = request.app
    settings: Settings = app.state.settings
    started = time.monotonic()
    filename = file.filename or ""
    client_error = app.state.docling_error
    if client_error:
        return _error_response(503, "Docling 解析服务未正确配置")
    suffix_preview = _manifest_version(filename)
    if not suffix_preview:
        return _error_response(415, "仅支持 .docx、.html 和 .htm 文档")
    data = bytearray()
    try:
        while True:
            chunk = await file.read(UPLOAD_CHUNK_BYTES)
            if not chunk:
                break
            data.extend(chunk)
            if len(data) > settings.max_upload_bytes:
                return _error_response(400, "上传文件超过服务端大小限制")
    finally:
        await file.close()
    try:
        suffix = validate_upload(bytes(data), filename)
    except ParserUnsupportedError as error:
        return _error_response(415, str(error))
    except ParserInputError as error:
        return _error_response(400, str(error))
    except ParserConfigError as error:
        return _error_response(503, str(error))

    semaphore: threading.BoundedSemaphore = app.state.semaphore
    converter: DoclingConverter = app.state.converter
    task = asyncio.create_task(
        asyncio.to_thread(_convert, settings, semaphore, converter, bytes(data), filename, suffix)
    )
    try:
        output = await asyncio.wait_for(asyncio.shield(task), timeout=settings.parse_timeout)
    except asyncio.TimeoutError:
        # The worker thread keeps its slot until the conversion really stops;
        # the timed-out client retries from a clean staging attempt.
        LOGGER.warning(
            "parse timeout request_id=%s format=%s bytes=%d",
            _request_id(request), suffix, len(data),
        )
        return _error_response(504, "Docling 解析超时")
    except ParserError as error:
        status = 400 if isinstance(error, ParserInputError) else 503 if isinstance(error, ParserConfigError) else 500
        LOGGER.warning(
            "parse failed request_id=%s format=%s bytes=%d category=%s",
            _request_id(request), suffix, len(data), type(error).__name__,
        )
        return _error_response(status, str(error))
    except Exception:
        LOGGER.exception(
            "parse internal error request_id=%s format=%s bytes=%d",
            _request_id(request), suffix, len(data),
        )
        return _error_response(500, "Docling 解析服务内部错误")
    blocks = len(output.manifest.get("blocks") or [])
    assets = len(output.manifest.get("assets") or [])
    LOGGER.info(
        "parsed request_id=%s format=%s bytes=%d blocks=%d assets=%d images=%d duration_ms=%d",
        _request_id(request), suffix, len(data), blocks, assets, len(output.images),
        int((time.monotonic() - started) * 1000),
    )
    return _zip_response(output)


def create_app(settings: Settings | None = None, converter: DoclingConverter | None = None) -> FastAPI:
    """Build the ASGI app.

    ``settings``/``converter`` exist for tests; the module-level ``app`` used by
    uvicorn reads the environment.
    """
    settings = settings or Settings.from_env()
    selected = converter or DoclingConverter()
    docling_error = _docling_import_error()
    try:
        import docling  # noqa: F401

        version = selected.version()
    except Exception:
        version = "unavailable"
    if docling_error is None and version in {"", "unavailable"}:
        docling_error = "Docling 版本信息不可用"

    app = FastAPI(
        title="Praxis-RAG Docling service",
        summary="Docling-backed DOCX/static-HTML parsing microservice",
        version=__version__,
    )
    app.state.settings = settings
    app.state.converter = selected
    app.state.docling_error = docling_error
    app.state.semaphore = threading.BoundedSemaphore(settings.concurrency)

    @app.middleware("http")
    async def request_correlation(request: Request, call_next):
        request_id = _request_id_from_header(request.headers.get(REQUEST_ID_HEADER))
        request.state.request_id = request_id
        response = await call_next(request)
        response.headers[REQUEST_ID_HEADER] = request_id
        return response

    @app.get("/")
    async def root() -> dict[str, Any]:
        return _health(app)

    @app.get("/healthz")
    async def healthz() -> dict[str, Any]:
        return _health(app)

    @app.get("/health")
    async def health() -> dict[str, Any]:
        return _health(app)

    @app.post("/v1/parse")
    async def parse(
        request: Request,
        file: UploadFile = File(...),
        options: str | None = Form(None),
    ) -> Response:
        return await _handle_parse(request, file, options)

    @app.exception_handler(RequestValidationError)
    async def on_validation_error(
        request: Request, exc: RequestValidationError
    ) -> JSONResponse:
        # Never render exc.errors()/exc.body verbatim: those objects include the
        # submitted input values. Only field locations are safe.
        locations = sorted(
            {error.get("loc", ("-",))[-1] for error in exc.errors()}
        )
        return _error_response(422, "请求格式无效：" + ", ".join(str(part) for part in locations))

    return app


configure_logging()
app = create_app()


def main() -> None:
    """Run uvicorn using the environment configuration."""
    import uvicorn

    settings = Settings.from_env()
    uvicorn.run(
        "app.main:app",
        host=settings.host,
        port=settings.port,
        workers=settings.workers,
        log_level="debug" if _truthy(os.environ.get(DEBUG_TRACE_ENV)) else "info",
    )


if __name__ == "__main__":
    main()
