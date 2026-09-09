"""Expose the RAG orchestrator through a shared MCP server definition."""

from __future__ import annotations

import asyncio
import json
import os
import sys
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any, Protocol

from mcp import types
from mcp.server import Server
from mcp.server.lowlevel.server import NotificationOptions

from . import __version__
from .config import RagConfigError
from .contracts import RagOperationError, SearchRequest, SearchValidationError
from .catalog import NamespaceCatalog
from .orchestrator import build_default_orchestrator


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
            "enum": ["none", "cross_encoder", "bi_encoder"],
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


def search_tool_definition() -> types.Tool:
    """Describe the stable action space exposed to Mini-Agent."""

    return types.Tool(
        name="search",
        description=(
            "Search user-uploaded/local knowledge for manuals, internal facts, exact identifiers, and evidence "
            "not already established in the conversation. Prefer hybrid + cross_encoder initially; use bm25 "
            "for exact codes/names and embedding for semantic paraphrases. Cite returned source and chunk_id. "
            "If evidence_status is no_evidence, rewrite the query once when useful, otherwise say the knowledge "
            "base is insufficient. Do not use this tool for greetings or unrelated general tasks."
        ),
        input_schema=SEARCH_TOOL_SCHEMA,
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
    execute: Callable[[Any], dict[str, object]]


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

    def invoke(self, name: str, arguments: dict[str, Any]) -> types.CallToolResult:
        try:
            if name not in {item.name for item in self.definitions()}:
                raise SearchValidationError(f"Unknown or disabled RAG tool: {name}")
            entry = self.entries[name]
            request = entry.validate(arguments)
            _trace("tool execution started")
            payload = entry.execute(request)
            _trace("tool execution completed")
        except (RagConfigError, RagOperationError, SearchValidationError, KeyError) as exc:
            _trace(f"tool failed: {type(exc).__name__}")
            return types.CallToolResult(content=[types.TextContent(type="text", text=str(exc))], is_error=True)
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


def create_registry(executor: SearchExecutor, catalog: NamespaceCatalog | None = None) -> ToolRegistry:
    return ToolRegistry(
        [RegisteredTool(search_tool_definition(), SearchRequest.from_arguments, executor.search)], catalog
    )


class RagServer(Server):
    def get_capabilities(self, notification_options=None, experimental_capabilities=None, extensions=None, **kwargs):
        return super().get_capabilities(
            NotificationOptions(tools_changed=True, prompts_changed=True, resources_changed=True),
            experimental_capabilities,
            extensions,
            **kwargs,
        )


def create_server(orchestrator: SearchExecutor | None = None, catalog=None, **handlers) -> Server:
    registry = create_registry(orchestrator or build_default_orchestrator(), catalog)

    async def list_tools(ctx, params) -> types.ListToolsResult:
        return types.ListToolsResult(tools=await asyncio.to_thread(registry.definitions))

    async def call_tool(ctx, params: types.CallToolRequestParams) -> types.CallToolResult:
        # Cancelling the wait never terminates synchronous inference already running.
        return await asyncio.to_thread(registry.invoke, params.name, params.arguments or {})

    return RagServer(
        "mini-agent-rag",
        version=__version__,
        on_list_tools=list_tools,
        on_call_tool=call_tool,
        **handlers,
    )
