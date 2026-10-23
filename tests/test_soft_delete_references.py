"""Soft deletion hides live access but leaves issued references immutable."""

import io
import json
from types import SimpleNamespace

import pytest
from qdrant_client import QdrantClient
from sqlalchemy import create_engine, inspect, select, text

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.config import RagConfig
from rag_mcp.service_knowledge import DenseDocumentIngester, KnowledgeDocumentStore
from rag_mcp.state.cache import CacheSettings, RedisStateCache
from rag_mcp.state.schema import documents
from rag_mcp.state.schema_migrations import migrate_soft_delete
from rag_mcp.state.search_references import SearchReferences
from rag_mcp.state.store import StateStores
from rag_mcp.storage.qdrant_store import QdrantStore
from rag_mcp.sparse import Bm25SparseEncoder

VECTOR = [1.0] + [0.0] * 1023
CONFIG = ChunkingConfig("character", 100, 10)


@pytest.fixture()
def stack(tmp_path, monkeypatch):
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    state = StateStores.create(tmp_path / "state")
    state.cache_sync.backend = RedisStateCache(CacheSettings(enabled=False))
    client = QdrantClient(":memory:")
    store = QdrantStore(collection="soft_delete", namespace="ns", client=client,
                        active_versions=lambda: state.documents.visible_versions("ns"))
    ingester = DenseDocumentIngester(RagConfig(backend="qdrant", collection="soft_delete"),
        SimpleNamespace(embed_documents=lambda texts: [VECTOR for _ in texts]), Bm25SparseEncoder(), store)
    source = KnowledgeDocumentStore(state, "ns")
    yield state, store, ingester, source
    state.close()
    client.close()


def upload(stack, content=b"original immutable evidence"):
    return stack[3].store_and_index("file.txt", io.BytesIO(content), stack[2], CONFIG)


def snapshot(stack):
    state, store, _, _ = stack
    points = store.dense_search(VECTOR, limit=10, filters={})
    assert points
    payload = {"results": [{**p.payload, "score": p.score} for p in points]}
    return SearchReferences(state).capture("ns", payload)


def test_delete_sets_flag_retains_bytes_and_new_search_hides_document(stack):
    state, store, ingester, source = stack
    record = upload(stack)
    issued = snapshot(stack)
    assert issued["snapshot_available"] and issued["result_id"]
    artifact = next(a for a in issued["artifacts"] if a["kind"] == "source")
    source.delete("file.txt", ingester)
    with state.engine.connect() as conn:
        row = conn.execute(select(documents).where(documents.c.name == "file.txt")).one()
        assert row.del_flag is True
    assert state.documents.get("ns", "file.txt") is None
    assert state.documents.get_by_id("ns", record["document_id"]) is None
    assert state.documents.candidates("ns", record["sha256"]) == []
    assert state.documents.list("ns") == []
    assert state.documents.namespaces() == {}
    assert state.views.documents("ns") == []
    assert state.views.published_versions("ns") == {}
    assert store.dense_search(VECTOR, limit=10, filters={}) == []
    assert state.uploads.get_object(record["operation_id"], "source", "file.txt") == b"original immutable evidence"
    assert state.results.get("ns", issued["result_id"])["results"] == issued["results"]
    assert state.artifacts.read("ns", artifact["artifact_id"])["content"] == b"original immutable evidence"
    with pytest.raises(ValueError):
        state.results.get("other", issued["result_id"])


def test_reupload_revives_live_document_without_mutating_old_reference(stack):
    state, store, ingester, source = stack
    old = upload(stack)
    issued = snapshot(stack)
    source.delete("file.txt", ingester)
    revived = upload(stack, b"replacement evidence")
    assert revived["operation_id"] != old["operation_id"]
    assert not revived.get("skipped")
    assert source.read_source("file.txt") == b"replacement evidence"
    assert [p.payload["text"] for p in store.dense_search(VECTOR, limit=10, filters={})] == ["replacement evidence"]
    assert state.results.get("ns", issued["result_id"])["results"][0]["text"] == "original immutable evidence"
    source_ref = next(a for a in issued["artifacts"] if a["kind"] == "source")
    assert state.artifacts.read("ns", source_ref["artifact_id"])["content"] == b"original immutable evidence"


def test_soft_deleted_legacy_points_are_hidden_without_physical_deletion(stack):
    state, store, ingester, source = stack
    from rag_mcp.chunking import DocumentChunk
    chunk = DocumentChunk("legacy-doc", "legacy-chunk", "legacy", "Legacy", "old.txt", {})
    store.replace_document([chunk], [VECTOR])
    state.documents.upsert("ns", {"name": "old.txt", "document_id": "legacy-doc", "collection": "soft_delete"})
    assert store.dense_search(VECTOR, limit=10, filters={})
    source.delete("old.txt", ingester)
    assert store.dense_search(VECTOR, limit=10, filters={}) == []
    # Index bytes still exist, hidden by authority tombstone, not by a Qdrant side effect.
    points, _ = store._client.scroll("soft_delete", limit=10)
    assert len(points) == 1


def test_additive_soft_delete_migration_preserves_old_rows(tmp_path):
    engine = create_engine("sqlite+pysqlite:///" + (tmp_path / "old.sqlite3").as_posix())
    with engine.begin() as conn:
        conn.execute(text("CREATE TABLE documents (name TEXT PRIMARY KEY, content TEXT)"))
        conn.execute(text("INSERT INTO documents VALUES ('old.txt','retained')"))
    migrate_soft_delete(engine)
    migrate_soft_delete(engine)
    assert "del_flag" in {c["name"] for c in inspect(engine).get_columns("documents")}
    with engine.connect() as conn:
        assert tuple(conn.execute(text("SELECT content, del_flag FROM documents")).one()) == ("retained", 0)
    engine.dispose()


def test_original_hit_image_and_table_refs_survive_soft_delete_and_cleanup(stack):
    import base64
    state, _, ingester, source = stack
    record = upload(stack)
    results = [
        {"document_id": record["document_id"], "metadata": {"upload_version": record["operation_id"], "asset_id": "picture"},
         "asset": {"type": "image", "mime_type": "image/png", "data": base64.b64encode(b"original-image").decode()}},
        {"document_id": record["document_id"], "metadata": {"upload_version": record["operation_id"], "asset_id": "table"},
         "asset": {"type": "table", "rows": [["A", "1"]]}},
    ]
    issued = SearchReferences(state).capture("ns", {"results": results})
    source.delete("file.txt", ingester)
    # Old source object GC must not break already-signed artifacts.
    from rag_mcp.state.schema import upload_objects
    from sqlalchemy import delete
    with state.engine.begin() as conn:
        conn.execute(delete(upload_objects).where(upload_objects.c.operation_id == record["operation_id"]))
    for hit in issued["results"]:
        data = state.artifacts.read("ns", hit["asset"]["artifact_id"])["content"]
        if hit["asset"]["type"] == "image":
            assert data == b"original-image"
        else:
            assert json.loads(data)["rows"] == [["A", "1"]]
    assert state.results.get("ns", issued["result_id"])["results"] == issued["results"]


def test_pdf_source_json_markdown_references_keep_exact_bytes_after_delete(stack, tmp_path, monkeypatch):
    from rag_mcp import pdf_assets
    from rag_mcp.chunking import DocumentChunk, _stable_id
    from rag_mcp.pdf_assets import ParsedPdf
    state, store, ingester, source = stack
    document_id = _stable_id("doc", "paper.pdf")
    def parse(_path, **options):
        folder = options["workspace"]
        folder.mkdir(parents=True)
        for name, value in (("document.md", b"PDF markdown"), ("manifest.json", b'{"blocks":[]}'),
                            ("document.json", b'{"name":"Paper"}'), ("ocr.json", b"[]")):
            (folder / name).write_bytes(value)
        return ParsedPdf([DocumentChunk(document_id, "c", "PDF evidence", "Paper", "paper.pdf", {})], [], folder)
    monkeypatch.setattr(pdf_assets, "parse_pdf", parse)
    record = source.store_and_index("paper.pdf", io.BytesIO(b"PDF source bytes"), ingester, CONFIG)
    issued = snapshot(stack)
    by_kind = {ref["kind"]: ref for ref in issued["artifacts"]}
    assert set(by_kind) == {"source", "markdown", "manifest", "document", "ocr"}
    source.delete("paper.pdf", ingester)
    assert state.documents.get("ns", "paper.pdf") is None
    assert state.artifacts.read("ns", by_kind["source"]["artifact_id"])["content"] == b"PDF source bytes"
    assert state.artifacts.read("ns", by_kind["markdown"]["artifact_id"])["content"] == b"PDF markdown"
    assert state.artifacts.read("ns", by_kind["document"]["artifact_id"])["content"] == b'{"name":"Paper"}'
    assert issued["expires_at"] - issued["created_at"] == 3600
    assert all(ref["version"] == record["operation_id"] for ref in by_kind.values())


def test_name_based_runtime_reads_reject_soft_delete_but_uuid_reads_work(stack):
    from threading import RLock
    from rag_mcp.service_knowledge import RagServiceRuntime, KnowledgeValidationError
    state, store, ingester, source = stack
    upload(stack)
    issued = snapshot(stack)
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = state
    runtime._inference_lock = RLock()
    runtime._tenant = lambda _ns: SimpleNamespace(documents=source, lock=RLock())
    source.delete("file.txt", ingester)
    for reader in (runtime.read_document_file, runtime.read_document):
        with pytest.raises(KnowledgeValidationError, match="does not exist"):
            reader("ns", "file.txt")
    source_id = next(a["artifact_id"] for a in issued["artifacts"] if a["kind"] == "source")
    assert runtime.download_artifact("ns", source_id)[0] == b"original immutable evidence"
    assert runtime.get_search_result("ns", issued["result_id"])["results"] == issued["results"]
