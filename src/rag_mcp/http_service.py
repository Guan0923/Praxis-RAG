"""Standalone loopback HTTP service for search and knowledge management."""

from __future__ import annotations

import hmac
import ipaddress
import os
import re
from contextlib import asynccontextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol
from typing import Literal
from urllib.parse import urlsplit

import anyio
from fastapi import Depends, FastAPI, File, Form, Header, HTTPException, Request, UploadFile
from fastapi.exceptions import RequestValidationError
from fastapi.exception_handlers import request_validation_exception_handler
from fastapi.responses import FileResponse, JSONResponse, Response
from fastapi.staticfiles import StaticFiles
from mcp.server.transport_security import TransportSecuritySettings
from mcp.server.subscriptions import PromptsListChanged, ToolsListChanged
from pydantic import BaseModel, ConfigDict, Field, SecretStr
from starlette.routing import Route

from . import __version__
from .config import RagConfig, RagConfigError
from .chunk_strategies import ChunkingConfig, strategy_catalog
from .catalog import PromptDefinition, ToolPatch
from .contracts import RagOperationError, RerankMode, SearchValidationError
from .mcp_http import McpHttpEndpoint
from .service_knowledge import (
    MAX_FILE_BYTES,
    MAX_FILES_PER_BATCH,
    SUPPORTED_EXTENSIONS,
    KnowledgeError,
    KnowledgeValidationError,
    RagServiceRuntime,
)
from .model_api import ModelAPIError

_NAMESPACE_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$")
_WEB_ROOT = Path(__file__).with_name("web")
_LOOPBACK_HOSTS = frozenset({"127.0.0.1", "localhost", "::1"})
_LOCAL_MCP_HOSTS = (
    "127.0.0.1",
    "127.0.0.1:*",
    "localhost",
    "localhost:*",
    "[::1]",
    "[::1]:*",
)
_LOCAL_MCP_ORIGINS = (
    "http://127.0.0.1",
    "http://127.0.0.1:*",
    "http://localhost",
    "http://localhost:*",
    "http://[::1]",
    "http://[::1]:*",
)


class ServiceRuntime(Protocol):
    def status(self) -> dict[str, object]: ...
    def model_settings(self) -> dict[str, object]: ...
    def model_action(self, kind: str, values: dict[str, object], action: str) -> dict[str, object]: ...
    def list_documents(self, namespace: str) -> list[dict[str, object]]: ...
    def upload(self, namespace: str, items: list[tuple[str, object]], chunking: ChunkingConfig | None = None) -> dict[str, object]: ...
    def check_documents(self, namespace: str, items: list[dict[str, str]], chunking: ChunkingConfig) -> dict[str, object]: ...
    def delete(self, namespace: str, name: str) -> dict[str, object]: ...
    def write_document(self, namespace: str, name: str, content: bytes, *, overwrite: bool) -> dict[str, object]: ...
    def search(self, namespace: str, arguments: dict[str, object]) -> dict[str, object]: ...
    def read_document(self, namespace: str, name: str) -> tuple[str, str]: ...
    def read_document_file(self, namespace: str, name: str) -> bytes: ...


@dataclass(frozen=True)
class RagHttpSettings:
    host: str = "127.0.0.1"
    port: int = 8011
    data_root: Path = Path.home() / ".mini_agent-rag"
    token: str | None = None
    admin_token: str | None = None
    mcp_allowed_hosts: tuple[str, ...] = _LOCAL_MCP_HOSTS
    mcp_allowed_origins: tuple[str, ...] = _LOCAL_MCP_ORIGINS

    @classmethod
    def from_env(cls) -> RagHttpSettings:
        raw_port = os.getenv("RAG_SERVICE_PORT", "8011").strip()
        try:
            port = int(raw_port)
        except ValueError as exc:
            raise RagConfigError("RAG_SERVICE_PORT must be an integer") from exc
        if not 1 <= port <= 65535:
            raise RagConfigError("RAG_SERVICE_PORT must be between 1 and 65535")
        token = os.getenv("RAG_SERVICE_TOKEN", "").strip() or None
        host = os.getenv("RAG_SERVICE_HOST", "127.0.0.1").strip() or "127.0.0.1"
        raw_allowed_hosts = os.getenv("RAG_MCP_ALLOWED_HOSTS")
        raw_allowed_origins = os.getenv("RAG_MCP_ALLOWED_ORIGINS")
        if host not in _LOOPBACK_HOSTS:
            if token is None:
                raise RagConfigError("RAG_SERVICE_TOKEN is required when RAG_SERVICE_HOST is not loopback")
            if not raw_allowed_hosts or not raw_allowed_hosts.strip():
                raise RagConfigError("RAG_MCP_ALLOWED_HOSTS is required for a non-loopback RAG service")
            if not raw_allowed_origins or not raw_allowed_origins.strip():
                raise RagConfigError("RAG_MCP_ALLOWED_ORIGINS is required for a non-loopback RAG service")
        return cls(
            host=host,
            port=port,
            data_root=Path(os.getenv("RAG_SERVICE_DATA_ROOT", str(Path.home() / ".mini_agent-rag"))),
            token=token,
            admin_token=os.getenv("RAG_ADMIN_TOKEN", "").strip() or None,
            mcp_allowed_hosts=_csv_setting(raw_allowed_hosts, _LOCAL_MCP_HOSTS),
            mcp_allowed_origins=_csv_setting(raw_allowed_origins, _LOCAL_MCP_ORIGINS),
        )


class ModelSettingsBody(BaseModel):
    model_config = ConfigDict(extra="forbid")
    base_url: str = Field(default="", max_length=2048)
    model: str = Field(default="", max_length=512)
    apikey: SecretStr = SecretStr("")

    def values(self) -> dict[str, object]:
        result = self.model_dump(exclude_unset=True)
        if "apikey" in result:
            result["apikey"] = self.apikey.get_secret_value()
        return result


def _local_model_request(request: Request) -> bool:
    if not request.client:
        return False
    try:
        if not ipaddress.ip_address(request.client.host).is_loopback:
            return False
        if request.url.hostname not in _LOOPBACK_HOSTS:
            return False
        if any(header in request.headers for header in (
            "forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "x-real-ip",
        )) or request.headers.get("sec-fetch-site") == "cross-site":
            return False
        if origin := request.headers.get("origin"):
            parsed = urlsplit(origin)
            port = parsed.port or (443 if parsed.scheme == "https" else 80)
            target_port = request.url.port or (443 if request.url.scheme == "https" else 80)
            if (parsed.scheme != request.url.scheme or parsed.hostname != request.url.hostname
                    or port != target_port or parsed.username or parsed.password):
                return False
        return True
    except ValueError:
        return False


class SearchBody(BaseModel):
    query: str
    retrieval: str = "hybrid"
    rerank: RerankMode = "cross_encoder"
    candidate_k: int = 10
    top_k: int = 5
    filters: dict[str, str | int | float | bool] = Field(default_factory=dict)


class CheckFile(BaseModel):
    name: str = Field(min_length=1, max_length=255)
    sha256: str = Field(pattern=r"^[a-f0-9]{64}$")


class CheckDocumentsBody(BaseModel):
    files: list[CheckFile] = Field(min_length=1, max_length=MAX_FILES_PER_BATCH)
    strategy: str = "recursive"
    chunk_size: int | None = Field(default=None, strict=True)
    chunk_overlap: int | None = Field(default=None, strict=True)


def _chunking_config(strategy: str, size: int | None, overlap: int | None) -> ChunkingConfig:
    try:
        return ChunkingConfig(strategy, size if size is not None else (700 if strategy == "character" else 512),
                              overlap if overlap is not None else (100 if strategy == "character" else 64))
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=str(exc)) from exc


def create_service_app(
    runtime: ServiceRuntime | None = None,
    *,
    settings: RagHttpSettings | None = None,
) -> FastAPI:
    resolved_settings = settings or RagHttpSettings.from_env()
    resolved_runtime = runtime or RagServiceRuntime(RagConfig.from_env(), resolved_settings.data_root)
    if resolved_settings.admin_token and resolved_settings.admin_token == resolved_settings.token:
        raise RagConfigError("RAG_ADMIN_TOKEN must differ from RAG_SERVICE_TOKEN")

    def matches(authorization, token):
        return authorization is not None and hmac.compare_digest(
            authorization.encode("utf-8"),
            f"Bearer {token}".encode("utf-8"),
        )

    def namespace_from_header(value):
        namespace = (value or "").strip()
        if not _NAMESPACE_PATTERN.fullmatch(namespace):
            raise HTTPException(status_code=400, detail="Invalid RAG namespace.")
        return namespace

    def tenant_from_headers(authorization: str | None, x_rag_namespace: str | None) -> str:
        if resolved_settings.admin_token and matches(authorization, resolved_settings.admin_token):
            raise HTTPException(status_code=401, detail="Use the service credential for ordinary requests.")
        if resolved_settings.token and not matches(authorization, resolved_settings.token):
            raise HTTPException(status_code=401, detail="RAG service authentication failed.")
        return namespace_from_header(x_rag_namespace)

    def tenant(
        authorization: str | None = Header(default=None),
        x_rag_namespace: str | None = Header(default=None),
    ) -> str:
        return tenant_from_headers(authorization, x_rag_namespace)

    mcp_endpoint = McpHttpEndpoint(
        resolved_runtime,
        tenant_from_headers,
        TransportSecuritySettings(
            allowed_hosts=list(resolved_settings.mcp_allowed_hosts),
            allowed_origins=list(resolved_settings.mcp_allowed_origins),
        ),
        resolved_settings.data_root,
    )

    @asynccontextmanager
    async def lifespan(_app: FastAPI):
        async with mcp_endpoint.run():
            yield

    app = FastAPI(title="RAG MCP Service", version=__version__, lifespan=lifespan)
    app.state.runtime = resolved_runtime
    app.state.mcp_endpoint = mcp_endpoint
    app.router.routes.append(Route("/mcp", endpoint=mcp_endpoint, methods=["GET", "POST", "DELETE"]))

    @app.exception_handler(RequestValidationError)
    async def validation_error(request, error):
        if request.url.path.startswith("/v1/model-settings"):
            return JSONResponse({"detail": "模型设置参数无效"}, status_code=422)
        return await request_validation_exception_handler(request, error)

    @app.exception_handler(ModelAPIError)
    async def model_api_error(_request, error):
        return JSONResponse({"detail": str(error)}, status_code=400)

    def model_auth(authorization: str | None = Header(default=None)) -> None:
        tenant_from_headers(authorization, "default")

    def local_model_auth(request: Request, _auth=Depends(model_auth)) -> None:
        if not _local_model_request(request):
            raise HTTPException(status_code=403, detail="仅允许本机直接访问时修改或测试模型设置")

    @app.get("/v1/model-settings")
    def model_settings(request: Request, _auth=Depends(model_auth)):
        return {"models": resolved_runtime.model_settings(), "writable": _local_model_request(request)}

    @app.patch("/v1/model-settings/{kind}")
    def save_model_settings(kind: Literal["embedding", "reranker"], body: ModelSettingsBody,
                            _auth=Depends(local_model_auth)):
        return resolved_runtime.model_action(kind, body.values(), "save")

    @app.post("/v1/model-settings/{kind}/models")
    def model_list(kind: Literal["embedding", "reranker"], body: ModelSettingsBody,
                   _auth=Depends(local_model_auth)):
        return resolved_runtime.model_action(kind, body.values(), "models")

    @app.post("/v1/model-settings/{kind}/test")
    def model_test(kind: Literal["embedding", "reranker"], body: ModelSettingsBody,
                   _auth=Depends(local_model_auth)):
        return resolved_runtime.model_action(kind, body.values(), "test")

    @app.exception_handler(KnowledgeError)
    async def knowledge_error(_request, _error):
        return JSONResponse({"detail": "Knowledge or MCP configuration operation failed."}, status_code=503)

    def admin(
        authorization: str | None = Header(default=None),
        x_rag_namespace: str | None = Header(default=None),
    ) -> str:
        if not resolved_settings.admin_token:
            raise HTTPException(status_code=403, detail="MCP administration is disabled.")
        if not matches(authorization, resolved_settings.admin_token):
            raise HTTPException(status_code=401, detail="MCP administration authentication failed.")
        return namespace_from_header(x_rag_namespace)

    @app.get("/v1/admin/tools")
    async def admin_tools(namespace: str = Depends(admin)):
        return {"tools": await anyio.to_thread.run_sync(mcp_endpoint.registry(namespace).configured)}

    @app.patch("/v1/admin/tools/{name}")
    async def patch_tool(name: str, body: ToolPatch, namespace: str = Depends(admin)):
        registry = mcp_endpoint.registry(namespace)
        entry = registry.entries.get(name)
        if entry is None:
            raise HTTPException(status_code=404, detail="Unknown registered tool.")
        defaults = {"enabled": True, "description": entry.definition.description or ""}
        async with mcp_endpoint.mutation_lock(namespace):
            with anyio.CancelScope(shield=True):
                changed = await anyio.to_thread.run_sync(registry.catalog.patch_tool, name, body, defaults)
                if changed:
                    await mcp_endpoint.publish(namespace, ToolsListChanged())
                tools = await anyio.to_thread.run_sync(registry.configured)
        return {"tool": next(item for item in tools if item["name"] == name), "changed": changed}

    @app.get("/v1/admin/prompts")
    async def admin_prompts(namespace: str = Depends(admin)):
        data = await anyio.to_thread.run_sync(mcp_endpoint.catalogs.for_namespace(namespace).snapshot)
        return {"prompts": [{"name": name, **prompt.model_dump()} for name, prompt in sorted(data.prompts.items())]}

    @app.put("/v1/admin/prompts/{name}")
    async def put_prompt(name: str, body: PromptDefinition, namespace: str = Depends(admin)):
        async with mcp_endpoint.mutation_lock(namespace):
            with anyio.CancelScope(shield=True):
                try:
                    changed = await anyio.to_thread.run_sync(
                        mcp_endpoint.catalogs.for_namespace(namespace).put_prompt,
                        name,
                        body,
                    )
                except ValueError as exc:
                    raise HTTPException(status_code=400, detail=str(exc)) from exc
                if changed:
                    await mcp_endpoint.publish(namespace, PromptsListChanged())
        return {"name": name, "changed": changed}

    @app.delete("/v1/admin/prompts/{name}")
    async def delete_prompt(name: str, namespace: str = Depends(admin)):
        async with mcp_endpoint.mutation_lock(namespace):
            with anyio.CancelScope(shield=True):
                try:
                    await anyio.to_thread.run_sync(mcp_endpoint.catalogs.for_namespace(namespace).delete_prompt, name)
                except KeyError as exc:
                    raise HTTPException(status_code=404, detail="Unknown prompt.") from exc
                except ValueError as exc:
                    raise HTTPException(status_code=400, detail=str(exc)) from exc
                await mcp_endpoint.publish(namespace, PromptsListChanged())
        return {"deleted": name}

    @app.get("/health")
    def health() -> dict[str, object]:
        return resolved_runtime.status()

    if _WEB_ROOT.is_dir():
        app.mount("/console/assets", StaticFiles(directory=_WEB_ROOT), name="rag-console-assets")

        @app.get("/", include_in_schema=False)
        @app.get("/console", include_in_schema=False)
        def console() -> FileResponse:
            return FileResponse(_WEB_ROOT / "index.html")

    @app.get("/v1/documents")
    def list_documents(namespace: str = Depends(tenant)) -> dict[str, object]:
        return {
            "documents": resolved_runtime.list_documents(namespace),
            "supported_extensions": sorted(SUPPORTED_EXTENSIONS),
            "max_files_per_batch": MAX_FILES_PER_BATCH,
            "max_file_bytes": MAX_FILE_BYTES,
        }

    @app.get("/v1/documents/{name}/content")
    def document_content(name: str, namespace: str = Depends(tenant)) -> Response:
        try:
            if Path(name).suffix.casefold() == ".pdf":
                content = resolved_runtime.read_document_file(namespace, name)
                media_type = "application/pdf"
            else:
                content, _version = resolved_runtime.read_document(namespace, name)
                media_type = "text/plain"
            return Response(content, media_type=media_type, headers={
                "Cache-Control": "no-store",
                "X-Content-Type-Options": "nosniff",
            })
        except KnowledgeValidationError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc
        except (KnowledgeError, OSError) as exc:
            raise HTTPException(status_code=503, detail="Document content could not be read.") from exc

    @app.get("/v1/chunking/strategies")
    def chunking_strategies(namespace: str = Depends(tenant)) -> dict[str, object]:
        return strategy_catalog()

    @app.post("/v1/documents/check")
    def check_documents(body: CheckDocumentsBody, namespace: str = Depends(tenant)) -> dict[str, object]:
        configuration = _chunking_config(body.strategy, body.chunk_size, body.chunk_overlap)
        try:
            return resolved_runtime.check_documents(namespace, [item.model_dump() for item in body.files], configuration)
        except KnowledgeValidationError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        except KnowledgeError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc

    @app.post("/v1/documents")
    async def upload_documents(
        files: list[UploadFile] = File(...),
        strategy: str = Form("recursive"),
        chunk_size: int | None = Form(None),
        chunk_overlap: int | None = Form(None),
        namespace: str = Depends(tenant),
    ) -> dict[str, object]:
        configuration = _chunking_config(strategy, chunk_size, chunk_overlap)
        items = [(upload.filename or "", upload.file) for upload in files]
        try:
            async with mcp_endpoint.mutation_lock(namespace):
                with anyio.CancelScope(shield=True):
                    before = await anyio.to_thread.run_sync(resolved_runtime.list_documents, namespace)
                    result = await anyio.to_thread.run_sync(resolved_runtime.upload, namespace, items, configuration)
                    await mcp_endpoint.documents_changed(namespace, before, result["documents"])
                    return result
        except KnowledgeValidationError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        except KnowledgeError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        except Exception as exc:
            raise HTTPException(status_code=503, detail=f"RAG upload failed: {type(exc).__name__}") from exc

    @app.delete("/v1/documents/{name}")
    async def delete_document(name: str, namespace: str = Depends(tenant)) -> dict[str, object]:
        try:
            async with mcp_endpoint.mutation_lock(namespace):
                with anyio.CancelScope(shield=True):
                    before = await anyio.to_thread.run_sync(resolved_runtime.list_documents, namespace)
                    result = await anyio.to_thread.run_sync(resolved_runtime.delete, namespace, name)
                    await mcp_endpoint.documents_changed(namespace, before, result["documents"])
                    return result
        except KnowledgeValidationError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc
        except KnowledgeError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        except Exception as exc:
            raise HTTPException(status_code=503, detail=f"RAG deletion failed: {type(exc).__name__}") from exc

    @app.post("/v1/search")
    def search(body: SearchBody, namespace: str = Depends(tenant)) -> dict[str, object]:
        try:
            return resolved_runtime.search(namespace, body.model_dump())
        except SearchValidationError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        except (KnowledgeError, RagOperationError) as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        except Exception as exc:
            raise HTTPException(status_code=503, detail=f"RAG search failed: {type(exc).__name__}") from exc

    return app


def _csv_setting(raw: str | None, default: tuple[str, ...]) -> tuple[str, ...]:
    if raw is None:
        return default
    values = tuple(item.strip() for item in raw.split(",") if item.strip())
    if not values:
        raise RagConfigError("MCP host and origin allowlists must not be empty")
    return values


def main() -> int:
    import uvicorn

    settings = RagHttpSettings.from_env()
    app = create_service_app(settings=settings)
    uvicorn.run(app, host=settings.host, port=settings.port, timeout_graceful_shutdown=5, proxy_headers=False)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
