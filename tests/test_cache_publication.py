"""Publication boundaries exercised with real ingestion, SQL and in-memory Qdrant."""

from __future__ import annotations

import asyncio
import copy
import io
import json
from contextlib import contextmanager
from dataclasses import replace
from threading import Event, RLock, Thread
from types import SimpleNamespace
from unittest.mock import Mock

import pytest
from qdrant_client import QdrantClient
from sqlalchemy import event, select

from rag_mcp.catalog import NamespaceCatalog, ToolPatch
from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.config import RagConfig
from rag_mcp.knowledge_bases import public_settings
from rag_mcp.server import create_registry
from rag_mcp.service_knowledge import DenseDocumentIngester, KnowledgeDocumentStore, RagServiceRuntime
from rag_mcp.sparse import Bm25SparseEncoder
from rag_mcp.state.schema import cache_outbox, cache_revisions
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from rag_mcp.storage.qdrant_store import QdrantStore
from test_cache_synchronization import MemoryCache


CHUNKING = ChunkingConfig("character", 100, 10)
VECTOR = [1.0, 0.0]


@pytest.fixture()
def stack(tmp_path, monkeypatch):
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    # Keep journal workspaces inside pytest's explicitly project-local base directory.
    monkeypatch.setattr("rag_mcp.service_knowledge.tempfile.gettempdir", lambda: str(tmp_path))
    state = StateStores(StateSettings(url="sqlite+pysqlite:///" + (tmp_path / "state.db").as_posix()))
    state.cache_sync.backend = MemoryCache()
    state.uploads.auto_heartbeat = False
    client = QdrantClient(":memory:")
    config = RagConfig(backend="qdrant", collection="uploads", vector_size=2,
                       embedding_model="original-model", chunk_tokenizer_model="unused-tokenizer")
    vectors = QdrantStore(collection=config.collection, vector_size=2, namespace="alpha", client=client,
                          active_versions=lambda: state.documents.visible_versions("alpha"))
    embedder = SimpleNamespace(embed_documents=Mock(side_effect=lambda texts: [VECTOR for _ in texts]))
    ingester = DenseDocumentIngester(config, embedder, Bm25SparseEncoder(), vectors)
    settings = {"namespace": "alpha", "collection": config.collection, "vector_size": 2,
                "embedding_model": config.embedding_model, "reranker_model": "reranker-model",
                "tokenizer": config.chunk_tokenizer_model,
                **CHUNKING.metadata(config.chunk_tokenizer_model),
                "embedding": {"base_url": "https://embedding.invalid", "protected_key": "embedding-secret"},
                "reranker": {"base_url": "https://reranker.invalid", "protected_key": "reranker-secret"}}
    state.knowledge_bases.create(settings)
    documents = KnowledgeDocumentStore(state, "alpha")
    try:
        yield SimpleNamespace(state=state, vectors=vectors, ingester=ingester, documents=documents,
                              embedder=embedder, settings=settings, config=config, client=client)
    finally:
        state.close()
        client.close()


def upload(stack, content=b"original document text", name="file.txt"):
    return stack.documents.store_and_index(name, io.BytesIO(content), stack.ingester, CHUNKING,
                                           tokenizer_model=stack.config.chunk_tokenizer_model,
                                           embedding_model=stack.config.embedding_model)


def table_rows(state, table):
    with state.engine.connect() as conn:
        return [dict(row) for row in conn.execute(select(table).order_by(*table.primary_key.columns)).mappings()]


PUBLIC_KINDS = {"documents", "kb_list", "kb_public", "catalog"}


def public_entries(values):
    return {key: value for key, value in values.items() if key[0] in PUBLIC_KINDS}


def public_rows(rows):
    return [row for row in rows if row["kind"] in PUBLIC_KINDS]


@contextmanager
def no_sql(state):
    def forbidden(*_):
        pytest.fail("A warm public read unexpectedly queried SQL")

    event.listen(state.engine, "before_cursor_execute", forbidden)
    try:
        yield
    finally:
        event.remove(state.engine, "before_cursor_execute", forbidden)


def test_upload_atomically_publishes_and_synchronously_refreshes_lists(stack, monkeypatch):
    state, backend = stack.state, stack.state.cache_sync.backend
    assert backend.entries["documents", "alpha"] == []
    assert backend.entries["kb_list", ""][0]["document_count"] == 0
    before = copy.deepcopy(backend.entries)
    before_watermarks = dict(backend.watermarks)
    original_stage = stack.vectors.stage_version
    original_commit = state.uploads.commit
    observed = []

    def stage(*args):
        count = original_stage(*args)
        assert count > 0
        assert stack.documents.list_documents() == []
        assert stack.vectors.dense_search(VECTOR, limit=10, filters={}) == []
        assert public_entries(backend.entries) == public_entries(before)
        observed.append("staged")
        return count

    def commit(op, result, conn):
        committed = original_commit(op, result, conn)
        assert conn.in_transaction()
        assert conn.scalar(select(cache_outbox.c.id).where(cache_outbox.c.status == "PENDING"))
        assert public_entries(backend.entries) == public_entries(before)
        assert public_entries(backend.watermarks) == public_entries(before_watermarks)
        observed.append("uncommitted")
        return committed

    monkeypatch.setattr(stack.vectors, "stage_version", stage)
    monkeypatch.setattr(state.uploads, "commit", commit)
    result = upload(stack, b"character chunk content. " * 12)
    assert observed == ["staged", "uncommitted"]
    assert result["chunk_count"] > 1
    assert stack.embedder.embed_documents.call_count == 1
    assert state.uploads.get(result["operation_id"])["status"] == "COMMITTED"
    assert state.uploads.head("alpha", result["document_id"]) == result["operation_id"]
    assert backend.entries["documents", "alpha"][0]["operation_id"] == result["operation_id"]
    assert backend.entries["kb_list", ""][0]["document_count"] == 1
    for key in (("documents", "alpha"), ("kb_list", "")):
        assert backend.watermarks[key] == before_watermarks[key] + 1
    assert all(row["status"] == "DONE" for row in table_rows(state, cache_outbox))
    with no_sql(state):
        records = state.views.documents("alpha")
        assert records[0]["document_id"] == result["document_id"]
        assert records[0]["chunk_count"] == result["chunk_count"]
        assert state.views.knowledge_bases()[0]["document_count"] == 1
    assert len(stack.vectors.dense_search(VECTOR, limit=20, filters={})) == result["chunk_count"]


def test_failed_publication_rolls_back_outbox_revisions_and_never_refreshes(stack, monkeypatch):
    state, backend = stack.state, stack.state.cache_sync.backend
    first = upload(stack)
    before_entries = copy.deepcopy(backend.entries)
    before_watermarks = dict(backend.watermarks)
    before_outbox = table_rows(state, cache_outbox)
    before_revisions = table_rows(state, cache_revisions)
    put = Mock(wraps=backend.put)
    monkeypatch.setattr(backend, "put", put)
    original_commit = state.uploads.commit
    attempted = []

    def fail_after_publication(op, result, conn):
        original_commit(op, result, conn)
        attempted.append(op["id"])
        assert conn.scalar(select(cache_outbox.c.id).where(cache_outbox.c.status == "PENDING"))
        assert public_entries(backend.entries) == public_entries(before_entries)
        raise OSError("injected publication transaction failure")

    monkeypatch.setattr(state.uploads, "commit", fail_after_publication)
    with pytest.raises(OSError, match="publication transaction failure"):
        upload(stack, b"replacement must never become visible")
    assert len(attempted) == 1
    assert state.uploads.get(attempted[0])["status"] == "ROLLED_BACK"
    assert public_rows(table_rows(state, cache_outbox)) == public_rows(before_outbox)
    assert public_rows(table_rows(state, cache_revisions)) == public_rows(before_revisions)
    state.cache_sync.flush_pending()
    assert not any(call.args[0] in PUBLIC_KINDS for call in put.call_args_list)
    assert public_entries(backend.entries) == public_entries(before_entries)
    assert public_entries(backend.watermarks) == public_entries(before_watermarks)
    assert state.uploads.head("alpha", first["document_id"]) == first["operation_id"]
    assert stack.documents.read_source("file.txt") == b"original document text"
    assert [hit.payload["text"] for hit in stack.vectors.dense_search(VECTOR, limit=10, filters={})] == [
        "original document text"]


def test_delete_synchronously_refreshes_documents_and_knowledge_base_count(stack):
    state, backend = stack.state, stack.state.cache_sync.backend
    first = upload(stack)
    before = dict(backend.watermarks)
    stack.documents.delete("file.txt", stack.ingester)
    assert backend.entries["documents", "alpha"] == []
    assert backend.entries["kb_list", ""][0]["document_count"] == 0
    for key in (("documents", "alpha"), ("kb_list", "")):
        assert backend.watermarks[key] == before[key] + 1
    with no_sql(state):
        assert state.views.documents("alpha") == []
        assert state.views.knowledge_bases()[0]["document_count"] == 0
    assert state.uploads.head("alpha", first["document_id"]) == first["operation_id"]
    assert state.documents.visible_versions("alpha")[first["document_id"]] == ""
    assert state.views.published_versions("alpha") == {}
    assert state.blobs.get("alpha", "source", "file.txt") == b"original document text"
    assert stack.vectors.dense_search(VECTOR, limit=10, filters={}) == []
    assert all(row["status"] == "DONE" for row in table_rows(state, cache_outbox))


def test_config_publication_refreshes_active_index_overlay_without_reembedding(stack):
    state, backend = stack.state, stack.state.cache_sync.backend
    first = upload(stack)
    stored = state.documents.get("alpha", "file.txt")
    before = dict(backend.watermarks)
    stack.embedder.embed_documents.reset_mock()
    updated = {**stack.settings, "collection": "reembedded-index", "embedding_model": "new-model"}
    state.knowledge_bases.update(updated)
    stack.embedder.embed_documents.assert_not_called()
    for key in (("documents", "alpha"), ("kb_list", ""), ("kb_public", "alpha")):
        assert backend.watermarks[key] == before[key] + 1
    with no_sql(state):
        document = state.views.documents("alpha")[0]
        assert document["collection"] == "reembedded-index"
        assert document["chunking"]["embedding_model"] == "new-model"
        assert document["operation_id"] == first["operation_id"]
        assert state.views.knowledge_base("alpha") == public_settings(updated)
        listed = state.views.knowledge_bases()[0]
        assert listed["collection"] == "reembedded-index"
        assert listed["embedding_model"] == "new-model"
        assert listed["document_count"] == 1
    assert state.documents.get("alpha", "file.txt") == stored
    assert stored["collection"] == "uploads"
    assert stored["chunking"]["embedding_model"] == "original-model"


def test_runtime_cache_hits_skip_sql_and_tenant_construction(stack, monkeypatch):
    upload(stack)
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = stack.state
    runtime._inference_lock = RLock()
    runtime._tenants = {}
    tenant = Mock(side_effect=AssertionError("Public reads must not construct a tenant"))
    monkeypatch.setattr(runtime, "_tenant", tenant)
    with no_sql(stack.state):
        assert len(runtime.list_documents("alpha")) == 1
        assert runtime.public_knowledge_base("alpha") == public_settings(stack.settings)
        assert runtime.list_knowledge_bases()[0]["document_count"] == 1
    tenant.assert_not_called()
    assert runtime._tenants == {}


def test_all_read_view_loaders_cache_only_redacted_model_configuration(stack):
    upload(stack)
    state = stack.state
    backend = state.cache_sync.backend
    backend.entries.clear()
    assert state.views.knowledge_base("alpha") == public_settings(stack.settings)
    assert state.views.knowledge_bases()[0]["document_count"] == 1
    assert len(state.views.documents("alpha")) == 1
    payloads = list(backend.entries.values())
    with state.engine.connect() as conn:
        for kind, scope in (("kb_public", "alpha"), ("kb_list", ""), ("documents", "alpha")):
            payloads.append(state.cache_sync.loaders[kind](conn, scope))
    encoded = json.dumps(payloads)
    assert "protected_key" not in encoded
    assert "embedding-secret" not in encoded
    assert "reranker-secret" not in encoded
    for settings in (backend.entries["kb_public", "alpha"], backend.entries["kb_list", ""][0]):
        assert settings["embedding"] == {"base_url": "https://embedding.invalid", "api_key_set": True}
        assert settings["reranker"] == {"base_url": "https://reranker.invalid", "api_key_set": True}
    assert state.knowledge_bases.get("alpha")["embedding"]["protected_key"] == "embedding-secret"


@pytest.mark.parametrize("mode", ["sync", "async_sync_handler", "async_handler"])
def test_actual_tool_call_rechecks_sql_when_cached_catalog_still_enables_tool(stack, monkeypatch, mode):
    state = stack.state
    backend = state.cache_sync.backend
    catalog = NamespaceCatalog(state, "alpha")
    execute = Mock(return_value={"results": []})
    registry = create_registry(SimpleNamespace(search=execute), catalog)
    name = next(iter(registry.entries))
    if mode == "async_handler":
        async def async_search(request):
            return execute(request)
        registry.entries[name] = replace(registry.entries[name], execute=async_search)
    assert name in [tool.name for tool in registry.definitions()]
    cached = copy.deepcopy(backend.entries["catalog", "alpha"])
    backend.down = True
    try:
        assert catalog.patch_tool(name, ToolPatch(enabled=False), {"enabled": True, "description": ""})
    finally:
        backend.down = False
    assert backend.entries["catalog", "alpha"] == cached
    assert name in [tool.name for tool in registry.definitions()]
    monkeypatch.setattr(backend, "get", Mock(side_effect=AssertionError("Execution must bypass cached metadata")))
    statements = []

    def record_sql(conn, cursor, statement, parameters, context, executemany):
        statements.append(statement)

    event.listen(state.engine, "before_cursor_execute", record_sql)
    try:
        if mode == "sync":
            result = registry.invoke(name, {"query": "evidence"})
        else:
            result = asyncio.run(registry.invoke_async(name, {"query": "evidence"}))
    finally:
        event.remove(state.engine, "before_cursor_execute", record_sql)
    assert result.is_error
    assert "disabled" in result.content[0].text
    execute.assert_not_called()
    assert any("SELECT" in statement.upper() and "mcp_catalogs" in statement for statement in statements)
    backend.get.assert_not_called()


def test_paused_snapshot_fill_cannot_overwrite_newer_config_refresh(stack, monkeypatch):
    upload(stack)
    state, backend = stack.state, stack.state.cache_sync.backend
    old = backend.entries.pop(("documents", "alpha"))
    paused, release = Event(), Event()
    original_put = backend.put
    results, errors, rejected = [], [], []

    def delayed_put(kind, scope, revision, value, deleted=False):
        if kind == "documents" and value and value[0]["collection"] == "uploads":
            paused.set()
            assert release.wait(5), "Timed out waiting for the newer refresh"
            accepted = original_put(kind, scope, revision, value, deleted)
            rejected.append(not accepted)
            return accepted
        return original_put(kind, scope, revision, value, deleted)

    def reader():
        try:
            results.append(state.views.documents("alpha"))
        except BaseException as exc:
            errors.append(exc)

    monkeypatch.setattr(backend, "put", delayed_put)
    thread = Thread(target=reader, daemon=True)
    thread.start()
    try:
        assert paused.wait(5), "Reader did not reach its snapshot fill"
        state.knowledge_bases.update({**stack.settings, "collection": "new-index", "embedding_model": "new-model"})
        assert backend.entries["documents", "alpha"][0]["collection"] == "new-index"
    finally:
        release.set()
        thread.join(5)
    assert not thread.is_alive()
    assert errors == []
    assert results == [old]
    assert rejected == [True]
    with no_sql(state):
        document = state.views.documents("alpha")[0]
        assert document["collection"] == "new-index"
        assert document["chunking"]["embedding_model"] == "new-model"
