"""Standalone RAG HTTP service and tenant storage boundary tests."""

from __future__ import annotations

import io
from html.parser import HTMLParser
from pathlib import Path
from types import SimpleNamespace

import pytest
from fastapi.testclient import TestClient
from rag_mcp.config import RagConfig, RagConfigError
from rag_mcp.http_service import RagHttpSettings, create_service_app
from rag_mcp.migrate import discover_legacy_sources, namespace_for_legacy_user
from rag_mcp.service_knowledge import (
    IndexedDocument,
    KnowledgeDocumentStore,
    KnowledgePaths,
    KnowledgeValidationError,
    RagServiceRuntime,
)

NAMESPACE = "user-0123456789abcdef"
AUTH = {"Authorization": "Bearer test-token", "X-RAG-Namespace": NAMESPACE}


class FakeRuntime:
    def __init__(self) -> None:
        self.documents: dict[str, list[dict[str, object]]] = {}
        self.calls: list[tuple[str, str]] = []
        self.search_arguments: list[dict[str, object]] = []
        self.search_result: dict[str, object] | None = None

    def status(self) -> dict[str, object]:
        return {"status": "ok", "service": "rag-mcp-service", "collection": "test_hybrid"}

    def list_documents(self, namespace: str) -> list[dict[str, object]]:
        self.calls.append(("list", namespace))
        return list(self.documents.setdefault(namespace, []))

    def upload(self, namespace: str, items: list[tuple[str, object]], chunking=None) -> dict[str, object]:
        self.calls.append(("upload", namespace))
        imported = []
        for name, readable in items:
            record = {
                "name": name,
                "size": len(readable.read()),
                "document_id": f"doc-{Path(name).stem}",
                "chunk_count": 1,
                "collection": "test_hybrid",
            }
            self.documents.setdefault(namespace, []).append(record)
            imported.append(record)
        return {"imported": imported, "failed": [], "documents": list(self.documents[namespace])}

    def delete(self, namespace: str, name: str) -> dict[str, object]:
        self.calls.append(("delete", namespace))
        current = self.documents.setdefault(namespace, [])
        deleted = next(item for item in current if item["name"] == name)
        current.remove(deleted)
        return {"deleted": deleted, "documents": list(current)}

    def search(self, namespace: str, arguments: dict[str, object]) -> dict[str, object]:
        self.calls.append(("search", namespace))
        self.search_arguments.append(arguments)
        return self.search_result or {"query": arguments["query"], "evidence_status": "no_evidence", "results": []}


@pytest.fixture()
def service() -> tuple[FakeRuntime, TestClient]:
    runtime = FakeRuntime()
    settings = RagHttpSettings(data_root=Path("unused"), token="test-token")
    return runtime, TestClient(create_service_app(runtime, settings=settings))


def test_health_is_available_without_tenant_credentials(service) -> None:
    _runtime, client = service

    response = client.get("/health")

    assert response.status_code == 200
    assert response.json()["service"] == "rag-mcp-service"


def test_runtime_health_reports_model_state_without_loading_models() -> None:
    runtime = object.__new__(RagServiceRuntime)
    runtime.config = RagConfig(
        backend="qdrant",
        embedding_model="local/embedding",
        embedding_device="cpu",
        reranker_model="local/reranker",
        reranker_device="cpu",
    )
    runtime._embedder = SimpleNamespace(is_loaded=True)
    runtime._reranker = SimpleNamespace(is_loaded=False)

    status = runtime.status()

    assert status["models"] == {
        "embedding": {"name": "local/embedding", "device": "cpu", "loaded": True},
        "reranker": {"name": "local/reranker", "device": "cpu", "loaded": False},
    }


def test_standalone_console_is_served_by_the_rag_package(service) -> None:
    _runtime, client = service

    class Assets(HTMLParser):
        def __init__(self):
            super().__init__()
            self.urls = []

        def handle_starttag(self, tag, attrs):
            attributes = dict(attrs)
            if tag == "script" and attributes.get("src"):
                self.urls.append(attributes["src"])
            if tag == "link" and attributes.get("rel") == "stylesheet":
                self.urls.append(attributes["href"])

    for url in ("/", "/console"):
        page = client.get(url)
        assert page.status_code == 200
        assert "星河知识库" in page.text
        assets = Assets()
        assets.feed(page.text)
        assert len(assets.urls) >= 2
        for asset in assets.urls:
            assert asset.startswith("/console/assets/")
            response = client.get(asset)
            assert response.status_code == 200
            assert response.content


def test_remote_service_requires_explicit_mcp_allowlists(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("RAG_MCP_ALLOWED_HOSTS", raising=False)
    monkeypatch.delenv("RAG_MCP_ALLOWED_ORIGINS", raising=False)
    monkeypatch.setenv("RAG_SERVICE_HOST", "0.0.0.0")
    monkeypatch.setenv("RAG_SERVICE_TOKEN", "test-token")

    with pytest.raises(RagConfigError, match="RAG_MCP_ALLOWED_HOSTS"):
        RagHttpSettings.from_env()

    monkeypatch.setenv("RAG_MCP_ALLOWED_HOSTS", "rag.example.com")
    monkeypatch.setenv("RAG_MCP_ALLOWED_ORIGINS", "https://app.example.com")
    settings = RagHttpSettings.from_env()

    assert settings.mcp_allowed_hosts == ("rag.example.com",)
    assert settings.mcp_allowed_origins == ("https://app.example.com",)


def test_tenant_api_requires_token_and_valid_namespace(service) -> None:
    _runtime, client = service

    assert client.get("/v1/documents").status_code == 401
    assert client.get("/v1/documents", headers={"Authorization": "Bearer test-token"}).status_code == 400
    invalid = {"Authorization": "Bearer test-token", "X-RAG-Namespace": "../other"}
    assert client.get("/v1/documents", headers=invalid).status_code == 400

    standalone = {"Authorization": "Bearer test-token", "X-RAG-Namespace": "product-manuals"}
    assert client.get("/v1/documents", headers=standalone).status_code == 200


def test_upload_list_search_and_delete_use_header_namespace(service) -> None:
    runtime, client = service

    uploaded = client.post(
        "/v1/documents",
        headers=AUTH,
        files=[("files", ("manual.md", io.BytesIO(b"E03 evidence"), "text/markdown"))],
    )
    listing = client.get("/v1/documents", headers=AUTH)
    search = client.post("/v1/search", headers=AUTH, json={"query": "E03"})
    deleted = client.delete("/v1/documents/manual.md", headers=AUTH)

    assert uploaded.status_code == 200
    assert set(listing.json()["supported_extensions"]) == {".docx", ".md", ".pdf", ".txt"}
    assert listing.json()["documents"][0]["name"] == "manual.md"
    assert search.json()["evidence_status"] == "no_evidence"
    assert runtime.search_arguments[0]["candidate_k"] == 10
    assert deleted.json()["documents"] == []
    assert runtime.calls == [
        ("list", NAMESPACE),
        ("upload", NAMESPACE),
        ("list", NAMESPACE),
        ("search", NAMESPACE),
        ("list", NAMESPACE),
        ("delete", NAMESPACE),
    ]


def test_http_search_keeps_image_base64_for_direct_clients(service) -> None:
    runtime, client = service
    runtime.search_result = {
        "query": "figure", "evidence_status": "found",
        "results": [{"asset": {"type": "image", "mime_type": "image/png", "data": "aW1hZ2U="}}],
    }

    response = client.post("/v1/search", headers=AUTH, json={"query": "figure"})

    assert response.status_code == 200
    assert response.json()["results"][0]["asset"]["data"] == "aW1hZ2U="


class FakeIngester:
    def __init__(self) -> None:
        self.deleted: list[str] = []

    def ingest(self, path: Path, *, source_root: Path, chunking=None) -> IndexedDocument:
        assert path.parent == source_root
        if "FAIL_INDEX" in path.read_text(encoding="utf-8"):
            raise RuntimeError("simulated index failure")
        return IndexedDocument(f"doc-{path.stem}", 1, "test_hybrid")

    def delete(self, document_id: str) -> None:
        self.deleted.append(document_id)


def test_service_owned_store_rolls_back_failed_replacement(tmp_path: Path) -> None:
    store = KnowledgeDocumentStore(KnowledgePaths(tmp_path / NAMESPACE))
    ingester = FakeIngester()
    store.store_and_index("guide.md", io.BytesIO(b"first"), ingester)

    with pytest.raises(RuntimeError, match="simulated"):
        store.store_and_index("guide.md", io.BytesIO(b"FAIL_INDEX"), ingester)

    assert (store.paths.knowledge_dir / "guide.md").read_text(encoding="utf-8") == "first"
    assert store.list_documents()[0]["document_id"] == "doc-guide"


def test_service_owned_store_accepts_document_formats_and_confines_names(tmp_path: Path) -> None:
    store = KnowledgeDocumentStore(KnowledgePaths(tmp_path / NAMESPACE))

    store.validate_uploads(
        [
            ("manual.pdf", io.BytesIO(b"pdf")),
            ("guide.docx", io.BytesIO(b"docx")),
        ]
    )

    with pytest.raises(KnowledgeValidationError, match="暂不支持"):
        store.validate_uploads([("legacy.doc", io.BytesIO(b"doc"))])

    assert store.sanitize_name("../outside.md") == "_outside.md"
    assert store.paths.knowledge_dir in store._confined_target("safe.md").parents


def test_legacy_migration_preview_discovers_sources_without_moving_them(tmp_path: Path) -> None:
    user_id = "123e4567-e89b-12d3-a456-426614174000"
    source = tmp_path / user_id / "rag" / "knowledge" / "manual.md"
    source.parent.mkdir(parents=True)
    source.write_text("E03", encoding="utf-8")

    found = discover_legacy_sources(tmp_path, set())

    assert found == [(user_id, [source])]
    assert namespace_for_legacy_user(user_id).startswith("user-")
    assert source.exists()
