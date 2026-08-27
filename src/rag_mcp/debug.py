"""Human-readable local debugger for the RAG contract without an MCP client."""

from __future__ import annotations

import argparse
import json

from .contracts import SearchRequest, SearchValidationError
from .orchestrator import build_default_orchestrator
from .server import SEARCH_TOOL_SCHEMA


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Run one RAG search and print its JSON observation.")
    parser.add_argument("query", nargs="?", help="Question or retrieval query")
    parser.add_argument("--retrieval", choices=("bm25", "embedding", "hybrid"), default="hybrid")
    parser.add_argument("--rerank", choices=("none", "cross_encoder", "bi_encoder"), default="cross_encoder")
    parser.add_argument("--candidate-k", type=int, default=10)
    parser.add_argument("--top-k", type=int, default=5)
    parser.add_argument("--filters", default="{}", help="JSON object using document_id, source, or title")
    parser.add_argument("--schema", action="store_true", help="Print the MCP input schema and exit")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    if args.schema:
        print(json.dumps(SEARCH_TOOL_SCHEMA, ensure_ascii=False, indent=2))
        return 0
    if not args.query:
        raise SystemExit("query is required unless --schema is used")
    try:
        filters = json.loads(args.filters)
        request = SearchRequest.from_arguments(
            {
                "query": args.query,
                "retrieval": args.retrieval,
                "rerank": args.rerank,
                "candidate_k": args.candidate_k,
                "top_k": args.top_k,
                "filters": filters,
            }
        )
    except (json.JSONDecodeError, SearchValidationError) as exc:
        raise SystemExit(str(exc)) from exc
    result = build_default_orchestrator().search(request)
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
