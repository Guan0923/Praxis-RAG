"""Expose the RAG orchestrator as a long-lived stdio MCP server."""

from __future__ import annotations

import asyncio
import json
import os
import sys
from collections.abc import Mapping, Sequence
from typing import Any, Protocol

from mcp import types
from mcp.server import Server
from mcp.server.stdio import stdio_server

from .config import RagConfig, RagConfigError
from .contracts import RagOperationError, SearchRequest, SearchValidationError
from .orchestrator import build_default_orchestrator
from .remote import RemoteRagOrchestrator, RemoteRagSettings


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
        inputSchema=SEARCH_TOOL_SCHEMA,
        annotations=types.ToolAnnotations(
            readOnlyHint=True,
            destructiveHint=False,
            idempotentHint=True,
            openWorldHint=False,
        ),
    )


def invoke_search(
    orchestrator: SearchExecutor,
    arguments: Mapping[str, Any] | None,
) -> types.CallToolResult:
    """Validate one MCP call and render a JSON observation for the Agent."""

    _trace("search validation started")
    try:
        request = SearchRequest.from_arguments(dict(arguments or {}))
        _trace("orchestrator search started")
        payload = orchestrator.search(request)
        _trace("orchestrator search completed")
    except (RagConfigError, RagOperationError, SearchValidationError, KeyError) as exc:
        _trace(f"search failed: {type(exc).__name__}")
        return types.CallToolResult(
            content=[types.TextContent(type="text", text=str(exc))],
            isError=True,
        )
    return types.CallToolResult(
        content=[types.TextContent(type="text", text=json.dumps(payload, ensure_ascii=False, indent=2))]
    )


def _trace(message: str) -> None:
    """Write secret-free stage diagnostics when explicitly enabled."""

    if os.getenv("RAG_DEBUG_TRACE", "").strip().lower() in {"1", "true", "yes"}:
        print(f"[rag-mcp] {message}", file=sys.stderr, flush=True)


def create_server(orchestrator: SearchExecutor | None = None) -> Server:
    """Create the MCP protocol adapter around one orchestrator instance."""

    resolved = orchestrator or build_default_orchestrator()
    server = Server("mini-agent-rag")

    @server.list_tools()
    async def list_tools() -> Sequence[types.Tool]:
        return [search_tool_definition()]

    @server.call_tool(validate_input=False)
    async def call_tool(name: str, arguments: dict[str, Any]) -> types.CallToolResult:
        if name != "search":
            return types.CallToolResult(
                content=[types.TextContent(type="text", text=f"Unknown RAG tool: {name}")],
                isError=True,
            )
        # Model inference and Qdrant's synchronous client must not block the
        # MCP protocol event loop while it is servicing the stdio session.
        return await asyncio.to_thread(invoke_search, resolved, arguments)

    return server


async def run_stdio_server() -> None:
    """Run until the parent MCP client closes standard input."""

    if os.getenv("RAG_SERVICE_URL", "").strip():
        orchestrator: SearchExecutor = RemoteRagOrchestrator(RemoteRagSettings.from_env())
        _trace("using remote RAG Service transport")
    else:
        config = RagConfig.from_env()
        local_orchestrator = build_default_orchestrator(config)
        if config.preload_model:
            _trace("model preload started")
            await asyncio.to_thread(local_orchestrator.warmup)
            _trace("model preload completed")
        orchestrator = local_orchestrator
    server = create_server(orchestrator)
    async with stdio_server() as (read_stream, write_stream):
        await server.run(read_stream, write_stream, server.create_initialization_options())


def main() -> int:
    """Console entry point used by Mini-Agent's external MCP manager."""

    asyncio.run(run_stdio_server())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
