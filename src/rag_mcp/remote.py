"""Thin HTTP transport used by the reusable stdio MCP adapter."""

from __future__ import annotations

import os
from dataclasses import asdict, dataclass
from urllib.parse import urlparse

import httpx

from .contracts import RagOperationError, SearchRequest


@dataclass(frozen=True)
class RemoteRagSettings:
    """Connection settings kept outside the Agent-visible MCP schema."""

    url: str
    namespace: str = "default"
    token: str | None = None
    timeout_seconds: float = 120.0

    @classmethod
    def from_env(cls) -> RemoteRagSettings:
        url = os.getenv("RAG_SERVICE_URL", "").strip().rstrip("/")
        parsed = urlparse(url)
        if parsed.scheme not in {"http", "https"} or not parsed.hostname:
            raise RagOperationError("RAG_SERVICE_URL must be an absolute HTTP(S) URL")
        namespace = os.getenv("RAG_NAMESPACE", "default").strip() or "default"
        token = os.getenv("RAG_SERVICE_TOKEN", "").strip() or None
        raw_timeout = os.getenv("RAG_SERVICE_TIMEOUT_SECONDS", "120").strip()
        try:
            timeout = float(raw_timeout)
        except ValueError as exc:
            raise RagOperationError("RAG_SERVICE_TIMEOUT_SECONDS must be a number") from exc
        if timeout <= 0:
            raise RagOperationError("RAG_SERVICE_TIMEOUT_SECONDS must be positive")
        if parsed.hostname not in {"127.0.0.1", "localhost", "::1"} and token is None:
            raise RagOperationError("RAG_SERVICE_TOKEN is required for a non-loopback RAG_SERVICE_URL")
        return cls(url=url, namespace=namespace, token=token, timeout_seconds=timeout)


class RemoteRagOrchestrator:
    """Expose the remote service through the same search method as a local orchestrator."""

    def __init__(self, settings: RemoteRagSettings) -> None:
        self.settings = settings

    def search(self, request: SearchRequest) -> dict[str, object]:
        headers = {
            "Accept": "application/json",
            "X-RAG-Namespace": self.settings.namespace,
        }
        if self.settings.token:
            headers["Authorization"] = f"Bearer {self.settings.token}"
        try:
            response = httpx.post(
                f"{self.settings.url}/v1/search",
                headers=headers,
                json=asdict(request),
                timeout=self.settings.timeout_seconds,
            )
            response.raise_for_status()
            payload = response.json()
        except httpx.HTTPStatusError as exc:
            detail = _safe_detail(exc.response)
            raise RagOperationError(f"RAG Service HTTP {exc.response.status_code}: {detail}") from exc
        except (httpx.HTTPError, ValueError) as exc:
            raise RagOperationError(f"RAG Service request failed: {type(exc).__name__}") from exc
        if not isinstance(payload, dict):
            raise RagOperationError("RAG Service response must be a JSON object")
        return {str(key): value for key, value in payload.items()}


def _safe_detail(response: httpx.Response) -> str:
    try:
        payload = response.json()
    except ValueError:
        return "invalid response"
    detail = payload.get("detail") if isinstance(payload, dict) else None
    return str(detail)[:500] if isinstance(detail, str) else "request rejected"
