"""Knowledge-base creation owns all later indexing settings."""

import io

import pytest
from fastapi.testclient import TestClient

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.config import RagConfig
from rag_mcp.http_service import RagHttpSettings, create_service_app
from rag_mcp.knowledge_bases import KnowledgeBaseStore
from test_model_api_settings import provider  # noqa: F401
from rag_mcp.service_knowledge import KnowledgeValidationError, RagServiceRuntime


class Embedder:
    def embed_documents(self, texts):
        return [[1.0, 0.0, 0.0] for _ in texts]

    def embed_query(self, text):
        return [1.0, 0.0, 0.0]


@pytest.fixture()
def runtime(tmp_path, provider):
    service = RagServiceRuntime(RagConfig(backend="qdrant", qdrant_path=tmp_path / "qdrant",
                                        embedding_model="test-model", vector_size=3), tmp_path / "data")
    service.provider_url = provider[0]
    yield service
    service._client.close()


def test_creation_is_immutable_and_persisted(runtime):
    settings = runtime.create_knowledge_base("alpha", ChunkingConfig("character", 120, 20), "test-model", embedding={"base_url": runtime.provider_url})
    assert runtime.knowledge_base("alpha")["collection"] == settings["collection"]
    with pytest.raises(KnowledgeValidationError, match="已存在"):
        runtime.create_knowledge_base("alpha", ChunkingConfig("character", 300, 40), "test-model", embedding={"base_url": runtime.provider_url})
    assert runtime.knowledge_base("alpha")["chunk_size"] == 120


def test_upload_and_mcp_write_use_created_settings(runtime):
    runtime.create_knowledge_base("alpha", ChunkingConfig("character", 120, 20), "test-model", embedding={"base_url": runtime.provider_url})
    uploaded = runtime.upload("alpha", [("first.txt", io.BytesIO(b"One document. " * 30))])
    written = runtime.write_document("alpha", "second.txt", b"Another document. " * 30, overwrite=False)
    updated = runtime.write_document("alpha", "second.txt", b"Updated document. " * 30, overwrite=True)
    for record in (uploaded["imported"][0], written["document"], updated["document"]):
        assert {key: record["chunking"][key] for key in ("strategy", "chunk_size", "chunk_overlap")} == {
            "strategy": "character", "chunk_size": 120, "chunk_overlap": 20,
        }
    record = uploaded["imported"][0]
    checked = runtime.check_documents("alpha", [{"name": "renamed.txt", "sha256": record["sha256"]}])
    assert checked["files"][0]["document"]["name"] == "first.txt"
    with pytest.raises(KnowledgeValidationError, match="创建知识库"):
        runtime.upload("not-created", [("test.txt", io.BytesIO(b"No implicit creation"))])


def test_http_rejects_per_upload_configuration_and_allows_local_creation(runtime):
    app = create_service_app(runtime, settings=RagHttpSettings(data_root=runtime.data_root))
    with TestClient(app, base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        created = client.post("/v1/admin/knowledge-bases", json={"namespace": "alpha", "embedding_model": "test-model", "embedding": {"base_url": runtime.provider_url},
                              "strategy": "character", "chunk_size": 120, "chunk_overlap": 20})
        assert created.status_code == 201
        headers = {"X-RAG-Namespace": "alpha"}
        assert client.get("/v1/knowledge-base", headers=headers).json()["chunk_size"] == 120
        response = client.post("/v1/documents", headers=headers, files={"files": ("test.txt", b"Upload text.")},
                               data={"chunk_size": "999"})
        assert response.status_code == 400
        assert client.post("/v1/documents/check", headers=headers,
                           json={"files": [{"name": "test.txt", "sha256": "a" * 64}], "strategy": "token"}).status_code == 422
        key = client.post("/v1/admin/api-keys", json={"name": "client", "namespaces": ["alpha"]}).json()["key"]
        headers["Authorization"] = "Bearer " + key
        assert client.post("/v1/admin/knowledge-bases", headers=headers,
                           json={"namespace": "beta", "embedding_model": "test-model", "embedding": {"base_url": runtime.provider_url}}).status_code == 201
        uploaded = client.post("/v1/documents", headers=headers, files={"files": ("test.txt", b"Upload text.")})
        assert uploaded.json()["imported"][0]["chunking"]["chunk_size"] == 120


def test_list_includes_document_workspaces_without_creation_record(runtime):
    runtime.create_knowledge_base("praxis", ChunkingConfig("character", 120, 20), "test-model", embedding={"base_url": runtime.provider_url})
    runtime.upload("praxis", [("existing.txt", io.BytesIO(b"Existing document. " * 30))])
    KnowledgeBaseStore(runtime.data_root).path.unlink()
    app = create_service_app(runtime, settings=RagHttpSettings(data_root=runtime.data_root))
    with TestClient(app, base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        response = client.get("/v1/admin/knowledge-bases")
        assert response.json()["knowledge_bases"][0]["namespace"] == "praxis"
        assert client.get("/v1/documents", headers={"X-RAG-Namespace": "praxis"}).json()["documents"]


def test_list_knowledge_bases_reads_current_store(runtime):
    app = create_service_app(runtime, settings=RagHttpSettings(data_root=runtime.data_root))
    with TestClient(app, base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        response = client.get("/v1/admin/knowledge-bases")
        assert response.json() == {"knowledge_bases": []}
        runtime.create_knowledge_base("beta", ChunkingConfig(), "test-model", embedding={"base_url": runtime.provider_url})
        runtime.create_knowledge_base("alpha", ChunkingConfig(), "test-model", embedding={"base_url": runtime.provider_url})
        response = client.get("/v1/admin/knowledge-bases")
        assert response.status_code == 200
        assert [item["namespace"] for item in response.json()["knowledge_bases"]] == ["alpha", "beta"]
        assert response.headers["cache-control"] == "no-store"
        assert client.get("/v1/admin/knowledge-bases", headers={"X-Forwarded-For": "127.0.0.1"}).status_code == 403
    with TestClient(app, base_url="http://127.0.0.1", client=("192.0.2.1", 1234)) as client:
        assert client.get("/v1/admin/knowledge-bases").status_code == 403
