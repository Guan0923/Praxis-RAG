"""HTTP boundary and storage tests; real inference is covered separately."""

import io
import json
import subprocess
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from fastapi.testclient import TestClient

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.config import RagConfig
from rag_mcp.http_service import RagHttpSettings, create_service_app
from rag_mcp.model_api import HTTPEmbedder, HTTPReranker, ModelAPIError, ModelConnection, ModelHTTPClient
from rag_mcp.service_knowledge import RagServiceRuntime


@pytest.fixture()
def provider():
    state = {"requests": [], "key": "", "size": 3, "sizes": {}, "error": None, "delay": 0}

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            self.handle_request(None)

        def do_POST(self):
            self.handle_request(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))

        def handle_request(self, body):
            state["requests"].append((self.path, dict(self.headers), body))
            time.sleep(state["delay"])
            status = 200
            if state["key"] and self.headers.get("Authorization") != "Bearer " + state["key"]:
                status, result = 401, {"detail": "private upstream error"}
            elif state["error"]:
                status, result = state["error"]
            elif self.path == "/custom/v1/models":
                result = {"data": [{"id": "embed-a"}, {"id": "rank-a"}]}
            elif self.path == "/custom/v1/embeddings":
                result = {"data": [{"index": i, "embedding": [float(i + 1)] * state["sizes"].get(body["model"], state["size"])}
                                   for i in reversed(range(len(body["input"])))]}
            elif self.path == "/custom/v1/rerank":
                result = {"results": [{"index": i, "relevance_score": 1 / (i + 1)}
                                      for i in reversed(range(len(body["documents"])))]}
            else:
                status, result = 404, {}
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            try:
                self.wfile.write(json.dumps(result).encode())
            except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
                pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}/custom/v1", state
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


@pytest.fixture()
def runtime(tmp_path):
    service = RagServiceRuntime(RagConfig(backend="qdrant", qdrant_path=tmp_path / "qdrant"), tmp_path / "data")
    yield service
    service._client.close()


def local_client(runtime, **kwargs):
    return TestClient(create_service_app(runtime, settings=RagHttpSettings(data_root=runtime.data_root)),
                      base_url="http://127.0.0.1", client=("127.0.0.1", 12345), **kwargs)


def create(runtime, url, namespace="alpha", model="embed-a", key=""):
    return runtime.create_knowledge_base(namespace, ChunkingConfig("character", 120, 20), model,
                                         embedding={"base_url": url, "apikey": key})


def test_connection_models_and_per_base_secret_storage(runtime, provider):
    url, state = provider
    with local_client(runtime) as client:
        response = client.post("/v1/admin/knowledge-bases/models/embedding", json={"base_url": url, "apikey": "private-test-key"})
        assert response.json() == {"models": ["embed-a", "rank-a"]}
        assert state["requests"][-1][1]["Authorization"] == "Bearer private-test-key"
        created = client.post("/v1/admin/knowledge-bases", json={"namespace": "alpha", "embedding_model": "embed-a",
                              "embedding": {"base_url": url, "apikey": "private-test-key"}, "strategy": "character",
                              "chunk_size": 120, "chunk_overlap": 20})
        assert created.status_code == 201, created.text
        assert created.json()["embedding"] == {"base_url": url, "api_key_set": True}
        for path in ("/v1/admin/knowledge-bases", "/v1/admin/knowledge-bases/alpha", "/v1/knowledge-base"):
            result = client.get(path, headers={"X-RAG-Namespace": "alpha"})
            assert "private-test-key" not in result.text and "protected_key" not in result.text
        revealed = client.get("/v1/admin/knowledge-bases/alpha/key/embedding")
        assert revealed.json() == {"apikey": "private-test-key"}
        assert revealed.headers["cache-control"] == "no-store"
        client.post("/v1/admin/knowledge-bases/models/embedding", json={"namespace": "alpha", "base_url": url})
        assert state["requests"][-1][1]["Authorization"] == "Bearer private-test-key"
        client.post("/v1/admin/knowledge-bases/models/embedding", json={"namespace": "alpha", "base_url": url.replace("127.0.0.1", "localhost")})
        assert "Authorization" not in state["requests"][-1][1]
        assert client.get("/v1/model-settings").status_code == 404


def test_model_change_is_blocked_without_reembedding(runtime, provider):
    url, state = provider
    initial = create(runtime, url)
    with local_client(runtime) as client:
        state["requests"].clear()
        response = client.patch("/v1/admin/knowledge-bases/alpha", json={"embedding_model": "embed-b"})
        assert response.status_code == 409 and "re embedding" in response.text
        assert not state["requests"]
        assert client.get("/v1/admin/knowledge-bases/alpha").json() == initial


def test_reembedding_publishes_complete_index_and_survives_restart(runtime, provider):
    from uuid import uuid4
    from qdrant_client import models
    from rag_mcp.knowledge_bases import model_connection

    url, state = provider
    state["sizes"] = {"embed-a": 3, "embed-b": 5}
    alpha = create(runtime, url, key="alpha-key")
    beta = create(runtime, url, "beta", key="beta-key")
    for namespace in ("alpha", "beta"):
        result = runtime.upload(namespace, [("same.txt", io.BytesIO((namespace + " document. ").encode() * 30))])
        assert len(result["imported"]) == 1, result
    document = runtime.list_documents("alpha")[0]
    asset = {"namespace": "alpha", "document_id": document["document_id"], "record_type": "asset", "asset_id": "test-image", "caption": "preserved asset"}
    runtime._client.upsert(collection_name=alpha["collection"], points=[models.PointStruct(id=str(uuid4()), vector={}, payload=asset)])
    old_points, _ = runtime._client.scroll(collection_name=alpha["collection"], limit=100, with_vectors=True)
    old_manifest = (runtime.data_root / "alpha/manifest.json").read_bytes()
    with local_client(runtime) as client:
        response = client.patch("/v1/admin/knowledge-bases/alpha", json={"embedding_model": "embed-b", "reembed": True})
        assert response.status_code == 200, response.text
        updated = response.json()
        assert updated["vector_size"] == 5 and updated["collection"] != alpha["collection"]
        new_points, _ = runtime._client.scroll(collection_name=updated["collection"], limit=100, with_vectors=True)
        assert {point.id for point in new_points} == {point.id for point in old_points}
        assert any(point.payload == asset for point in new_points)
        assert all(len(point.vector["dense"]) == 5 for point in new_points if point.payload["record_type"] == "chunk")
        assert runtime._client.scroll(collection_name=alpha["collection"], limit=100, with_vectors=True)[0] == old_points
        assert (runtime.data_root / "alpha/manifest.json").read_bytes() == old_manifest
        assert runtime.knowledge_base("beta")["collection"] == beta["collection"]
        assert model_connection(runtime.knowledge_base("beta"), "embedding").api_key == "beta-key"
        record = runtime.list_documents("alpha")[0]
        assert record["collection"] == updated["collection"] and record["chunking"]["embedding_model"] == "embed-b"
        assert runtime.search("alpha", {"query": "document", "retrieval": "embedding", "rerank": "none"})["results"]
        assert state["requests"][-1][2]["model"] == "embed-b"
        assert state["requests"][-1][1]["Authorization"] == "Bearer alpha-key"
    config, root = runtime.config, runtime.data_root
    runtime._client.close()
    restarted = RagServiceRuntime(config, root)
    try:
        assert restarted.list_documents("alpha")[0]["collection"] == updated["collection"]
        assert restarted.search("alpha", {"query": "document", "retrieval": "embedding", "rerank": "none"})["results"]
    finally:
        restarted._client.close()


@pytest.mark.parametrize("failure", ["upsert", "publish"])
def test_failed_reembedding_preserves_old_configuration_and_index(runtime, provider, monkeypatch, failure):
    from rag_mcp.knowledge_bases import KnowledgeBaseStore

    url, state = provider
    original = create(runtime, url)
    runtime.upload("alpha", [("one.txt", io.BytesIO(b"Original document. " * 30))])
    collections = {item.name for item in runtime._client.get_collections().collections}
    settings = runtime.knowledge_base("alpha")
    manifest = (runtime.data_root / "alpha/manifest.json").read_bytes()
    if failure == "upsert":
        upsert = runtime._client.upsert
        def fail_after_write(*args, **kwargs):
            upsert(*args, **kwargs)
            raise ModelAPIError("controlled partial reembedding failure")
        monkeypatch.setattr(runtime._client, "upsert", fail_after_write)
    else:
        def fail_publish(*_args):
            raise ModelAPIError("controlled configuration commit failure")
        monkeypatch.setattr(KnowledgeBaseStore, "update", fail_publish)
    with local_client(runtime) as client:
        response = client.patch("/v1/admin/knowledge-bases/alpha", json={"embedding_model": "embed-b", "reembed": True})
        assert response.status_code == 400, response.text
    assert runtime.knowledge_base("alpha") == settings
    assert runtime.list_documents("alpha")[0]["collection"] == original["collection"]
    assert (runtime.data_root / "alpha/manifest.json").read_bytes() == manifest
    assert {item.name for item in runtime._client.get_collections().collections} == collections
    assert runtime.search("alpha", {"query": "document", "retrieval": "embedding", "rerank": "none"})["results"]


def test_key_and_reranker_edits_do_not_reembed(runtime, provider):
    url, state = provider
    original = create(runtime, url)
    with local_client(runtime) as client:
        response = client.patch("/v1/admin/knowledge-bases/alpha", json={"embedding": {"apikey": "new-key"},
                                "reranker_model": "rank-a", "reranker": {"base_url": url}})
        assert response.status_code == 200, response.text
        assert response.json()["collection"] == original["collection"]
        assert response.json()["reranker_model"] == "rank-a"
        assert client.get("/v1/admin/knowledge-bases/alpha/key/embedding").json()["apikey"] == "new-key"
        assert client.patch("/v1/admin/knowledge-bases/alpha", json={"embedding": {"apikey": ""}}).status_code == 200
        assert client.get("/v1/admin/knowledge-bases/alpha/key/embedding").json()["apikey"] == ""


@pytest.mark.parametrize("headers", [{"Origin": "https://evil.example"}, {"Host": "remote.example"},
                                    {"X-Forwarded-For": "127.0.0.1"}, {"Sec-Fetch-Site": "cross-site"}])
def test_management_requires_direct_local_access(runtime, provider, headers):
    create(runtime, provider[0])
    with local_client(runtime) as client:
        assert client.get("/v1/admin/knowledge-bases/alpha", headers=headers).status_code == 403
        assert client.get("/v1/admin/knowledge-bases/alpha/key/embedding", headers=headers).status_code == 403
        assert client.patch("/v1/admin/knowledge-bases/alpha", headers=headers, json={}).status_code == 403
        assert client.post("/v1/admin/knowledge-bases/models/embedding", headers=headers, json={}).status_code == 403
        response = client.patch("/v1/admin/knowledge-bases/alpha", json={"embedding_model": [], "embedding": {"apikey": "never-echo-this"}})
        assert response.status_code == 422 and "never-echo-this" not in response.text
