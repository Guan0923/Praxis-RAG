"""Standalone loopback HTTP service for search and knowledge management."""

from __future__ import annotations

import hmac
import os
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol

from fastapi import Depends, FastAPI, File, Header, HTTPException, UploadFile
from fastapi.responses import FileResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field

from .config import RagConfig, RagConfigError
from .contracts import RagOperationError, SearchValidationError
from .service_knowledge import (
    MAX_FILE_BYTES,
    MAX_FILES_PER_BATCH,
    SUPPORTED_EXTENSIONS,
    KnowledgeError,
    KnowledgeValidationError,
    RagServiceRuntime,
)

_NAMESPACE_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$")
_WEB_ROOT = Path(__file__).with_name("web")


class ServiceRuntime(Protocol):
    def status(self) -> dict[str, object]: ...
    def list_documents(self, namespace: str) -> list[dict[str, object]]: ...
    def upload(self, namespace: str, items: list[tuple[str, object]]) -> dict[str, object]: ...
    def delete(self, namespace: str, name: str) -> dict[str, object]: ...
    def search(self, namespace: str, arguments: dict[str, object]) -> dict[str, object]: ...


@dataclass(frozen=True)
class RagHttpSettings:
    host: str = "127.0.0.1"
    port: int = 8011
    data_root: Path = Path.home() / ".mini_agent-rag"
    token: str | None = None

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
        if host not in {"127.0.0.1", "localhost", "::1"} and token is None:
            raise RagConfigError("RAG_SERVICE_TOKEN is required when RAG_SERVICE_HOST is not loopback")
        return cls(
            host=host,
            port=port,
            data_root=Path(os.getenv("RAG_SERVICE_DATA_ROOT", str(Path.home() / ".mini_agent-rag"))),
            token=token,
        )


class SearchBody(BaseModel):
    query: str
    retrieval: str = "hybrid"
    rerank: str = "cross_encoder"
    candidate_k: int = 10
    top_k: int = 5
    filters: dict[str, str | int | float | bool] = Field(default_factory=dict)


def create_service_app(
    runtime: ServiceRuntime | None = None,
    *,
    settings: RagHttpSettings | None = None,
) -> FastAPI:
    resolved_settings = settings or RagHttpSettings.from_env()
    resolved_runtime = runtime or RagServiceRuntime(RagConfig.from_env(), resolved_settings.data_root)
    app = FastAPI(title="RAG MCP Service", version="0.2.0")
    app.state.runtime = resolved_runtime

    def tenant(
        authorization: str | None = Header(default=None),
        x_rag_namespace: str | None = Header(default=None),
    ) -> str:
        if resolved_settings.token:
            expected = f"Bearer {resolved_settings.token}"
            if authorization is None or not hmac.compare_digest(authorization, expected):
                raise HTTPException(status_code=401, detail="RAG service authentication failed.")
        namespace = (x_rag_namespace or "").strip()
        if not _NAMESPACE_PATTERN.fullmatch(namespace):
            raise HTTPException(status_code=400, detail="Invalid RAG namespace.")
        return namespace

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

    @app.post("/v1/documents")
    def upload_documents(
        files: list[UploadFile] = File(...),
        namespace: str = Depends(tenant),
    ) -> dict[str, object]:
        items = [(upload.filename or "", upload.file) for upload in files]
        try:
            return resolved_runtime.upload(namespace, items)
        except KnowledgeValidationError as exc:
            raise HTTPException(status_code=400, detail=str(exc)) from exc
        except KnowledgeError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        except Exception as exc:
            raise HTTPException(status_code=503, detail=f"RAG upload failed: {type(exc).__name__}") from exc

    @app.delete("/v1/documents/{name}")
    def delete_document(name: str, namespace: str = Depends(tenant)) -> dict[str, object]:
        try:
            return resolved_runtime.delete(namespace, name)
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


def main() -> int:
    import uvicorn

    settings = RagHttpSettings.from_env()
    app = create_service_app(settings=settings)
    runtime = app.state.runtime
    if getattr(runtime, "config", None) is not None and runtime.config.preload_model:
        runtime.warmup()
    uvicorn.run(app, host=settings.host, port=settings.port)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
