"""Expose the RAG orchestrator through a shared MCP server definition."""

from __future__ import annotations

import asyncio
import inspect
import json
import math
import os
import sys
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass
from typing import Any, Protocol
from uuid import UUID

from mcp import types
from mcp.server import Server
from mcp.server.lowlevel.server import NotificationOptions

from . import __version__
from .config import RagConfigError
from .contracts import RagOperationError, SearchRequest, SearchValidationError
from .catalog import NamespaceCatalog
from .orchestrator import build_default_orchestrator
from .service_knowledge import KnowledgeError
from .state.result_snapshots import SnapshotUnavailable

MAX_ARTIFACT_LENGTH = 1024 * 1024
MAX_RESULT_PAGE = 2**31 - 1


def validate_uuid(value: Any) -> str:
    """Accept only canonical lowercase dashed UUID strings."""
    if not isinstance(value, str) or len(value) != 36:
        raise SearchValidationError("Expected a dashed UUID string")
    try:
        canonical = str(UUID(value))
    except ValueError as exc:
        raise SearchValidationError("Expected a dashed UUID string") from exc
    if canonical != value:
        raise SearchValidationError("Expected a dashed UUID string")
    return canonical


def validate_search_result(arguments: dict[str, Any]) -> dict[str, Any]:
    if set(arguments) - {"result_id", "page", "page_size"}:
        raise SearchValidationError("Unknown search-result arguments")
    result_id = validate_uuid(arguments.get("result_id"))
    page = arguments.get("page", 1)
    page_size = arguments.get("page_size", 10)
    if type(page) is not int or not 1 <= page <= MAX_RESULT_PAGE:
        raise SearchValidationError(f"page must be an integer between 1 and {MAX_RESULT_PAGE}")
    if type(page_size) is not int or not 1 <= page_size <= 100:
        raise SearchValidationError("page_size must be an integer between 1 and 100")
    return {"result_id": result_id, "page": page, "page_size": page_size}


def _read_search_result(get_result, request):
    try:
        return get_result(**request)
    except SnapshotUnavailable:
        raise
    except ValueError as exc:
        raise SearchValidationError(str(exc)) from exc


def validate_artifact_range(offset: int, length: int) -> None:
    if type(offset) is not int or offset < 0:
        raise SearchValidationError("offset must be a non-negative integer")
    if type(length) is not int or not 1 <= length <= MAX_ARTIFACT_LENGTH:
        raise SearchValidationError("length must be between 1 and 1048576 bytes")


class SearchExecutor(Protocol):
    """Minimal boundary shared by local and remote retrieval implementations."""

    def search(self, request: SearchRequest) -> dict[str, object]: ...


SEARCH_TOOL_SCHEMA: dict[str, object] = {
    "type": "object",
    "properties": {
        "query": {"type": "string", "minLength": 1, "maxLength": 2000},
        "retrieval": {
            "type": "string",
            "enum": ["bm25", "embedding", "hybrid"],
            "default": "hybrid",
        },
        "rerank": {
            "type": "string",
            "enum": ["none", "cross_encoder"],
            "default": "cross_encoder",
        },
        "candidate_k": {"type": "integer", "minimum": 1, "maximum": 100, "default": 10},
        "top_k": {"type": "integer", "minimum": 1, "maximum": 10, "default": 5},
        "filters": {
            "type": "object",
            "properties": {
                "document_id": {"type": ["string", "number", "boolean"]},
                "source": {"type": ["string", "number", "boolean"]},
                "title": {"type": ["string", "number", "boolean"]},
            },
            "additionalProperties": False,
            "default": {},
        },
    },
    "required": ["query"],
    "additionalProperties": False,
}


@dataclass(frozen=True)
class QueuedSearchRequest:
    search: SearchRequest
    wait_seconds: float = 10

    @classmethod
    def from_arguments(cls, arguments):
        values = dict(arguments)
        wait = values.pop("wait_seconds", 10)
        if type(wait) not in (int, float) or not math.isfinite(wait) or not 0 <= wait <= 30:
            raise SearchValidationError("wait_seconds must be a finite number between 0 and 30")
        return cls(SearchRequest.from_arguments(values), float(wait))


def search_tool_definition(*, queued: bool = False) -> types.Tool:
    """Describe the stable action space exposed to MCP clients."""
    schema = SEARCH_TOOL_SCHEMA
    if queued:
        schema = {**schema, "properties": {**schema["properties"], "wait_seconds": {
            "type": "number", "minimum": 0, "maximum": 30, "default": 10,
            "description": "Maximum seconds to wait for this call (not the task deadline). Use 0 to enqueue without waiting.",
        }}}
    return types.Tool(
        name="search",
        description=(
            "Search user-uploaded/local knowledge for manuals, internal facts, exact identifiers, and evidence "
            "not already established in the conversation. Prefer hybrid + cross_encoder initially; use bm25 "
            "for exact codes/names and embedding for semantic paraphrases. Cite returned source and chunk_id. "
            "If evidence_status is no_evidence, rewrite the query once when useful, otherwise say the knowledge "
            "base is insufficient. Do not use this tool for greetings or unrelated general tasks."
            + (" Searches are queued. Set wait_seconds (0..30, default 10). If only result_id is returned, "
               "poll get_search_result with that UUID; status queued means still waiting or processing. "
               "Do not submit the same search again merely because the wait ended." if queued else "")
        ),
        input_schema=schema,
        annotations=types.ToolAnnotations(
            read_only_hint=True,
            destructive_hint=False,
            idempotent_hint=True,
            open_world_hint=False,
        ),
    )


def invoke_search(
    orchestrator: SearchExecutor,
    arguments: Mapping[str, Any] | None,
) -> types.CallToolResult:
    """Validate one MCP call and render a JSON observation for the Agent."""

    return create_registry(orchestrator).invoke("search", dict(arguments or {}))


def _trace(message: str) -> None:
    """Write secret-free stage diagnostics when explicitly enabled."""

    if os.getenv("RAG_DEBUG_TRACE", "").strip().lower() in {"1", "true", "yes"}:
        print(f"[rag-mcp] {message}", file=sys.stderr, flush=True)


@dataclass(frozen=True)
class RegisteredTool:
    definition: types.Tool
    validate: Callable[[dict[str, Any]], Any]
    execute: Callable[[Any], dict[str, object] | Awaitable[dict[str, object]]]


class ToolRegistry:
    def __init__(self, entries: list[RegisteredTool], catalog: NamespaceCatalog | None = None) -> None:
        self.entries = {entry.definition.name: entry for entry in entries}
        if len(self.entries) != len(entries):
            raise ValueError("Duplicate tool name")
        self.catalog = catalog

    def configured(self) -> list[dict[str, Any]]:
        overrides = self.catalog.snapshot().tools if self.catalog else {}
        return [
            {
                "name": name,
                "enabled": True,
                "description": entry.definition.description or "",
                **overrides.get(name, {}),
            }
            for name, entry in self.entries.items()
        ]

    def definitions(self) -> list[types.Tool]:
        return [
            self.entries[item["name"]].definition.model_copy(update={"description": item["description"]})
            for item in self.configured()
            if item["enabled"]
        ]

    def enabled_for_execution(self, name: str) -> bool:
        if name not in self.entries:
            return False
        if self.catalog is None:
            return True
        # Listing/templates may be eventually consistent; actual execution never
        # authorizes a disabled tool from a stale cache.
        return bool(self.catalog.snapshot(authoritative=True).tools.get(name, {}).get("enabled", True))

    def invoke(self, name: str, arguments: dict[str, Any]) -> types.CallToolResult:
        try:
            if not self.enabled_for_execution(name):
                raise SearchValidationError(f"Unknown or disabled RAG tool: {name}")
            entry = self.entries[name]
            request = entry.validate(arguments)
            _trace("tool execution started")
            payload = entry.execute(request)
            _trace("tool execution completed")
        except (RagConfigError, RagOperationError, SearchValidationError, KnowledgeError, SnapshotUnavailable, KeyError) as exc:
            _trace(f"tool failed: {type(exc).__name__}")
            return types.CallToolResult(content=[types.TextContent(type="text", text=str(exc))], is_error=True)
        return self.render(payload)

    async def invoke_async(self, name: str, arguments: dict[str, Any]) -> types.CallToolResult:
        entry = self.entries.get(name)
        if entry is None or not inspect.iscoroutinefunction(entry.execute):
            return await asyncio.to_thread(self.invoke, name, arguments)
        try:
            if not await asyncio.to_thread(self.enabled_for_execution, name):
                raise SearchValidationError(f"Unknown or disabled RAG tool: {name}")
            request = await asyncio.to_thread(entry.validate, arguments)
            payload = await entry.execute(request)
        except (RagConfigError, RagOperationError, SearchValidationError, KnowledgeError, SnapshotUnavailable, KeyError) as exc:
            return types.CallToolResult(content=[types.TextContent(type="text", text=str(exc))], is_error=True)
        return self.render(payload)

    @staticmethod
    def render(payload: dict[str, object]) -> types.CallToolResult:
        images = []
        public_payload = json.loads(json.dumps(payload, ensure_ascii=False))
        for result in public_payload.get("results", []):
            asset = result.get("asset") if isinstance(result, dict) else None
            if isinstance(asset, dict) and asset.get("type") == "image" and isinstance(asset.get("data"), str):
                images.append(types.ImageContent(type="image", data=asset.pop("data"), mime_type=str(asset["mime_type"])))
                asset["content_block"] = len(images)
        return types.CallToolResult(content=[
            types.TextContent(type="text", text=json.dumps(public_payload, ensure_ascii=False)), *images
        ])


def create_registry(executor: SearchExecutor, catalog: NamespaceCatalog | None = None, document_mutator=None) -> ToolRegistry:
    queued_search = getattr(executor, "search_queued", None)
    entries = [RegisteredTool(
        search_tool_definition(queued=callable(queued_search)),
        QueuedSearchRequest.from_arguments if callable(queued_search) else SearchRequest.from_arguments,
        queued_search if callable(queued_search) else executor.search,
    )]
    get_result = getattr(executor, "get_search_result", None)
    if callable(get_result):
        entries.append(RegisteredTool(
            types.Tool(
                name="get_search_result",
                description="Read a search by result_id UUID without running retrieval again. Pending queued searches return status queued and their stage; completed searches return frozen results. Failed, timed-out or expired searches return errors.",
                input_schema={
                    "type": "object",
                    "properties": {
                        "result_id": {"type": "string", "format": "uuid", "minLength": 36, "maxLength": 36},
                        "page": {"type": "integer", "minimum": 1, "maximum": MAX_RESULT_PAGE, "default": 1},
                        "page_size": {"type": "integer", "minimum": 1, "maximum": 100, "default": 10},
                    },
                    "required": ["result_id"],
                    "additionalProperties": False,
                },
                annotations=types.ToolAnnotations(
                    read_only_hint=True, destructive_hint=False, idempotent_hint=True, open_world_hint=False,
                ),
            ),
            validate_search_result,
            lambda request: _read_search_result(get_result, request),
        ))
    if document_mutator is not None:
        from .document_tools import document_tool_entries

        entries.extend(document_tool_entries(document_mutator))
    return ToolRegistry(entries, catalog)


class RagServer(Server):
    def get_capabilities(self, notification_options=None, experimental_capabilities=None, extensions=None, **kwargs):
        return super().get_capabilities(
            NotificationOptions(tools_changed=True, prompts_changed=True, resources_changed=True),
            experimental_capabilities,
            extensions,
            **kwargs,
        )


def create_server(orchestrator: SearchExecutor | None = None, catalog=None, document_mutator=None, **handlers) -> Server:
    registry = create_registry(orchestrator or build_default_orchestrator(), catalog, document_mutator)

    async def list_tools(ctx, params) -> types.ListToolsResult:
        return types.ListToolsResult(tools=await asyncio.to_thread(registry.definitions))

    async def call_tool(ctx, params: types.CallToolRequestParams) -> types.CallToolResult:
        # Cancelling the wait never terminates synchronous inference already running.
        return await registry.invoke_async(params.name, params.arguments or {})

    return RagServer(
        "Praxis-RAG",
        version=__version__,
        on_list_tools=list_tools,
        on_call_tool=call_tool,
        **handlers,
    )
