"""PDF structure, real tokenizer, persistence and resource boundary regressions."""

import copy
import io
import json
from types import SimpleNamespace

import pytest
from qdrant_client import QdrantClient

from rag_mcp.chunking import _stable_id
from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.config import RagConfig
from rag_mcp.pdf_assets import ParsedPdf, read_pdf_content
from rag_mcp.pdf_content import PDF_PARSER_VERSION, content_chunks, render_content
from rag_mcp.resources import paragraph_pages
from rag_mcp.service_knowledge import DenseDocumentIngester, KnowledgeDocumentStore, KnowledgePaths
from rag_mcp.sparse import Bm25SparseEncoder
from rag_mcp.storage.qdrant_store import QdrantStore


def manifest_fixture():
    blocks = [
        {"ref": "text-left", "kind": "text", "text": "Left column sentence. " * 30},
        {"ref": "picture", "kind": "picture", "asset_id": "picture", "text": ""},
        {"ref": "caption", "kind": "caption", "text": "Fig. 1", "association": "document", "associated_assets": ["picture"]},
        {"ref": "formula", "kind": "formula", "text": "$$\n\\frac{a+b}{c}\n$$", "latex": r"\frac{a+b}{c}", "original_text": "a+b c (2)"},
        {"ref": "text-right", "kind": "text", "text": "Right column sentence. " * 30},
        {"ref": "table", "kind": "table", "asset_id": "table", "text": ""},
        {"ref": "unlinked", "kind": "caption", "text": "Unlinked caption", "association": "unlinked", "associated_assets": []},
    ]
    for index, block in enumerate(blocks):
        block.update(page=1, order=index, bbox={"l": 1, "t": 2, "r": 10, "b": 12})
    return {"version": PDF_PARSER_VERSION, "blocks": blocks, "assets": [
        {"asset_id": "picture", "kind": "picture", "caption": "Fig. 1", "generated_description": "private"},
        {"asset_id": "table", "kind": "table", "caption": "", "generated_description": "private table description",
         "table": {"headers": ["A", "", "C"], "rows": [["1", "", "3"]]}},
    ]}


@pytest.mark.parametrize("strategy", ["character", "token", "sentence", "recursive", "structure"])
def test_order_token_limits_and_atomic_formula(strategy):
    config = ChunkingConfig(strategy, 100, 10) if strategy == "character" else ChunkingConfig(strategy, 32, 8)
    manifest = manifest_fixture()
    chunks = content_chunks(manifest, "doc", "paper.pdf", "Paper", config, "BAAI/bge-m3")
    orders = [chunk.metadata["order"] for chunk in chunks]
    assert orders == sorted(orders)
    formulas = [chunk for chunk in chunks if chunk.metadata.get("kind") == "formula"]
    assert [chunk.text for chunk in formulas] == [manifest["blocks"][3]["text"]]
    assert formulas[0].metadata["refs"] == ["formula"]
    ordinary = [chunk for chunk in chunks if "kind" not in chunk.metadata]
    assert len(ordinary) > 2
    assert all(len(chunk.text) <= 100 if strategy == "character" else chunk.metadata["token_count"] <= 32 for chunk in ordinary)
    table = next(chunk for chunk in chunks if chunk.metadata.get("asset_kind") == "table")
    assert "A |  | C\n1 |  | 3" in table.text
    text = render_content(manifest)
    assert text.index("[Image:") < text.index("Fig. 1") < text.index(r"\frac") < text.index("Right column")
    assert text.count("Fig. 1") == 1 and "Unlinked caption" in text and "private" not in text


def test_atomic_items_over_model_limit_are_rejected():
    manifest = manifest_fixture()
    manifest["blocks"][3]["text"] = "$$\n" + "x + " * 9000 + "\n$$"
    with pytest.raises(ValueError, match="model input limit"):
        content_chunks(manifest, "doc", "paper.pdf", "Paper", ChunkingConfig(), "BAAI/bge-m3")
    manifest = manifest_fixture()
    manifest["assets"][1]["table"]["rows"] = [["x " * 9000]]
    with pytest.raises(ValueError, match="model input limit"):
        content_chunks(manifest, "doc", "paper.pdf", "Paper", ChunkingConfig(), "BAAI/bge-m3")


@pytest.mark.parametrize("prefix", ["a" * 7900 + "\n\n", ""])
def test_mcp_pages_preserve_complete_formula(prefix):
    formula = "$$\n" + "x + " * 2100 + "\n$$"
    text = prefix + formula + "\n\nAfter formula"
    pages = paragraph_pages(text)
    assert "".join(pages) == text
    assert sum(formula in page for page in pages) == 1


def test_docling_export_preserves_short_and_unlinked_captions(tmp_path):
    from PIL import Image
    from docling_core.types.doc import DoclingDocument, DocItemLabel, ProvenanceItem, BoundingBox, ImageRef
    from rag_mcp.pdf_worker import _export

    doc = DoclingDocument(name="columns")
    prov = ProvenanceItem(page_no=1, bbox=BoundingBox(l=0, t=20, r=20, b=0), charspan=(0, 0))
    doc.add_text(label=DocItemLabel.TEXT, text="Left column", prov=prov)
    caption = doc.add_text(label=DocItemLabel.CAPTION, text="Fig. 1", prov=prov)
    picture = doc.add_picture(prov=prov, caption=caption,
                              image=ImageRef.from_pil(Image.new("RGB", (20, 20), "white"), dpi=72))
    doc.add_text(label=DocItemLabel.CAPTION, text="Loose", prov=prov)
    formula = doc.add_text(label=DocItemLabel.FORMULA, text=r"\frac{a}{b}", orig="a b (1)", prov=prov)
    doc.add_text(label=DocItemLabel.TEXT, text="Right column", prov=prov)
    manifest = _export(doc, tmp_path)
    asset = manifest["assets"][0]
    assert asset["caption"] == "Fig. 1" and asset["caption_source"] == "document"
    assert asset["captions"][0]["ref"] == caption.self_ref
    loose = next(block for block in manifest["blocks"] if block["text"] == "Loose")
    assert loose["association"] == "unlinked"
    assert len([block for block in manifest["blocks"] if block["ref"] == picture.self_ref]) == 1
    assert "$$\n\\frac{a}{b}\n$$" in render_content(manifest)
    formula.text = ""
    with pytest.raises(RuntimeError, match="empty LaTeX"):
        _export(doc, tmp_path)


def test_pdf_publish_failure_restores_source_assets_manifest_and_vectors(tmp_path, monkeypatch):
    import rag_mcp.pdf_assets as assets_module

    paths = KnowledgePaths(tmp_path / "tenant")
    documents = KnowledgeDocumentStore(paths)
    client = QdrantClient(":memory:")
    store = QdrantStore(collection="rollback", client=client, namespace="tenant")
    config = RagConfig(backend="qdrant", collection="rollback")
    embedder = SimpleNamespace(embed_documents=lambda texts: [[1.0] + [0.0] * 1023 for _ in texts])
    ingester = DenseDocumentIngester(config, embedder, Bm25SparseEncoder(), store)
    document_id = _stable_id("doc", "paper.pdf")

    def parsed(path, **kwargs):
        data = manifest_fixture()
        data["blocks"][0]["text"] = path.read_text(encoding="utf-8")
        data["assets"] = []
        data["blocks"] = [data["blocks"][0]]
        stage = paths.knowledge_dir / ".pdf-assets" / ".stage"
        stage.mkdir(parents=True)
        (stage / "manifest.json").write_text(json.dumps(data), encoding="utf-8")
        (stage / "document.md").write_text(render_content(data), encoding="utf-8")
        chunks = content_chunks(data, document_id, "paper.pdf", "Paper", ChunkingConfig(), "BAAI/bge-m3")
        return ParsedPdf(chunks, [], stage)

    monkeypatch.setattr(assets_module, "parse_pdf", parsed)
    documents.store_and_index("paper.pdf", io.BytesIO(b"old formula"), ingester, ChunkingConfig())
    old_manifest = copy.deepcopy(documents._read_manifest())
    original_write = documents._write_manifest
    monkeypatch.setattr(documents, "_write_manifest", lambda value: (_ for _ in ()).throw(OSError("disk full")))
    with pytest.raises(OSError, match="disk full"):
        documents.store_and_index("paper.pdf", io.BytesIO(b"new formula"), ingester, ChunkingConfig())
    monkeypatch.setattr(documents, "_write_manifest", original_write)
    assert documents._read_manifest() == old_manifest
    path = paths.knowledge_dir / "paper.pdf"
    assert path.read_bytes() == b"old formula"
    assert read_pdf_content(path, paths.knowledge_dir) == "old formula"
    assert store.dense_search([1.0] + [0.0] * 1023, limit=10, filters={})[0].payload["text"] == "old formula"
    client.close()


def test_old_pdf_version_does_not_deduplicate(tmp_path):
    from test_mcp_http import ProtocolRuntime

    runtime = ProtocolRuntime(tmp_path)
    # Only exercise the preflight: the PDF record deliberately predates this parser.
    runtime.create_knowledge_base("tenant", ChunkingConfig(), runtime.config.embedding_model, runtime.config.reranker_model, **runtime.provider_settings)
    documents = runtime._tenant("tenant").documents
    source = documents.paths.knowledge_dir / "paper.pdf"
    source.write_bytes(b"pdf")
    documents._write_manifest({"paper.pdf": {"name": "paper.pdf", "sha256": "a" * 64,
                                           "chunking": ChunkingConfig().metadata("BAAI/bge-m3")}})
    response = runtime.check_documents("tenant", [{"name": "paper.pdf", "sha256": "a" * 64}])
    assert response["files"][0]["document"] is None
    with pytest.raises(ValueError, match="reimport"):
        read_pdf_content(source, documents.paths.knowledge_dir)


def test_truncated_formula_output_is_rejected():
    from rag_mcp.pdf_worker import _CheckedFormulaEngine

    response = SimpleNamespace(text="partial formula", metadata={"num_tokens": 2048})
    engine = _CheckedFormulaEngine(SimpleNamespace(predict_batch=lambda inputs: [response]))
    with pytest.raises(RuntimeError, match="did not finish"):
        engine.predict_batch([SimpleNamespace(max_new_tokens=2048)])
    response.text = r"a + b</formula>"
    response.metadata["num_tokens"] = 8
    assert engine.predict_batch([SimpleNamespace(max_new_tokens=2048)])[0] is response


def test_asset_install_failure_restores_previous_directory(tmp_path):
    from rag_mcp.pdf_assets import install_assets

    folder = tmp_path / ".pdf-assets" / "doc"
    folder.mkdir(parents=True)
    (folder / "document.md").write_text("previous", encoding="utf-8")
    with pytest.raises(OSError):
        install_assets(ParsedPdf([], [], tmp_path / "missing-stage"), tmp_path, "doc")
    assert (folder / "document.md").read_text(encoding="utf-8") == "previous"


def test_native_table_export_keeps_empty_columns_and_merged_cells():
    from docling_core.types.doc import DoclingDocument, TableData, TableCell
    from rag_mcp.pdf_worker import _table_data

    doc = DoclingDocument(name="table")
    cells = [
        TableCell(text="A", start_row_offset_idx=0, end_row_offset_idx=1,
                  start_col_offset_idx=0, end_col_offset_idx=1, column_header=True),
        TableCell(text="C", start_row_offset_idx=0, end_row_offset_idx=1,
                  start_col_offset_idx=2, end_col_offset_idx=3, column_header=True),
        TableCell(text="merged", start_row_offset_idx=1, end_row_offset_idx=2,
                  start_col_offset_idx=0, end_col_offset_idx=2, col_span=2),
        TableCell(text="3", start_row_offset_idx=1, end_row_offset_idx=2,
                  start_col_offset_idx=2, end_col_offset_idx=3),
    ]
    item = doc.add_table(data=TableData(num_rows=2, num_cols=3, table_cells=cells))
    table = _table_data(item, doc)
    assert table["headers"] == ["A", "", "C"]
    assert table["rows"] == [["A", "", "C"], ["merged", "merged", "3"]]
    assert table["cells"][2]["col_span"] == 2


def test_low_confidence_ocr_is_recorded_and_reaches_chunks(tmp_path):
    from docling_core.types.doc import BoundingBox
    from rag_mcp.pdf_worker import _ocr_diagnostics

    rect = SimpleNamespace(to_bounding_box=lambda: BoundingBox(l=0, t=2, r=3, b=0))
    page = SimpleNamespace(page_no=1, cells=[
        SimpleNamespace(from_ocr=True, confidence=0.01, text="retained", rect=rect),
        SimpleNamespace(from_ocr=False, confidence=1.0, text="native", rect=rect),
    ])
    warnings = _ocr_diagnostics([page], tmp_path)
    assert json.loads((tmp_path / "ocr.json").read_text(encoding="utf-8"))[0]["text"] == "retained"
    manifest = manifest_fixture()
    manifest["warnings"] = warnings
    chunks = content_chunks(manifest, "doc", "paper.pdf", "Paper", ChunkingConfig(), "BAAI/bge-m3")
    assert all(chunk.metadata["source_warnings"][0]["count"] == 1 for chunk in chunks)


def test_multilevel_table_headers_are_not_overwritten():
    from docling_core.types.doc import DoclingDocument, TableData, TableCell
    from rag_mcp.pdf_worker import _table_data

    doc = DoclingDocument(name="headers")
    cells = [
        TableCell(text="Group", start_row_offset_idx=0, end_row_offset_idx=1,
                  start_col_offset_idx=0, end_col_offset_idx=2, col_span=2, column_header=True),
        TableCell(text="A", start_row_offset_idx=1, end_row_offset_idx=2,
                  start_col_offset_idx=0, end_col_offset_idx=1, column_header=True),
        TableCell(text="B", start_row_offset_idx=1, end_row_offset_idx=2,
                  start_col_offset_idx=1, end_col_offset_idx=2, column_header=True),
        TableCell(text="1", start_row_offset_idx=2, end_row_offset_idx=3,
                  start_col_offset_idx=0, end_col_offset_idx=1),
    ]
    item = doc.add_table(data=TableData(num_rows=3, num_cols=2, table_cells=cells))
    table = _table_data(item, doc)
    assert table["headers"] == ["Group\nA", "Group\nB"]
    assert table["header_rows"] == [["Group", "Group"], ["A", "B"]]
    manifest = manifest_fixture()
    manifest["assets"][1]["table"] = table
    chunks = content_chunks(manifest, "doc", "paper.pdf", "Paper", ChunkingConfig(), "BAAI/bge-m3")
    rows = [chunk.text for chunk in chunks if chunk.metadata.get("asset_kind") == "table"]
    assert len(rows) == 3 and all("Group | Group\nA | B" in row for row in rows)


def test_asset_projection_preserves_source_references(tmp_path):
    from rag_mcp.contracts import Candidate
    from rag_mcp.pdf_assets import project_results

    metadata = {"asset_id": "table", "refs": ["#/tables/0"], "table_row": 2,
                "source_warnings": [{"page": 1, "kind": "low_confidence_ocr"}]}
    candidate = Candidate("doc", "chunk", "Title", "paper.pdf", "private", 0.9, metadata)
    store = SimpleNamespace(get_assets=lambda ids: {"table": {
        "asset_kind": "table", "page": 1, "table": {"rows": [["1", "", "3"]], "headers": ["A", "", "C"]},
    }})
    result = project_results([candidate], store, tmp_path)[0]
    assert result["metadata"]["refs"] == ["#/tables/0"]
    assert result["metadata"]["source_warnings"] == metadata["source_warnings"]
    assert result["asset"]["headers"] == ["A", "", "C"]


def test_cross_page_block_keeps_all_source_locations():
    manifest = manifest_fixture()
    locations = [{"page": page, "bbox": {"l": 1}, "charspan": [0, 100]} for page in (1, 2)]
    manifest["blocks"][0]["locations"] = locations
    chunks = content_chunks(manifest, "doc", "paper.pdf", "Paper", ChunkingConfig(), "BAAI/bge-m3")
    first = chunks[0].metadata
    assert first["pages"] == [1, 2]
    assert first["provenance"][0]["locations"] == locations
