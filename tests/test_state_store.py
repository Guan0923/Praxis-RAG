"""Shared state database: repositories, blobs, encryption and migrations."""

import base64
import hashlib
import json
import sqlite3

import pytest

from rag_mcp.api_keys import ApiKeyStore
from rag_mcp.knowledge_bases import KnowledgeBaseStore
from rag_mcp.state.migrate import main as migrate_main
from rag_mcp.state.schema import metadata
from rag_mcp.state.store import StateStores

KEY = "base64:" + base64.b64encode(b"k" * 32).decode()


@pytest.fixture()
def stores(tmp_path, monkeypatch):
    monkeypatch.setenv("RAG_STATE_ENCRYPTION_KEY", KEY)
    state = StateStores.create(tmp_path / "state")
    yield state
    state.close()


def test_api_key_lifecycle_and_portable_encryption(tmp_path, stores):
    store = ApiKeyStore(stores)
    created = store.create("app", ["alpha", "beta"])
    assert store.namespaces_for_key(created["key"]) == ["alpha", "beta"]
    assert store.secret_for_namespace(created["id"], "alpha") == created["key"]
    assert all("key" not in item for item in store.list())
    with pytest.raises(PermissionError):
        store.secret_for_namespace(created["id"], "other")
    assert created["key"].encode() not in (tmp_path / "state" / "state.sqlite3").read_bytes()
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


def test_legacy_migration_imports_sqlite_files_and_blobs(tmp_path, monkeypatch):
    root = tmp_path / "legacy"
    target = tmp_path / "target"
    alpha = root / "alpha"
    (alpha / "knowledge" / ".pdf-assets" / "doc-a").mkdir(parents=True)
    (alpha / "knowledge" / "a.txt").write_bytes(b"legacy text")
    (alpha / "knowledge" / ".pdf-assets" / "doc-a" / "document.md").write_text("saved pdf", encoding="utf-8")
    (alpha / "manifest.json").write_text(json.dumps({"a.txt": {
        "name": "a.txt", "size": 11, "document_id": "doc-a",
        "sha256": hashlib.sha256(b"legacy text").hexdigest(), "chunk_count": 1,
        "collection": "c", "indexed_at": "2026-01-01", "chunking": {},
    }}), encoding="utf-8")
    (alpha / "mcp_catalog.json").write_text('{"tools":{},"prompts":{}}', encoding="utf-8")
    with sqlite3.connect(root / "api-keys.sqlite3") as db:
        db.execute("CREATE TABLE api_keys (id TEXT PRIMARY KEY, name TEXT, prefix TEXT, digest TEXT,"
                   " namespaces TEXT, enabled INTEGER, created_at TEXT, protected_key TEXT)")
        db.execute("INSERT INTO api_keys VALUES (?,?,?,?,?,?,?,?)",
                   ("k1", "App", "prk_x", hashlib.sha256(b"prk_secret").hexdigest(),
                    '["alpha"]', 1, "2026-01-01", None))
    with sqlite3.connect(root / "knowledge-bases.sqlite3") as db:
        db.execute("CREATE TABLE knowledge_bases (namespace TEXT PRIMARY KEY, settings TEXT)")
        db.execute("INSERT INTO knowledge_bases VALUES (?,?)", ("alpha", json.dumps({
            "namespace": "alpha", "collection": "c", "vector_size": 3,
            "embedding_model": "m", "reranker_model": "",
            "embedding": {"base_url": "u", "protected_key": ""},
            "reranker": {"base_url": "", "protected_key": ""},
            "tokenizer": "t", "strategy": "character", "chunk_size": 100, "chunk_overlap": 10,
        })))
    monkeypatch.setenv("RAG_STATE_ENCRYPTION_KEY", KEY)
    monkeypatch.setenv("RAG_STATE_URL", "sqlite+pysqlite:///" + (target / "state.sqlite3").as_posix())
    assert migrate_main(["--legacy-root", str(root), "--apply"]) == 0
    assert migrate_main(["--legacy-root", str(root), "--apply"]) == 0

    state = StateStores.create(target)
    try:
        assert state.api_keys.namespaces_for_key("prk_secret") == ["alpha"]
        assert state.knowledge_bases.get("alpha")["collection"] == "c"
        assert state.documents.get("alpha", "a.txt")["document_id"] == "doc-a"
        assert state.blobs.get("alpha", "source", "a.txt") == b"legacy text"
        assert state.blobs.get_text("alpha", "pdf-assets", "doc-a/document.md") == "saved pdf"
        assert json.loads(state.catalogs.load("alpha")) == {"tools": {}, "prompts": {}}
    finally:
        state.close()
