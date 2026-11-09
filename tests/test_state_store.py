"""Shared state database: repositories, blobs, encryption and migrations."""

import base64

import pytest
from sqlalchemy import select

from rag_mcp.api_keys import ApiKeyStore
from rag_mcp.knowledge_bases import KnowledgeBaseStore
from rag_mcp.state.schema import api_keys as api_keys_table, metadata
from rag_mcp.state.store import StateStores

KEY = "base64:" + base64.b64encode(b"k" * 32).decode()


@pytest.fixture()
def stores(monkeypatch):
    monkeypatch.setenv("RAG_STATE_ENCRYPTION_KEY", KEY)
    state = StateStores.create()
    yield state
    state.close()


def test_api_key_lifecycle_and_portable_encryption(stores):
    store = ApiKeyStore(stores)
    created = store.create("app", ["alpha", "beta"])
    assert store.namespaces_for_key(created["key"]) == ["alpha", "beta"]
    assert store.secret_for_namespace(created["id"], "alpha") == created["key"]
    assert all("key" not in item for item in store.list())
    with pytest.raises(PermissionError):
        store.secret_for_namespace(created["id"], "other")
    with stores.engine.connect() as conn:
        stored = [dict(row) for row in conn.execute(select(api_keys_table)).mappings()]
    assert stored
    assert created["key"] not in repr(stored)
    assert store.update(created["id"], "renamed", ["beta"], False)["enabled"] is False
    with pytest.raises(ValueError, match="停用"):
        store.namespaces_for_key(created["key"])
    assert store.delete(created["id"]) is True


def test_knowledge_base_versioning_and_duplicates(stores):
    store = KnowledgeBaseStore(stores)
    store.create({"namespace": "alpha", "collection": "c1", "value": 1})
    assert stores.knowledge_bases.version("alpha") == 1
    store.update({"namespace": "alpha", "collection": "c2", "value": 2})
    assert stores.knowledge_bases.version("alpha") == 2
    assert store.get("alpha")["value"] == 2
    with pytest.raises(LookupError):
        store.create({"namespace": "alpha", "collection": "c3"})
    with pytest.raises(LookupError):
        store.update({"namespace": "missing", "collection": "c4"})
    assert store.delete("alpha") is True
    assert store.get("alpha") is None


def test_documents_and_blobs_round_trip(stores):
    document = {"name": "a.txt", "document_id": "doc-a", "size": 5, "sha256": "0" * 64,
                "chunk_count": 1, "collection": "c", "chunking": {"strategy": "character"},
                "diagnostics": [{"kind": "note"}], "skipped_object_count": 2}
    stores.documents.upsert("alpha", document)
    stores.blobs.put("alpha", "source", "a.txt", b"hello", document_id="doc-a")
    stores.blobs.put("alpha", "pdf-assets", "doc-a/document.md", b"saved", document_id="doc-a")
    assert stores.documents.list("alpha")[0]["name"] == "a.txt"
    assert stores.blobs.get("alpha", "source", "a.txt") == b"hello"
    assert stores.blobs.get_text("alpha", "pdf-assets", "doc-a/document.md") == "saved"
    assert stores.blobs.list_keys("alpha", "pdf-assets", document_id="doc-a") == ["doc-a/document.md"]
    stores.documents.delete("alpha", "a.txt")
    stores.blobs.delete_document("alpha", "pdf-assets", "doc-a")
    assert stores.documents.list("alpha") == []
    assert stores.blobs.get("alpha", "pdf-assets", "doc-a/document.md") is None


def test_document_names_and_namespaces_are_case_sensitive(stores):
    stores.documents.upsert("Alpha", {"name": "Doc.txt", "document_id": "d1", "collection": "c"})
    stores.documents.upsert("alpha", {"name": "Doc.txt", "document_id": "d2", "collection": "c"})
    assert stores.documents.get("Alpha", "Doc.txt")["document_id"] == "d1"
    assert stores.documents.get("alpha", "Doc.txt")["document_id"] == "d2"
    assert stores.documents.get("Alpha", "doc.txt") is None


def test_blob_update_rolls_back_with_transaction(stores):
    stores.blobs.put("alpha", "pdf-assets", "doc/document.md", b"previous")
    with pytest.raises(OSError):
        with stores.engine.begin() as conn:
            stores.blobs.put("alpha", "pdf-assets", "doc/document.md", b"next", conn=conn)
            raise OSError("controlled")
    assert stores.blobs.get("alpha", "pdf-assets", "doc/document.md") == b"previous"


def test_catalog_mutation_is_persisted_and_atomic(stores):
    from rag_mcp.catalog import CatalogStore, PromptDefinition, ToolPatch

    catalog = CatalogStore(stores).for_namespace("alpha")
    assert catalog.patch_tool("search", ToolPatch(enabled=False), {}) is True
    assert catalog.patch_tool("search", ToolPatch(enabled=False), {}) is False
    assert CatalogStore(stores).for_namespace("alpha").snapshot().tools["search"]["enabled"] is False
    prompt = PromptDefinition(messages=[{"role": "user", "text": "hi {{name}}"}],
                              arguments=[{"name": "name"}])
    assert catalog.put_prompt("greet", prompt) is True
    assert catalog.delete_prompt("greet") is True
    with pytest.raises(KeyError):
        catalog.delete_prompt("greet")


def test_mysql_schema_compiles_with_long_columns():
    from sqlalchemy.dialects import mysql
    from sqlalchemy.schema import CreateTable

    ddl = "\n".join(str(CreateTable(table).compile(dialect=mysql.dialect())) for table in metadata.sorted_tables)
    assert "LONGBLOB" in ddl
    assert "LONGTEXT" in ddl
    assert "utf8mb4_bin" in ddl
