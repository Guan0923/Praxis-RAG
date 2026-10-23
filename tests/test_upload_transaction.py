"""Fault injection for the complete upload transaction without model downloads."""

import io
import tempfile
from pathlib import Path
from types import SimpleNamespace

import pytest
from qdrant_client import QdrantClient
from sqlalchemy import update

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.config import RagConfig
from rag_mcp.service_knowledge import DenseDocumentIngester, KnowledgeDocumentStore, KnowledgeValidationError
from rag_mcp.sparse import Bm25SparseEncoder
from rag_mcp.state.schema import document_leases, upload_operations
from rag_mcp.state.store import StateStores
from rag_mcp.state.upload_coordinator import UploadCoordinator
from rag_mcp.state.uploads import UploadConflict
from rag_mcp.storage.qdrant_store import QdrantStore

CONFIG = ChunkingConfig("character", 100, 10)
VECTOR = [1.0] + [0.0] * 1023


@pytest.fixture()
def stack(tmp_path):
    state = StateStores.create(tmp_path / "state")
    client = QdrantClient(":memory:")
    vectors = QdrantStore(collection="uploads", namespace="alpha", client=client,
                          active_versions=lambda: state.documents.visible_versions("alpha"))
    embedder = SimpleNamespace(embed_documents=lambda texts: [VECTOR for _ in texts])
    ingester = DenseDocumentIngester(RagConfig(backend="qdrant", collection="uploads"),
                                     embedder, Bm25SparseEncoder(), vectors)
    documents = KnowledgeDocumentStore(state, "alpha")
    yield state, vectors, ingester, documents
    state.close()
    client.close()


def upload(stack, content=b"original document text", key=None):
    return stack[3].store_and_index("file.txt", io.BytesIO(content), stack[2], CONFIG,
                                     idempotency_key=key)


def test_commits_publish_only_after_all_steps_and_replay(stack):
    state, vectors, ingester, documents = stack
    original_stage = vectors.stage_version

    def check_stage(*args):
        count = original_stage(*args)
        assert documents.list_documents() == []
        assert vectors.dense_search(VECTOR, limit=10, filters={}) == []
        return count

    vectors.stage_version = check_stage
    first = upload(stack, key="once")
    vectors.stage_version = original_stage
    op = state.uploads.get(first["operation_id"])
    assert op["status"] == "COMMITTED"
    assert all(step["status"] == "DONE" for step in op["steps"])
    assert {step["name"] for step in op["steps"]} >= {
        "receive_source", "chunking_tokenizer", "embedding", "sparse", "stage_mysql", "stage_qdrant", "publish"}
    assert state.uploads.head("alpha", first["document_id"]) == first["operation_id"]
    assert documents.read_source("file.txt") == b"original document text"
    replay = upload(stack, key="once")
    assert replay["replayed"] and replay["operation_id"] == first["operation_id"]
    with pytest.raises(UploadConflict):
        upload(stack, b"different", key="once")
    assert vectors.dense_search(VECTOR, limit=10, filters={})[0].payload["text"] == "original document text"


@pytest.mark.parametrize("failed_step", ["receive_source", "chunking_tokenizer", "embedding", "sparse", "stage_mysql", "stage_qdrant"])
def test_failure_after_side_effect_keeps_old_head_and_idempotently_compensates(stack, monkeypatch, failed_step):
    state, vectors, ingester, documents = stack
    original = upload(stack)
    actual_step = state.uploads.run_step

    def inject(op, name, action, details=None):
        def execute():
            result = action()
            if name == failed_step:
                raise OSError("injected failure after effect")
            return result
        return actual_step(op, name, execute, details)

    monkeypatch.setattr(state.uploads, "run_step", inject)
    with pytest.raises(OSError):
        upload(stack, b"replacement")
    assert documents.read_source("file.txt") == b"original document text"
    assert state.uploads.head("alpha", original["document_id"]) == original["operation_id"]
    assert all(p.payload["text"] != "replacement" for p in vectors.dense_search(VECTOR, limit=20, filters={}))
    with state.engine.connect() as conn:
        statuses = list(conn.execute(__import__("sqlalchemy").select(upload_operations.c.status)).scalars())
    assert sorted(statuses) == ["COMMITTED", "ROLLED_BACK"]
    assert all(op["status"] == "COMMITTED" for op in state.uploads.unfinished())


def test_database_publish_failure_preserves_previous_everything(stack, monkeypatch):
    state, vectors, ingester, documents = stack
    first = upload(stack)
    original_upsert = state.documents.upsert
    monkeypatch.setattr(state.documents, "upsert", lambda *a, **k: (_ for _ in ()).throw(OSError("publish")))
    with pytest.raises(OSError):
        upload(stack, b"new")
    monkeypatch.setattr(state.documents, "upsert", original_upsert)
    assert documents.read_source("file.txt") == b"original document text"
    assert state.uploads.head("alpha", first["document_id"]) == first["operation_id"]
    assert [p.payload["text"] for p in vectors.dense_search(VECTOR, limit=10, filters={})] == ["original document text"]


def test_crash_after_qdrant_intent_recovered_from_durable_journal(stack):
    state, vectors, ingester, documents = stack
    first = upload(stack)
    journal = state.uploads
    journal.close()
    journal.auto_heartbeat = False
    op = journal.begin("alpha", "file.txt", first["document_id"], "uploads")
    workspace = Path(tempfile.gettempdir()).resolve() / ("rag-txn-" + op["id"])
    journal.register_workspace(op, workspace)
    workspace.mkdir()
    (workspace / "document.md").write_text("unpublished", encoding="utf-8")
    journal.intent(op, "stage_qdrant")
    from rag_mcp.chunking import DocumentChunk
    vectors.stage_version([DocumentChunk(first["document_id"], "new", "unpublished", "File", "file.txt", {})],
                          [VECTOR], None, [], op["id"])
    assert documents.read_source("file.txt") == b"original document text"
    with state.engine.begin() as conn:
        conn.execute(update(document_leases).where(document_leases.c.operation_id == op["id"]).values(expires_at=0))
    UploadCoordinator(state).recover(lambda _: vectors)
    assert journal.get(op["id"])["status"] == "ROLLED_BACK"
    assert not workspace.exists()
    assert [p.payload["text"] for p in vectors.dense_search(VECTOR, limit=10, filters={})] == ["original document text"]
    UploadCoordinator(state).recover(lambda _: vectors)
    assert journal.head("alpha", first["document_id"]) == first["operation_id"]


def test_dedup_commits_a_noop_without_switching_head(stack):
    state, vectors, ingester, documents = stack
    first = upload(stack)
    skipped = upload(stack, key="dedup")
    assert skipped["skipped"]
    assert state.uploads.head("alpha", first["document_id"]) == first["operation_id"]
    assert state.uploads.get(skipped["operation_id"])["status"] == "COMMITTED"


@pytest.mark.parametrize("failed_step", ["parse", "render_artifacts", "chunking_tokenizer", None])
def test_pdf_artifacts_are_journaled_and_recoverable_before_tokenization(stack, monkeypatch, failed_step):
    import json
    from rag_mcp import pdf_assets
    from rag_mcp.chunking import DocumentChunk, _stable_id
    from rag_mcp.pdf_content import PDF_PARSER_VERSION

    state, vectors, ingester, documents = stack
    document_id = _stable_id("doc", "paper.pdf")
    workspaces = []

    def parser(command, **_kwargs):
        root = Path(command[command.index("--output") + 1])
        root.mkdir(parents=True, exist_ok=True)
        workspaces.append(root.parent)
        (root / "document.json").write_text("{}", encoding="utf-8")
        (root / "manifest.json").write_text(json.dumps({
            "version": PDF_PARSER_VERSION,
            "blocks": [{"kind": "text", "text": "parsed evidence", "ref": "t1", "page": 1}],
            "assets": [],
        }), encoding="utf-8")
        return SimpleNamespace(returncode=0, stdout="", stderr="")

    monkeypatch.setattr(pdf_assets, "run_bounded", parser)
    monkeypatch.setattr(pdf_assets, "content_chunks", lambda *a, **k: [
        DocumentChunk(document_id, "c1", "parsed evidence", "Paper", "paper.pdf", {})])
    actual = state.uploads.run_step

    def trace(op, name, action, details=None):
        def execute():
            value = action()
            if name == failed_step:
                raise OSError("after PDF side effect")
            return value
        return actual(op, name, execute, details)

    monkeypatch.setattr(state.uploads, "run_step", trace)
    if failed_step:
        with pytest.raises((OSError, KnowledgeValidationError)):
            documents.store_and_index("paper.pdf", io.BytesIO(b"pdf bytes"), ingester, CONFIG)
        assert documents.list_documents() == []
    else:
        record = documents.store_and_index("paper.pdf", io.BytesIO(b"pdf bytes"), ingester, CONFIG)
        operation = state.uploads.get(record["operation_id"])
        steps = {step["name"]: step for step in operation["steps"]}
        assert {"parse", "render_artifacts", "chunking_tokenizer"} <= steps.keys()
        assert documents.read_text("paper.pdf", record)[0] == "parsed evidence"
    assert workspaces and all(not root.exists() for root in workspaces)


def test_soft_delete_retains_version_blobs_without_touching_other_documents(stack):
    state, vectors, ingester, documents = stack
    first = upload(stack)
    second = upload(stack, b"replacement")
    other = documents.store_and_index("other.txt", io.BytesIO(b"unrelated"), ingester, CONFIG)
    documents.delete("file.txt", ingester)
    assert documents.list_documents()[0]["name"] == "other.txt"
    for record in (first, second):
        assert state.uploads.get_object(record["operation_id"], "source", "file.txt") is not None
    assert documents.read_source("other.txt") == b"unrelated"
    assert state.uploads.head("alpha", other["document_id"]) == other["operation_id"]
    assert [hit.payload["text"] for hit in vectors.dense_search(VECTOR, limit=10, filters={})] == ["unrelated"]
