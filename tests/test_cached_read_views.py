"""Public read caches never replace authoritative mutation or permission reads."""

import json
from threading import RLock
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient
from sqlalchemy import event, insert

from rag_mcp.catalog import NamespaceCatalog, PromptDefinition, PromptMessage
from rag_mcp.http_service import RagHttpSettings, create_service_app
from rag_mcp.knowledge_bases import public_settings
from rag_mcp.service_knowledge import KnowledgeValidationError, RagServiceRuntime
from rag_mcp.state.read_views import CachedReadViews
from rag_mcp.state.schema import document_heads
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores


class FakeSync:
    def __init__(self, engine):
        self.engine = engine
        self.entries = {}
        self.loads = []
        self.loaders = {}

    def register(self, kind, loader):
        self.loaders[kind] = loader

    def flush_pending(self):
        pass

    def start(self):
        pass

    def stop(self):
        pass

    def close(self):
        pass

    def get_or_load(self, kind, scope, loader):
        key = (kind, scope)
        if key not in self.entries:
            with self.engine.connect() as conn:
                self.loads.append((key, conn))
                value = loader(conn)
            self.entries[key] = json.dumps(value)
        return json.loads(self.entries[key])


@pytest.fixture()
def state():
    stores = StateStores(StateSettings(url="sqlite+pysqlite:///:memory:"))
    stores.cache_sync = FakeSync(stores.engine)
    stores.views = CachedReadViews(stores)
    yield stores
    stores.close()


def settings(namespace="alpha"):
    return {"namespace": namespace, "collection": "active-index", "embedding_model": "active-model",
            "reranker_model": "", "vector_size": 2, "tokenizer": "test-tokenizer",
            "embedding": {"base_url": "https://provider.example", "protected_key": "secret-provider-key"},
            "reranker": {"base_url": "", "protected_key": ""},
            "strategy": "character", "chunk_size": 120, "chunk_overlap": 20}


def record(name="document.txt"):
    return {"name": name, "document_id": "doc-" + name, "collection": "old-index", "chunk_count": 3,
            "chunking": {"strategy": "character", "embedding_model": "old-model"}}


def runtime_for(state):
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = state
    runtime._inference_lock = RLock()
    runtime._tenants = {}
    return runtime


def forbid_queries(conn, cursor, statement, parameters, context, executemany):
    pytest.fail("Cache hit unexpectedly queried the authoritative database")


def test_public_settings_are_redacted_before_caching_and_hit_avoids_database(state):
    raw = settings()
    state.knowledge_bases.create(raw)
    assert state.views.knowledge_base("alpha") == public_settings(raw)
    payload = state.cache_sync.entries[("kb_public", "alpha")]
    assert "protected_key" not in payload
    assert "secret-provider-key" not in payload
    assert json.loads(payload)["embedding"]["api_key_set"] is True
    event.listen(state.engine, "before_cursor_execute", forbid_queries)
    try:
        assert state.views.knowledge_base("alpha") == public_settings(raw)
    finally:
        event.remove(state.engine, "before_cursor_execute", forbid_queries)
    assert state.knowledge_bases.get("alpha")["embedding"]["protected_key"] == "secret-provider-key"


def test_missing_views_are_cached_per_namespace(state):
    assert state.views.knowledge_base("absent") is None
    assert state.views.catalog("absent") is None
    assert state.views.documents("absent") == []
    event.listen(state.engine, "before_cursor_execute", forbid_queries)
    try:
        assert state.views.knowledge_base("absent") is None
        assert state.views.catalog("absent") is None
        assert state.views.documents("absent") == []
    finally:
        event.remove(state.engine, "before_cursor_execute", forbid_queries)
    state.knowledge_bases.create(settings("other"))
    assert state.views.knowledge_base("other")["namespace"] == "other"


def test_list_merges_configuration_and_legacy_counts_with_two_queries_on_one_connection(state):
    for namespace in ("zulu", "alpha", "empty"):
        state.knowledge_bases.create(settings(namespace))
    for namespace in ("alpha", "legacy"):
        state.documents.upsert(namespace, record())
    state.documents.upsert("legacy", record("second.txt"))
    connections = []

    def count_queries(conn, cursor, statement, parameters, context, executemany):
        connections.append(conn)

    event.listen(state.engine, "before_cursor_execute", count_queries)
    try:
        items = state.views.knowledge_bases()
        assert state.views.knowledge_bases() == items
    finally:
        event.remove(state.engine, "before_cursor_execute", count_queries)
    assert len(connections) == 2
    assert connections[0] is connections[1] is state.cache_sync.loads[0][1]
    assert [(item["namespace"], item["configured"], item["document_count"]) for item in items] == [
        ("alpha", True, 1), ("empty", True, 0), ("legacy", False, 2), ("zulu", True, 0)]
    assert "secret-provider-key" not in state.cache_sync.entries[("kb_list", "")]


def test_document_view_overlays_active_config_and_preserves_repository_record(state):
    state.knowledge_bases.create(settings())
    for name in ("zulu.txt", "Alpha.txt"):
        state.documents.upsert("alpha", record(name))
    with state.engine.begin() as conn:
        conn.execute(insert(document_heads).values(namespace="alpha", document_id="doc-Alpha.txt",
                                                  version="published-version", updated_at=1.0))
    connections = []

    def count_queries(conn, cursor, statement, parameters, context, executemany):
        connections.append(conn)

    event.listen(state.engine, "before_cursor_execute", count_queries)
    try:
        records = state.views.documents("alpha")
    finally:
        event.remove(state.engine, "before_cursor_execute", count_queries)
    assert len(connections) == 2
    assert connections[0] is connections[1]
    assert [item["name"] for item in records] == ["Alpha.txt", "zulu.txt"]
    assert records[0]["operation_id"] == "published-version"
    assert all(item["collection"] == "active-index" for item in records)
    assert all(item["chunking"]["embedding_model"] == "active-model" for item in records)
    assert state.documents.list("alpha")[0]["collection"] == "old-index"
    state.documents.upsert("legacy", record())
    assert state.views.documents("legacy")[0]["collection"] == "old-index"


def test_runtime_read_hits_never_resolve_tenant_or_query_database(state):
    state.knowledge_bases.create(settings())
    state.documents.upsert("alpha", record())
    runtime = runtime_for(state)
    runtime._tenant = lambda namespace: pytest.fail("Public read must not resolve tenant resources")
    expected = runtime.list_documents("alpha")
    public = runtime.public_knowledge_base("alpha")
    bases = runtime.list_knowledge_bases()
    event.listen(state.engine, "before_cursor_execute", forbid_queries)
    try:
        assert runtime.list_documents("alpha") == expected
        assert runtime.public_knowledge_base("alpha") == public
        assert runtime.list_knowledge_bases() == bases
    finally:
        event.remove(state.engine, "before_cursor_execute", forbid_queries)
    with pytest.raises(KnowledgeValidationError, match="创建知识库"):
        runtime.public_knowledge_base("missing")


def test_authoritative_document_snapshot_bypasses_cached_public_view(state):
    state.documents.upsert("alpha", record())
    runtime = runtime_for(state)
    assert len(runtime.list_documents("alpha")) == 1
    state.documents.upsert("alpha", record("second.txt"))
    runtime._tenant = lambda namespace: SimpleNamespace(
        lock=RLock(), documents=SimpleNamespace(list_documents=lambda: state.documents.list(namespace)))
    assert len(runtime.authoritative_documents("alpha")) == 2
    assert len(runtime.list_documents("alpha")) == 1


def test_catalog_snapshots_cache_by_default_and_mutations_read_authoritative_state(state):
    catalog = NamespaceCatalog(state, "alpha")
    assert not catalog.snapshot().prompts
    prompt = PromptDefinition(messages=[PromptMessage(role="user", text="Initial")])
    catalog.put_prompt("first", prompt)
    assert not catalog.snapshot().prompts
    assert catalog.snapshot(authoritative=True).prompts["first"] == prompt
    catalog.put_prompt("second", prompt)
    assert set(catalog.snapshot(authoritative=True).prompts) == {"first", "second"}
    state.cache_sync.entries.clear()
    assert set(catalog.snapshot().prompts) == {"first", "second"}
    event.listen(state.engine, "before_cursor_execute", forbid_queries)
    try:
        assert set(catalog.snapshot().prompts) == {"first", "second"}
    finally:
        event.remove(state.engine, "before_cursor_execute", forbid_queries)


@pytest.mark.parametrize("cached_reader", [False, True])
def test_http_prefers_cached_public_reader_with_legacy_fixture_fallback(state, cached_reader):
    raw = settings()
    calls = []

    def authoritative(namespace):
        calls.append("authoritative")
        return raw

    runtime = SimpleNamespace(state=state, knowledge_base=authoritative)
    if cached_reader:
        def cached(namespace):
            calls.append("cached")
            return public_settings(raw)
        runtime.public_knowledge_base = cached
    app = create_service_app(runtime, settings=RagHttpSettings())
    with TestClient(app, base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        for url in ("/v1/admin/knowledge-bases/alpha", "/v1/knowledge-base"):
            response = client.get(url, headers={"X-RAG-Namespace": "alpha"})
            assert response.status_code == 200
            assert response.json()["embedding"] == public_settings(raw)["embedding"]
            assert "secret-provider-key" not in response.text
    assert calls == (["cached"] * 2 if cached_reader else ["authoritative"] * 2)


def test_reembedding_uses_authoritative_document_counts_not_public_cache(state, monkeypatch):
    raw = settings()
    raw["embedding"]["protected_key"] = ""
    state.knowledge_bases.create(raw)
    state.documents.upsert("alpha", record())
    state.views.documents("alpha")
    state.documents.upsert("alpha", record("second.txt"))
    runtime = runtime_for(state)
    runtime.list_documents = lambda namespace: pytest.fail("Reembedding must not read public cache")
    observed = []

    class Source:
        def __init__(self, **kwargs):
            pass

        def reembed_into(self, collection, embedder, counts):
            observed.append(counts)

    monkeypatch.setattr("rag_mcp.storage.qdrant_store.QdrantStore", Source)
    monkeypatch.setattr("rag_mcp.service_knowledge.HTTPEmbedder",
                        lambda connection: SimpleNamespace(embed_query=lambda text: [1.0, 0.0]))
    runtime._client = object()
    runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert observed == [{"doc-document.txt": 3, "doc-second.txt": 3}]
    assert len(json.loads(state.cache_sync.entries[("documents", "alpha")])) == 1


def test_registered_refresh_loaders_use_supplied_connection_and_redact_secrets(state):
    state.knowledge_bases.create(settings())
    state.documents.upsert("alpha", record())
    assert {"kb_public", "kb_list", "documents", "catalog"} <= set(state.cache_sync.loaders)
    with state.engine.connect() as conn:
        for kind, scope in (("kb_public", "alpha"), ("kb_list", ""),
                            ("documents", "alpha"), ("catalog", "alpha")):
            value = state.cache_sync.loaders[kind](conn, scope)
            assert "secret-provider-key" not in json.dumps(value)
            assert "protected_key" not in json.dumps(value)
    assert not state.cache_sync.entries
