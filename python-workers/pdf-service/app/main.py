"""FastAPI application for the Praxis-RAG Docling PDF microservice.

Endpoints
---------
``GET /healthz``
    Liveness/configuration probe. Always answers 200 so a container healthcheck
    stays useful while the operator fixes a configuration problem; the body
    reports ``status``, the selected converter and any ``config_error``.

``POST /v1/parse``
    ``multipart/form-data`` field ``file`` carrying one PDF plus the optional
    text field ``options`` (JSON) with per-request VLM/OCR overrides. On success
    the response is ``application/zip`` with ``manifest.json`` at the archive
    root and every picture asset at its manifest-relative path (for example
    ``pictures/picture-0.png``). Failures are JSON ``{"detail": "<safe text>"}``
    that never quotes document content or request secrets.

The Go backend downloads the zip into its parser staging directory and feeds
``manifest.json`` to ``document.BuildPDFContent``; the zip layout is therefore
part of the contract and is validated by :func:`app.parser.validate_manifest`.
"""

from __future__ import annotations

import asyncio
import io
import json
import logging
import os
import re
import shutil
import sys
import tempfile
import zipfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from uuid import uuid4

from fastapi import FastAPI, File, Form, HTTPException, Request, UploadFile
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, Response

from . import SERVICE_NAME, __version__
from .parser import (
    SERVICE_PARSER_VERSION,
    ParserConfigError,
    ParserConversionError,
    ParserError,
    ParserOptionsError,
    ParseOptions,
    PdfConverter,
    VlmSettings,
    build_converter,
    parse_options_payload,
    validate_manifest,
    vlm_settings_from_env,
)

LOGGER = logging.getLogger(SERVICE_NAME)

DEFAULT_HOST = "0.0.0.0"
DEFAULT_PORT = 8091
DEFAULT_TIMEOUT = 900.0
DEFAULT_WORKERS = 1
DEFAULT_CONCURRENCY = 1
DEFAULT_MAX_UPLOAD_BYTES = 100 * 1024 * 1024
UPLOAD_CHUNK_BYTES = 1024 * 1024
PDF_HEADER_WINDOW = 1024

#: Structured logging knobs. ``RAG_DEBUG_TRACE`` stays the shared debug switch
#: with the Go service when the level variable is unset.
LOG_LEVEL_ENV = "RAG_PDF_SERVICE_LOG_LEVEL"
LOG_FORMAT_ENV = "RAG_PDF_SERVICE_LOG_FORMAT"
DEBUG_TRACE_ENV = "RAG_DEBUG_TRACE"
REQUEST_ID_HEADER = "X-Request-ID"

#: Bounded, log-safe request ids: no whitespace or control characters can forge
#: a log record. Invalid inbound values are replaced with a generated id.
_REQUEST_ID_PATTERN = re.compile(r"^[A-Za-z0-9._:-]{1,64}$")

_TRUTHY_VALUES = {"1", "true", "yes", "on"}
_LOG_LEVELS = {
    "debug": logging.DEBUG,
    "info": logging.INFO,
    "warn": logging.WARNING,
    "warning": logging.WARNING,
    "error": logging.ERROR,
}


def _truthy(value: str | None) -> bool:
    return (value or "").strip().lower() in _TRUTHY_VALUES


def _log_level_from_env() -> int:
    """Resolve the configured log level, falling back to the debug switch."""
    raw = os.environ.get(LOG_LEVEL_ENV, "").strip().lower()
    if not raw:
        return logging.DEBUG if _truthy(os.environ.get(DEBUG_TRACE_ENV)) else logging.INFO
    level = _LOG_LEVELS.get(raw)
    if level is None:
        raise ValueError(
            f"{LOG_LEVEL_ENV} must be one of debug, info, warning, error (got {raw!r})"
        )
    return level


def _log_format_from_env() -> str:
    """Resolve the log handler encoding: text (default) or json."""
    raw = os.environ.get(LOG_FORMAT_ENV, "").strip().lower()
    if not raw:
        return "text"
    if raw not in {"text", "json"}:
        raise ValueError(f"{LOG_FORMAT_ENV} must be one of text, json (got {raw!r})")
    return raw


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


def configure_logging() -> None:
    """Install one process-wide handler matching the Go service conventions."""
    level = _log_level_from_env()
    log_format = _log_format_from_env()
    handler = logging.StreamHandler(stream=sys.stderr)
    if log_format == "json":
        handler.setFormatter(_JsonLogFormatter())
    else:
        handler.setFormatter(
            logging.Formatter("%(asctime)s %(levelname)s %(name)s %(message)s")
        )
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
    """Return the request correlation id, empty outside the HTTP middleware."""
    return getattr(request.state, "request_id", "") or "-"

#: Error messages deliberately contain no document text: the Go backend shows
#: ``detail`` to users, so it must stay safe for arbitrary uploads.
INVALID_PDF_MESSAGE = "上传的文件不是有效的 PDF 文档"
EMPTY_FILE_MESSAGE = "上传的 PDF 文件为空"
TOO_LARGE_MESSAGE = "PDF 文件超过服务端大小限制"
UNKNOWN_CONVERTER_MESSAGE = "PDF 解析服务未正确配置"


@dataclass(frozen=True)
class Settings:
    """Runtime configuration read from the environment."""

    host: str = DEFAULT_HOST
    port: int = DEFAULT_PORT
    models_path: Path | None = None
    timeout: float = DEFAULT_TIMEOUT
    workers: int = DEFAULT_WORKERS
    concurrency: int = DEFAULT_CONCURRENCY
    converter: str = "docling"
    max_upload_bytes: int = DEFAULT_MAX_UPLOAD_BYTES
    vlm: VlmSettings | None = None
    #: ``RAG_VLM_*`` configuration problem (partial/invalid VLM settings). A
    #: complete per-request ``options.vlm`` overrides it, so it is reported
    #: separately from :attr:`non_vlm_config_error`.
    vlm_error: str | None = None
    #: Deployment problems a request cannot fix (converter, port, ...).
    non_vlm_config_error: str | None = None
    #: Union of every configuration problem; shown by ``/healthz``.
    config_error: str | None = None

    @classmethod
    def from_env(cls) -> "Settings":
        errors: list[str] = []

        converter = (os.environ.get("RAG_PDF_SERVICE_CONVERTER", "docling").strip().lower()
                     or "docling")
        if converter not in {"docling", "fake"}:
            errors.append(
                f"RAG_PDF_SERVICE_CONVERTER must be 'docling' or 'fake', got {converter!r}"
            )

        host = os.environ.get("RAG_PDF_SERVICE_HOST", DEFAULT_HOST).strip() or DEFAULT_HOST
        port = _int_env("PORT", DEFAULT_PORT, minimum=1, errors=errors)
        timeout = _float_env("RAG_PDF_SERVICE_TIMEOUT", DEFAULT_TIMEOUT, minimum=0.001, errors=errors)
        workers = _int_env("RAG_PDF_SERVICE_WORKERS", DEFAULT_WORKERS, minimum=1, errors=errors)
        concurrency = _int_env("RAG_PDF_SERVICE_CONCURRENCY", DEFAULT_CONCURRENCY, minimum=1,
                               errors=errors)
        max_upload_bytes = _int_env("RAG_PDF_SERVICE_MAX_BYTES", DEFAULT_MAX_UPLOAD_BYTES,
                                    minimum=1, errors=errors)

        models_raw = os.environ.get("RAG_PDF_MODELS_PATH", "").strip()
        models_path = Path(models_raw).expanduser() if models_raw else None

        vlm: VlmSettings | None = None
        vlm_error: str | None = None
        try:
            vlm = vlm_settings_from_env()
        except ParserConfigError as error:
            vlm_error = str(error)

        non_vlm_error = "; ".join(errors) or None
        config_errors = errors + ([vlm_error] if vlm_error else [])
        return cls(
            host=host,
            port=port,
            models_path=models_path,
            timeout=timeout,
            workers=workers,
            concurrency=concurrency,
            converter=converter,
            max_upload_bytes=max_upload_bytes,
            vlm=vlm,
            vlm_error=vlm_error,
            non_vlm_config_error=non_vlm_error,
            config_error="; ".join(config_errors) or None,
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


class _UnavailableConverter:
    """Placeholder returned when the configured converter name is invalid."""

    name = "unavailable"

    def __init__(self, message: str) -> None:
        self.message = message

    def convert(
        self,
        source: Path,
        output_dir: Path,
        scratch_dir: Path,
        *,
        options: ParseOptions | None = None,
    ) -> dict[str, Any]:
        raise ParserConfigError(self.message)


def create_app(
    settings: Settings | None = None,
    converter: PdfConverter | None = None,
) -> FastAPI:
    """Build the ASGI app.

    ``settings``/``converter`` exist for tests; the module-level ``app`` used by
    uvicorn reads the environment.
    """
    settings = settings or Settings.from_env()
    if converter is not None:
        selected: PdfConverter = converter
    else:
        try:
            selected = build_converter(
                settings.converter, models_path=settings.models_path, vlm=settings.vlm
            )
        except ParserConfigError as error:
            selected = _UnavailableConverter(str(error))  # type: ignore[assignment]

    app = FastAPI(
        title="Praxis-RAG PDF service",
        summary="Docling PDF parsing microservice",
        version=__version__,
    )
    app.state.settings = settings
    app.state.converter = selected
    app.state.semaphore = asyncio.Semaphore(settings.concurrency)

    @app.middleware("http")
    async def request_correlation(request: Request, call_next):
        """Attach one safe correlation id and echo it to the caller."""
        request_id = _request_id_from_header(request.headers.get(REQUEST_ID_HEADER))
        request.state.request_id = request_id
        response = await call_next(request)
        response.headers[REQUEST_ID_HEADER] = request_id
        return response

    @app.get("/")
    async def root() -> dict[str, Any]:
        return _service_info(app)

    @app.get("/healthz")
    async def healthz() -> dict[str, Any]:
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
        # submitted input values. Only field locations and error kinds are safe.
        errors = exc.errors()
        locations = sorted(
            {
                ".".join(str(part) for part in error.get("loc", ())) or "-"
                for error in errors
            }
        )
        kinds = sorted({str(error.get("type", "unknown")) for error in errors})
        LOGGER.warning(
            "Rejected invalid /v1/parse request request_id=%s error_count=%d fields=%s kinds=%s",
            _request_id(request),
            len(errors),
            ",".join(locations),
            ",".join(kinds),
        )
        return JSONResponse(
            status_code=422,
            content={
                "detail": "请求无效：必须以 multipart/form-data 提交一个名为 file 的 PDF 文件，"
                "options 必须是文本字段"
            },
        )

    @app.exception_handler(Exception)
    async def on_unhandled(request: Request, exc: Exception) -> JSONResponse:
        # The exception message and traceback may quote request input, so only
        # the class name is logged.
        LOGGER.error(
            "Unhandled error while serving %s request_id=%s error_type=%s",
            request.url.path,
            _request_id(request),
            type(exc).__name__,
        )
        return JSONResponse(status_code=500, content={"detail": "PDF 解析服务内部错误，请查看服务端日志"})

    return app


# ---------------------------------------------------------------------------
# Service metadata
# ---------------------------------------------------------------------------


def _docling_installed() -> bool:
    try:
        import importlib.util

        return importlib.util.find_spec("docling") is not None
    except Exception:  # a broken install must not break healthz
        return False


def _service_info(app: FastAPI) -> dict[str, Any]:
    settings: Settings = app.state.settings
    return {
        "service": SERVICE_NAME,
        "version": __version__,
        "parser_version": SERVICE_PARSER_VERSION,
        "converter": getattr(app.state.converter, "name", "unknown"),
        "converter_requested": settings.converter,
        "models_configured": settings.models_path is not None,
        "vlm_configured": settings.vlm is not None,
        "max_upload_bytes": settings.max_upload_bytes,
        "timeout_seconds": settings.timeout,
        "concurrency": settings.concurrency,
        "config_error": settings.config_error,
    }


def _health(app: FastAPI) -> dict[str, Any]:
    info = _service_info(app)
    info["status"] = "degraded" if info["converter"] == "unavailable" else "ok"
    info["docling_installed"] = _docling_installed()
    return info


# ---------------------------------------------------------------------------
# Parsing request
# ---------------------------------------------------------------------------


async def _handle_parse(
    request: Request, file: UploadFile, raw_options: str | None = None
) -> Response:
    app = request.app
    settings: Settings = app.state.settings
    # Parse options before touching the upload: a bad payload is a cheap 422 and
    # the raw text (which may carry an API key) is never logged or echoed.
    try:
        options = parse_options_payload(raw_options)
    except ParserOptionsError as error:
        LOGGER.warning("Rejected /v1/parse options: %s", error)
        raise HTTPException(status_code=422, detail=str(error)) from error

    if getattr(app.state.converter, "name", "") != "fake":
        # A complete per-request VLM overrides a broken RAG_VLM_* configuration;
        # deployment problems (bad converter name, invalid PORT, ...) always 503.
        blocker = settings.config_error
        if blocker and settings.vlm_error and options.vlm is not None:
            blocker = settings.non_vlm_config_error
        if blocker:
            raise HTTPException(
                status_code=503, detail=f"{UNKNOWN_CONVERTER_MESSAGE}: {blocker}"
            )

    workdir = Path(tempfile.mkdtemp(prefix="rag-pdf-service-"))
    try:
        source = workdir / "source.pdf"
        size = await _store_upload(file, source, settings.max_upload_bytes)
        if size == 0:
            raise HTTPException(status_code=400, detail=EMPTY_FILE_MESSAGE)
        if not _looks_like_pdf(source):
            raise HTTPException(status_code=400, detail=INVALID_PDF_MESSAGE)

        payload = await _run_conversion(
            app, source, workdir, options, request_id=_request_id(request)
        )
        return Response(
            content=payload,
            media_type="application/zip",
            headers={
                "Content-Disposition": 'attachment; filename="parse.zip"',
                "X-PDF-Parser-Version": SERVICE_PARSER_VERSION,
                "X-PDF-Converter": str(getattr(app.state.converter, "name", "unknown")),
            },
        )
    finally:
        await file.close()
        shutil.rmtree(workdir, ignore_errors=True)


async def _store_upload(file: UploadFile, destination: Path, max_bytes: int) -> int:
    size = 0
    try:
        with destination.open("wb") as handle:
            while True:
                chunk = await file.read(UPLOAD_CHUNK_BYTES)
                if not chunk:
                    break
                size += len(chunk)
                if size > max_bytes:
                    raise HTTPException(
                        status_code=413,
                        detail=f"{TOO_LARGE_MESSAGE}（上限 {max_bytes // (1024 * 1024)} MB）",
                    )
                handle.write(chunk)
    except HTTPException:
        raise
    return size


def _looks_like_pdf(source: Path) -> bool:
    with source.open("rb") as handle:
        header = handle.read(PDF_HEADER_WINDOW)
    return b"%PDF-" in header


async def _run_conversion(
    app: FastAPI,
    source: Path,
    workdir: Path,
    options: ParseOptions | None = None,
    *,
    request_id: str = "",
) -> bytes:
    """Queue and run one conversion in a worker thread.

    The semaphore bounds concurrent Docling conversions and the request timeout
    bounds both the queue wait and the conversion itself. Docling cannot be
    interrupted, so a timed-out conversion keeps its semaphore slot until the
    worker thread returns; the request already answered 504 and the worker's
    result is discarded (the temporary directory may already be removed, so the
    worker can fail after the response was sent).
    """
    settings: Settings = app.state.settings
    semaphore: asyncio.Semaphore = app.state.semaphore
    loop = asyncio.get_running_loop()
    converter: PdfConverter = app.state.converter
    scratch = workdir / "scratch"
    artifacts = workdir / "artifacts"
    requested = options if options is not None else ParseOptions()

    try:
        await asyncio.wait_for(semaphore.acquire(), timeout=settings.timeout)
    except asyncio.TimeoutError as error:
        raise HTTPException(
            status_code=503,
            detail="PDF 解析服务繁忙（等待队列超时），请稍后重试",
        ) from error

    released = False

    def release() -> None:
        nonlocal released
        if not released:
            released = True
            semaphore.release()

    def work() -> bytes:
        try:
            manifest = converter.convert(source, artifacts, scratch, options=requested)
            validate_manifest(manifest, artifacts)
            (artifacts / "manifest.json").write_text(
                json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8"
            )
            return _archive_artifacts(artifacts)
        finally:
            loop.call_soon_threadsafe(release)

    future = loop.run_in_executor(None, work)
    try:
        return await asyncio.wait_for(future, timeout=settings.timeout)
    except asyncio.TimeoutError as error:
        raise HTTPException(
            status_code=504,
            detail="PDF 解析超时，请稍后重试或拆分文档",
        ) from error
    except ParserOptionsError as error:
        raise HTTPException(status_code=422, detail=str(error)) from error
    except ParserConfigError as error:
        raise HTTPException(status_code=503, detail=str(error)) from error
    except ParserConversionError as error:
        raise HTTPException(status_code=500, detail=str(error)) from error
    except ParserError as error:
        raise HTTPException(status_code=500, detail=str(error)) from error
    except Exception as error:
        # The exception message/traceback may embed document text, so only the
        # exception class name reaches the log sink.
        LOGGER.error(
            "Unexpected PDF conversion failure request_id=%s error_type=%s",
            request_id or "-",
            type(error).__name__,
        )
        raise HTTPException(
            status_code=500,
            detail="PDF 解析失败，服务端日志包含详细信息",
        ) from error


def _archive_artifacts(artifacts: Path) -> bytes:
    """Zip ``manifest.json`` plus every referenced asset, and nothing else."""
    manifest_path = artifacts / "manifest.json"
    if not manifest_path.is_file():
        raise ParserConversionError("Parser produced no manifest.json")
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, "w", zipfile.ZIP_DEFLATED) as archive:
        archive.write(manifest_path, "manifest.json")
        for path in sorted(artifacts.rglob("*")):
            if path.is_file() and path != manifest_path:
                archive.write(path, path.relative_to(artifacts).as_posix())
    return buffer.getvalue()


# Uvicorn's multiprocess launcher re-imports this module in every worker, so
# the handler setup must happen at import time as well: with log_config=None the
# workers keep this root configuration instead of installing per-process uvicorn
# handlers (and losing the "Started server process" diagnostics).
configure_logging()
app = create_app()


def main() -> None:
    """Start the configured Uvicorn processes on one shared listening port."""
    import uvicorn

    configure_logging()
    settings = Settings.from_env()
    # An import string is required for Uvicorn's multiprocess launcher; passing
    # the app object would prevent workers > 1 from starting. log_config=None
    # keeps the root handler installed above (same format/level for uvicorn's
    # own loggers) instead of letting uvicorn replace it per worker.
    uvicorn.run(
        "app.main:app",
        host=settings.host,
        port=settings.port,
        workers=settings.workers,
        log_config=None,
    )


if __name__ == "__main__":
    main()
