from __future__ import annotations

import base64
import json
from pathlib import Path

from mcp import types
from qdrant_client import QdrantClient

from rag_mcp.chunking import DocumentChunk
from rag_mcp.config import DENSE_VECTOR_SIZE
from rag_mcp.contracts import Candidate
from rag_mcp.orchestrator import RagOrchestrator
from rag_mcp.pdf_assets import project_results
from rag_mcp.pdf_worker import _validate_table
from rag_mcp.server import RegisteredTool, ToolRegistry, search_tool_definition
from rag_mcp.storage.qdrant_store import QdrantStore


def _vector() -> list[float]:
    return [1.0] + [0.0] * (DENSE_VECTOR_SIZE - 1)


def _chunk(asset_id: str, kind: str = "picture") -> DocumentChunk:
    return DocumentChunk("doc-1", f"chunk-{kind}", "private retrieval description", "PDF", "manual.pdf",
                         {"asset_id": asset_id, "asset_kind": kind, "page": 2})


def test_qdrant_asset_has_no_vector_and_projection_returns_original_image() -> None:
    client = QdrantClient(":memory:")
    store = QdrantStore(collection="pdf-assets", client=client, namespace="tenant")
    asset_id = "doc-1:#/pictures/0"
    image = b"\x89PNG\r\n\x1a\noriginal"
    root = Path(".test-tmp/pdf-assets-tests/image").resolve()
    image_path = root / "doc-1" / "pictures" / "image.png"
    image_path.parent.mkdir(parents=True, exist_ok=True)
    image_path.write_bytes(image)
    store.replace_document([_chunk(asset_id)], [_vector()], asset_payloads=[{
        "record_type": "asset", "document_id": "doc-1", "asset_id": asset_id,
        "asset_kind": "picture", "source": "manual.pdf", "page": 2, "bbox": {"l": 1},
        "caption": "Figure 1. Original caption", "caption_source": "document",
        "generated_description": "must stay private", "image_file": "doc-1/pictures/image.png", "table": None,
    }])
    candidate = Candidate("doc-1", "chunk-picture", "PDF", "manual.pdf", "private retrieval description",
                          0.9, {"asset_id": asset_id, "asset_kind": "picture", "page": 2})

    result = project_results([candidate], store, root)[0]

    assert base64.b64decode(result["asset"]["data"]) == image
    encoded = json.dumps(result, ensure_ascii=False)
    assert "Original caption" in encoded
    assert "private retrieval description" not in encoded
    assert "must stay private" not in encoded


def test_table_projection_preserves_cells_and_spans() -> None:
    client = QdrantClient(":memory:")
    store = QdrantStore(collection="pdf-table", client=client, namespace="tenant")
    asset_id = "doc-1:#/tables/0"
    table = {"num_rows": 2, "num_cols": 2, "headers": ["Name", "Value"],
             "rows": [["Name", "Value"], ["A", "1.25"]],
             "cells": [{"text": "Name", "row_span": 1, "col_span": 2}], "footnotes": ["Source note"]}
    store.replace_document([_chunk(asset_id, "table")], [_vector()], asset_payloads=[{
        "record_type": "asset", "document_id": "doc-1", "asset_id": asset_id,
        "asset_kind": "table", "source": "manual.pdf", "page": 3, "bbox": {},
        "caption": "Table 1. Values", "caption_source": "document",
        "generated_description": "private", "image_file": None, "table": table,
    }])
    candidate = Candidate("doc-1", "chunk-table", "PDF", "manual.pdf", "private", 0.8,
                          {"asset_id": asset_id, "asset_kind": "table"})

    asset = project_results([candidate], store, Path(".test-tmp/pdf-assets-tests/table").resolve())[0]["asset"]

    assert asset["rows"][1] == ["A", "1.25"]
    assert asset["cells"][0]["col_span"] == 2
    assert asset["footnotes"] == ["Source note"]


def test_orchestrator_deduplicates_rows_from_the_same_table() -> None:
    asset_id = "doc-1:#/tables/0"
    candidates = [Candidate("doc-1", f"chunk-{index}", "PDF", "manual.pdf", str(index), 1 - index / 10,
                            {"asset_id": asset_id, "asset_kind": "table"}) for index in range(3)]

    class Retriever:
        def search(self, query, *, limit, filters):
            return candidates

    orchestrator = RagOrchestrator({"hybrid": Retriever()}, {}, implementation="test")
    result = orchestrator.search(__import__("rag_mcp.contracts", fromlist=["SearchRequest"]).SearchRequest(
        query="value", retrieval="hybrid", rerank="none", candidate_k=3, top_k=3))
    assert len(result["results"]) == 1


def test_mcp_returns_image_content_block_without_base64_in_json() -> None:
    encoded = base64.b64encode(b"image").decode("ascii")
    registry = ToolRegistry([RegisteredTool(search_tool_definition(), lambda value: value, lambda value: {
        "results": [{"asset": {"type": "image", "mime_type": "image/png", "data": encoded}}]
    })])

    result = registry.invoke("search", {"query": "image"})

    assert isinstance(result.content[0], types.TextContent)
    assert isinstance(result.content[1], types.ImageContent)
    assert result.content[1].data == encoded
    assert encoded not in result.content[0].text


def test_qdrant_replacement_restores_old_document_when_cleanup_fails() -> None:
    client = QdrantClient(":memory:")
    store = QdrantStore(collection="pdf-rollback", client=client, namespace="tenant")
    old = DocumentChunk("doc-1", "old", "old text", "PDF", "manual.pdf", {})
    new = DocumentChunk("doc-1", "new", "new text", "PDF", "manual.pdf", {})
    store.replace_document([old], [_vector()])
    original_delete = client.delete
    failed = False

    def fail_stale_delete(*args, **kwargs):
        nonlocal failed
        if not failed:
            failed = True
            raise RuntimeError("controlled cleanup failure")
        return original_delete(*args, **kwargs)

    client.delete = fail_stale_delete
    try:
        store.replace_document([new], [_vector()])
    except RuntimeError as exc:
        assert "controlled" in str(exc)
    else:
        raise AssertionError("replacement should report the write failure")

    points = store.search(_vector(), limit=10, filters={})
    assert [point.payload["text"] for point in points] == ["old text"]


def test_unreliable_wide_rotated_table_is_rejected_before_indexing() -> None:
    table = {"num_cols": 11, "headers": [""] * 11,
             "rows": [[""] * 11, ["", "Table Site ch", "stics for all", "ments", "", "", "", "", "", "", ""]]}

    try:
        _validate_table(table, "#/tables/0")
    except RuntimeError as exc:
        assert "unreliable" in str(exc)
    else:
        raise AssertionError("corrupted rotated table must not be published")
