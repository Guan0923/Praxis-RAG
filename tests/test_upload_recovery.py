"""Upload failure containment: stale workspace reclamation and in-flight delete guard."""

from __future__ import annotations

import os
import tempfile
import threading
import time
from pathlib import Path

import pytest

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.chunking import _stable_id
from rag_mcp.service_knowledge import IndexedDocument, KnowledgeDocumentStore, KnowledgeValidationError
from rag_mcp.state.store import StateStores
from rag_mcp.workspace import cleanup_workspace, sweep_stale_workspaces


def test_sweep_removes_only_old_known_workspaces(tmp_path, monkeypatch):
    old = tmp_path / "rag-pdf-abandoned"
    fresh = tmp_path / "rag-word-active"
    unrelated = tmp_path / "other-directory"
    for directory in (old, fresh, unrelated):
        directory.mkdir()
        (directory / "file.txt").write_text("x", encoding="utf-8")
    old_time = time.time() - 48 * 60 * 60
    os.utime(old, (old_time, old_time))
    monkeypatch.setattr("rag_mcp.workspace.tempfile.gettempdir", lambda: str(tmp_path))

    removed = sweep_stale_workspaces(max_age_seconds=24 * 60 * 60)

    assert removed == [old]
    assert not old.exists()
    assert fresh.exists()
    assert unrelated.exists()


def test_cleanup_workspace_is_idempotent(tmp_path):
    target = tmp_path / "rag-upload-test"
    target.mkdir()
    (target / "source.txt").write_text("x", encoding="utf-8")
    assert cleanup_workspace(target) is True
    assert cleanup_workspace(target) is True


def test_delete_is_rejected_while_the_same_document_is_uploading(tmp_path):
    state = StateStores.create(tmp_path / "state")
    store = KnowledgeDocumentStore(state, "alpha")
    document_id = _stable_id("doc", "a.txt")
    state.blobs.put("alpha", "source", "a.txt", b"old", document_id=document_id)
    state.documents.upsert("alpha", {
        "name": "a.txt", "document_id": document_id, "sha256": "old", "chunking": {},
        "collection": "c", "chunk_count": 1,
    })

    started = threading.Event()
    release = threading.Event()

    class BlockingIngester:
        def ingest_and_publish(self, path, *, source_name, chunking=None, publish=None):
            started.set()
            assert release.wait(10)
            publish(IndexedDocument(document_id, 1, "c"))

        def delete(self, document_id):
            raise AssertionError("delete must not run while the upload is in flight")

    errors: list[BaseException] = []

    def upload():
        try:
            store.store_and_index("a.txt", __import__("io").BytesIO(b"new"), BlockingIngester(),
                                  ChunkingConfig("character", 100, 10))
        except BaseException as exc:  # noqa: BLE001 - collected for the assertion
            errors.append(exc)

    thread = threading.Thread(target=upload, daemon=True)
    thread.start()
    assert started.wait(10)
    try:
        with pytest.raises(KnowledgeValidationError, match="正在更新"):
            store.delete("a.txt", BlockingIngester())
    finally:
        release.set()
        thread.join(10)
    assert not errors
    assert state.blobs.get("alpha", "source", "a.txt") == b"new"
    state.close()


def test_failed_embedding_removes_the_parse_workspace(tmp_path, monkeypatch):
    from rag_mcp import pdf_assets
    from rag_mcp.chunking import DocumentChunk
    from rag_mcp.config import RagConfig
    from rag_mcp.pdf_assets import ParsedPdf
    from rag_mcp.service_knowledge import DenseDocumentIngester
    from rag_mcp.workspace import create_workspace

    stage = create_workspace("rag-pdf-")
    (stage / "manifest.json").write_text("{}", encoding="utf-8")
    (stage / "document.md").write_text("saved", encoding="utf-8")
    chunk = DocumentChunk("doc-paper", "chunk-1", "text", "Paper", "paper.pdf", {})

    monkeypatch.setattr(pdf_assets, "parse_pdf", lambda *_args, **_kwargs: ParsedPdf([chunk], [], stage))

    class FailingEmbedder:
        def embed_documents(self, _texts):
            raise RuntimeError("controlled embedding failure")

    ingester = DenseDocumentIngester(RagConfig(backend="qdrant"), FailingEmbedder(), object(), object())
    with pytest.raises(RuntimeError, match="controlled embedding failure"):
        ingester.ingest_and_publish(
            tmp_path / "source.pdf", source_name="paper.pdf", chunking=ChunkingConfig("character", 100, 10)
        )
    assert not stage.exists()


def test_staging_workspace_is_removed_after_parse_failure(tmp_path, monkeypatch):
    from rag_mcp import pdf_assets

    created: list[Path] = []

    def fake_workspace(prefix):
        path = Path(tempfile.mkdtemp(prefix=prefix, dir=tmp_path))
        created.append(path)
        return path

    monkeypatch.setattr(pdf_assets, "create_workspace", fake_workspace)

    class FailedProcess:
        returncode = 1
        stdout = ""
        stderr = "controlled failure"

    monkeypatch.setattr(pdf_assets, "run_bounded", lambda *_args, **_kwargs: FailedProcess())

    with pytest.raises(ValueError, match="PDF 解析失败"):
        pdf_assets.parse_pdf(
            tmp_path / "missing.pdf", source_name="paper.pdf", config=ChunkingConfig(),
            tokenizer_model="BAAI/bge-m3", parser_python=None, models_path=None, timeout_seconds=5,
        )
    assert created and all(not path.exists() for path in created)
