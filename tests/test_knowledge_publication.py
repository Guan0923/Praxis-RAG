"""Index publication must survive lost commit acknowledgements and post-commit failures."""

import os
import threading
from types import SimpleNamespace

import pytest
from sqlalchemy import delete, update
from sqlalchemy.engine import make_url

from rag_mcp.config import RagConfig
from rag_mcp.service_knowledge import KnowledgeError, RagServiceRuntime
from rag_mcp.state.schema import document_leases
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores


@pytest.fixture
def publication(tmp_path, monkeypatch):
    import rag_mcp.service_knowledge as module
    from rag_mcp.storage.qdrant_store import QdrantStore

    url = os.getenv("RAG_TEST_PUBLICATION_MYSQL_URL")
    if url:
        assert make_url(url).database.startswith("praxis_publication_test_"), "Disposable schema only"
    state = StateStores(StateSettings(url=url or f"sqlite+pysqlite:///{tmp_path / 'publication.sqlite3'}"))
    if url:
        from rag_mcp.state.schema import metadata

        with state.engine.begin() as conn:
            for table in reversed(metadata.sorted_tables):
                conn.execute(delete(table))
    state.knowledge_bases.create({
        "namespace": "alpha", "collection": "old", "embedding_model": "model", "reranker_model": "", "vector_size": 3,
        "tokenizer": "unused", "strategy": "character", "chunk_size": 100, "chunk_overlap": 10,
        "embedding": {"base_url": "http://unused.invalid/v1"}, "reranker": {},
    })
    deleted, built = [], []
    runtime = object.__new__(RagServiceRuntime)
    runtime.state = state
    runtime.config = RagConfig(backend="qdrant")
    runtime._inference_lock = threading.RLock()
    runtime._tenants = {"alpha": object()}
    runtime._client = SimpleNamespace(
        collection_exists=lambda _: True, delete_collection=lambda collection: deleted.append(collection))

    class Embedder:
        def __init__(self, connection):
            self.connection = connection

        def embed_query(self, _text):
            return [1, 0, 0]

    monkeypatch.setattr(module, "HTTPEmbedder", Embedder)
    monkeypatch.setattr(QdrantStore, "reembed_into", lambda _self, collection, *_: built.append(collection))
    yield state, runtime, built, deleted
    state.close()


def test_reembed_success_keeps_new_index(publication):
    state, runtime, built, deleted = publication
    result = runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert result["collection"] == built[0] == state.knowledge_bases.get("alpha")["collection"]
    assert deleted == []
    assert "alpha" not in runtime._tenants


def test_construction_failure_only_deletes_unpublished_index(publication, monkeypatch):
    from rag_mcp.storage.qdrant_store import QdrantStore

    state, runtime, built, deleted = publication

    def fail(_self, collection, *_):
        built.append(collection)
        raise ValueError("construction failed")

    monkeypatch.setattr(QdrantStore, "reembed_into", fail)
    with pytest.raises(ValueError, match="construction failed"):
        runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert state.knowledge_bases.get("alpha")["collection"] == "old"
    assert deleted == built


def test_post_commit_cache_failure_does_not_delete_active_index(publication, monkeypatch):
    state, runtime, built, deleted = publication

    original = state.cache_sync.flush_pending

    def fail(*args, **kwargs):
        if any(kind == "kb_public" for kind, _scope in kwargs.get("keys", ())):
            raise OSError("cache notification failed")
        return original(*args, **kwargs)

    monkeypatch.setattr(state.cache_sync, "flush_pending", fail)
    result = runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert result["collection"] == built[0] == state.knowledge_bases.get("alpha")["collection"]
    assert deleted == []


def test_lease_expiry_before_publication_never_overwrites_newer_config(publication, monkeypatch):
    from rag_mcp.storage.qdrant_store import QdrantStore

    state, runtime, built, deleted = publication
    state.uploads.lease_seconds = 300

    def newer(_self, collection, *_):
        built.append(collection)
        with state.engine.begin() as conn:
            conn.execute(update(document_leases).where(
                document_leases.c.resource == "namespace:exclusive").values(expires_at=0))
        with state.uploads.lock("alpha"):
            current = state.knowledge_bases.get("alpha")
            state.knowledge_bases.update({**current, "collection": "newer"})

    monkeypatch.setattr(QdrantStore, "reembed_into", newer)
    with pytest.raises(KnowledgeError, match="回滚"):
        runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert state.knowledge_bases.get("alpha")["collection"] == "newer"
    assert deleted == built
