"""Opt-in real CUDA PDF parsing, HTTP ingestion, Qdrant retrieval and MCP reads."""

import hashlib
import json
import os
import threading
import time
from pathlib import Path
from uuid import uuid4

import anyio
import httpx2 as httpx
import pytest

from rag_mcp.config import RagConfig
from rag_mcp.pdf_content import PDF_PARSER_VERSION
from rag_mcp.resources import document_uri
from rag_mcp.service_knowledge import RagServiceRuntime
from test_mcp_http import AUTH, mcp_client, running_service
from model_api_test_support import configure_api_runtime


def check_saved_fidelity(client, collection, folder, document_id):
    from docling_core.types.doc import DoclingDocument
    from qdrant_client import models
    from rag_mcp.pdf_content import table_row_text

    manifest = json.loads((folder / "manifest.json").read_text(encoding="utf-8"))
    native = DoclingDocument.load_from_json(folder / "document.json")
    native_items = {item.self_ref: item for item, _ in native.iterate_items()}
    native_order = list(native_items)
    order = [native_order.index(block["ref"]) for block in manifest["blocks"]]
    assert order == sorted(order), "Export changed Docling reading order"
    for block in manifest["blocks"]:
        item = native_items[block["ref"]]
        assert [part["page"] for part in block["locations"]] == [part.page_no for part in item.prov]
    payloads, offset = [], None
    while True:
        points, offset = client.scroll(collection, scroll_filter=models.Filter(must=[
            models.FieldCondition(key="document_id", match=models.MatchValue(value=document_id))
        ]), limit=256, offset=offset, with_vectors=False)
        payloads.extend(point.payload for point in points)
        if offset is None:
            break
    chunks = [point for point in payloads if point["record_type"] == "chunk"]
    ocr = json.loads((folder / "ocr.json").read_text(encoding="utf-8"))
    low_pages = {cell["page"] for cell in ocr if cell["confidence"] < 0.5}
    assert low_pages == {warning["page"] for warning in manifest.get("warnings", [])}
    for point in chunks:
        metadata = point["metadata"]
        if low_pages.intersection(metadata.get("pages", [metadata.get("page")])):
            assert metadata.get("source_warnings")
    body = (folder / "document.md").read_text(encoding="utf-8")
    formulas = [item for item in native.texts if item.label.value == "formula"]
    for item in formulas:
        block = next(block for block in manifest["blocks"] if block["ref"] == item.self_ref)
        indexed = next(point for point in chunks if point["metadata"].get("refs") == [item.self_ref])
        assert item.text == block["latex"] == indexed["metadata"]["latex"]
        assert indexed["text"] == block["text"] and block["text"] in body
        assert indexed["metadata"]["page"] == item.prov[0].page_no
    for asset in manifest["assets"]:
        indexed = next(point for point in payloads if point.get("asset_id") == f"{document_id}:{asset['asset_id']}")
        assert indexed["captions"] == asset["captions"] and indexed["footnotes"] == asset["footnotes"]
        if asset["kind"] == "table":
            assert indexed["table"] == asset["table"]
            rows = [point for point in chunks if point["metadata"].get("asset_id") == indexed["asset_id"]]
            assert len(rows) == asset["table"]["num_rows"]
            headers = "\n".join(table_row_text(row) for row in asset["table"]["header_rows"])
            assert all(headers in point["text"] for point in rows)
        else:
            assert (folder / asset["image_file"]).is_file()
    return {"native_order_preserved": True, "formula_latex_preserved": len(formulas),
            "assets_preserved": len(manifest["assets"]), "indexed_chunks": len(chunks)}


@pytest.mark.skipif(os.getenv("RAG_REAL_PDF") != "1", reason="Set RAG_REAL_PDF=1 for CUDA PDF acceptance")
def test_real_pdf_ingestion_resources_and_retrieval():
    samples = Path(os.environ["RAG_PDF_SAMPLES"])
    root = Path(".test-tmp/pdf-fidelity-real") / uuid4().hex[:8]
    root.mkdir(parents=True)
    config = RagConfig(
        backend="qdrant", qdrant_path=root / "qdrant", collection="pdf_fidelity",
        embedding_device="cuda:0", reranker_device="cuda:0",
        pdf_parser_python=Path(os.environ["RAG_PDF_PARSER_PYTHON"]),
        pdf_models_path=Path(os.environ["RAG_PDF_MODELS_PATH"]), pdf_parse_timeout=900,
    )
    runtime = RagServiceRuntime(config, root / "data")
    runtime.release = threading.Event()
    runtime.wire_responses = []
    report = {"root": str(root.resolve()), "parser_version": PDF_PARSER_VERSION, "documents": []}
    filenames = os.getenv("RAG_PDF_SAMPLE_FILES", "aviation.pdf,doclaynet.pdf,mineru_demo2.pdf,scan.pdf").split(",")
    try:
        configure_api_runtime(runtime)
        with running_service(root / "data", runtime=runtime) as (url, _, _):
            async def scenario():
                async with httpx.AsyncClient(headers=AUTH, timeout=1200) as http:
                    for name in filenames:
                        content = (samples / name).read_bytes()
                        started = time.perf_counter()
                        response = await http.post(url + "/v1/documents", files={"files": (name, content)},
                                                   data={"strategy": "recursive", "chunk_size": "256", "chunk_overlap": "32"})
                        payload = response.json()
                        row = {"name": name, "seconds": time.perf_counter() - started, "upload": payload}
                        report["documents"].append(row)
                        (root / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
                        if payload.get("failed") or not payload.get("imported"):
                            continue
                        record = payload["imported"][0]
                        folder = root / "data/manuals/knowledge/.pdf-assets" / record["document_id"]
                        manifest = json.loads((folder / "manifest.json").read_text(encoding="utf-8"))
                        row["preservation"] = check_saved_fidelity(runtime._client, runtime.config.collection, folder, record["document_id"])
                        row["parser"] = manifest["parser"]
                        row["metrics"] = manifest["metrics"]
                        row["formula_count"] = sum(block["kind"] == "formula" for block in manifest["blocks"])
                        row["asset_count"] = len(manifest["assets"])
                        row["unlinked_captions"] = [block["ref"] for block in manifest["blocks"]
                                                    if block["kind"] == "caption" and block.get("association") == "unlinked"]
                        row["warnings"] = manifest.get("warnings", [])
                        if name == "scan.pdf":
                            assert row["warnings"], "Known low-confidence scan must retain OCR warnings"
                        checked = await http.post(url + "/v1/documents/check", json={
                            "files": [{"name": "renamed.pdf", "sha256": hashlib.sha256(content).hexdigest()}],
                            "strategy": "recursive", "chunk_size": 256, "chunk_overlap": 32,
                        })
                        assert checked.json()["files"][0]["document"]["name"] == name
                        original_text = (folder / "document.md").read_text(encoding="utf-8")
                        pages = []
                        async with mcp_client(url) as client:
                            uri = document_uri("manuals", name)
                            while uri:
                                result = await client.read_resource(uri)
                                resource = json.loads(result.contents[0].text)
                                pages.append(resource["text"])
                                uri = resource["next_uri"]
                        assert "".join(pages) == original_text
                        queries = [block["latex"] for block in manifest["blocks"] if block["kind"] == "formula"][:1]
                        queries += [asset["caption"] for asset in manifest["assets"] if asset["caption"]][:1]
                        queries = queries or [next(block["text"] for block in manifest["blocks"] if block["text"])[:120]]
                        row["searches"] = []
                        for query in queries:
                            found = await http.post(url + "/v1/search", json={
                                "query": query, "retrieval": "hybrid", "rerank": "none", "candidate_k": 50,
                                "top_k": 10, "filters": {"document_id": record["document_id"]},
                            })
                            result = found.json()
                            assert found.status_code == 200 and result["results"], result
                            if query in [block.get("latex") for block in manifest["blocks"]]:
                                assert any(item.get("metadata", {}).get("latex") == query for item in result["results"])
                            else:
                                captioned = [asset for asset in manifest["assets"] if asset["caption"] == query]
                                if captioned:
                                    assert any(item.get("asset", {}).get("caption") == query for item in result["results"])
                            row["searches"].append({"query": query, "results": [
                                {key: value for key, value in item.items() if key != "asset"} for item in result["results"]
                            ]})
                        row["mcp_pages"] = len(pages)
                        row["saved_read_matches"] = True
                        (root / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
                        print(f"REAL_PDF {name}: {row['seconds']:.2f}s, {row['formula_count']} formulas, {row['asset_count']} assets", flush=True)
            anyio.run(scenario)
    finally:
        runtime._client.close()
        (root / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"REAL_PDF_REPORT={root.resolve() / 'report.json'}", flush=True)
    failed = [row for row in report["documents"] if not row.get("saved_read_matches")]
    assert not failed, [(row["name"], row["upload"]) for row in failed]
