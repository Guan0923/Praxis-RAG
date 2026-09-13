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
    state = {"requests": [], "key": "", "size": 3, "error": None, "delay": 0}

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
                result = {"data": [{"index": i, "embedding": [float(i + 1)] * state["size"]}
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


def save(runtime, url, kind="embedding", **fields):
    return runtime.model_action(kind, {"base_url": url, "model": "embed-a" if kind == "embedding" else "rank-a", **fields}, "save")


def test_empty_key_prefix_and_response_order(provider):
    url, state = provider
    connection = ModelConnection(url, "embed-a")
    assert ModelHTTPClient(connection).models() == ["embed-a", "rank-a"]
    assert HTTPEmbedder(connection).embed_documents(["one", "two"]) == [[1.0] * 3, [2.0] * 3]
    assert HTTPReranker(connection).scores("query", ["one", "two"]) == [(0, 1.0), (1, 0.5)]
    assert all("Authorization" not in headers for _, headers, _ in state["requests"])


@pytest.mark.parametrize("failure", [(404, {}), (200, []), (200, {"data": [{"index": 0, "embedding": []}]}),
                                    (200, {"data": [{"index": True, "embedding": [1]}]}),
                                    (200, {"data": [{"index": 0, "embedding": [float("nan")]}]})])
def test_failed_save_keeps_live_and_saved_settings(runtime, provider, failure):
    url, state = provider
    save(runtime, url)
    before = runtime._settings_store.path.read_bytes()
    original = runtime._embedder
    state["error"] = failure
    with pytest.raises(ModelAPIError):
        save(runtime, url, model="bad")
    assert runtime._embedder is original
    assert runtime._settings_store.path.read_bytes() == before


def test_auth_key_preservation_clear_and_protected_restart(runtime, provider):
    url, state = provider
    state["key"] = "test-only-api-secret"
    with local_client(runtime) as client:
        body = {"base_url": url, "model": "embed-a", "apikey": state["key"]}
        response = client.patch("/v1/model-settings/embedding", json=body)
        assert response.status_code == 200, response.text
        assert response.json()["api_key_set"]
        assert state["key"] not in response.text
        assert state["key"] not in runtime._settings_store.path.read_text()
        reread, _ = runtime._settings_store.load({}, "unused")
        assert reread["embedding"].api_key == state["key"]
        body.pop("apikey")
        assert client.patch("/v1/model-settings/embedding", json=body).status_code == 200
        response = client.post("/v1/model-settings/embedding/test", json={**body, "apikey": "wrong"})
        assert response.status_code == 400 and "401" in response.text
        assert runtime._connections["embedding"].api_key == state["key"]
        state["key"] = ""
        response = client.patch("/v1/model-settings/embedding", json={**body, "apikey": ""})
        assert response.status_code == 200 and not response.json()["api_key_set"]
        assert "Authorization" not in state["requests"][-1][1]


def test_address_change_does_not_forward_saved_key(runtime, provider):
    url, state = provider
    save(runtime, url, apikey="test-secret")
    alias = url.replace("127.0.0.1", "localhost")
    runtime.model_action("embedding", {"base_url": alias}, "test")
    assert "Authorization" not in state["requests"][-1][1]


def test_new_process_restores_settings_and_decrypts_key(runtime, provider):
    url, state = provider
    state["key"] = "process-test-only-key"
    save(runtime, url, apikey=state["key"])
    root, qdrant = str(runtime.data_root), str(runtime.config.qdrant_path)
    runtime._client.close()
    script = """
import json, sys
from pathlib import Path
from rag_mcp.config import RagConfig
from rag_mcp.service_knowledge import RagServiceRuntime
runtime = RagServiceRuntime(RagConfig(backend='qdrant', qdrant_path=Path(sys.argv[2])), Path(sys.argv[1]))
try:
    runtime.model_action('embedding', {}, 'test')
    print(json.dumps(runtime.model_settings()))
finally:
    runtime._client.close()
"""
    result = subprocess.run([sys.executable, "-c", script, root, qdrant],
                            capture_output=True, text=True, check=True, timeout=30)
    restored = json.loads(result.stdout)["embedding"]
    assert restored["model"] == "embed-a" and restored["vector_size"] == 3
    assert restored["api_key_set"] and restored["status"] == "ok"
    assert state["key"] not in result.stdout + result.stderr


def test_storage_failure_does_not_replace_runtime(runtime, provider, monkeypatch):
    url, _ = provider
    save(runtime, url)
    before = runtime._connections
    def fail(*_args):
        raise ModelAPIError("save failed")
    monkeypatch.setattr(runtime._settings_store, "save", fail)
    with pytest.raises(ModelAPIError, match="save failed"):
        save(runtime, url, kind="reranker")
    assert runtime._connections is before


def test_nonempty_cold_workspace_and_vector_length_guard(runtime, provider):
    url, state = provider
    save(runtime, url)
    runtime.create_knowledge_base("cold", ChunkingConfig("character", 100, 0), 3)
    result = runtime.upload("cold", [("one.txt", io.BytesIO(b"First document for model API storage checks."))])
    assert result["imported"], result
    runtime._tenants.clear()
    with pytest.raises(ModelAPIError, match="已有知识库"):
        save(runtime, url, model="embed-b")
    state["size"] = 4
    with pytest.raises(ModelAPIError, match="已有知识库"):
        save(runtime, url)
    state["size"] = 3
    save(runtime, url, kind="reranker")
    assert runtime.search("cold", {"query": "document", "retrieval": "embedding", "rerank": "cross_encoder"})["results"]
    runtime.delete("cold", "one.txt")
    old_collection = runtime.config.collection
    state["size"] = 5
    with pytest.raises(ModelAPIError, match="已有知识库"):
        save(runtime, url, model="embed-b")
    assert runtime.config.vector_size == 3 and runtime.config.collection == old_collection
    assert runtime._client.collection_exists(old_collection)


def test_existing_points_block_model_change_even_without_manifest(runtime, provider):
    url, _ = provider
    save(runtime, url)
    runtime.create_knowledge_base("orphan", ChunkingConfig("character", 100, 0), 3)
    runtime.upload("orphan", [("one.txt", io.BytesIO(b"Stored points must not be ignored."))])
    (runtime.data_root / "orphan" / "manifest.json").unlink()
    with pytest.raises(ModelAPIError, match="已有知识库"):
        save(runtime, url, model="different")


def test_configuration_waits_for_current_operation_and_refreshes_tenants(runtime, provider):
    url, _ = provider
    save(runtime, url)
    old = runtime._tenant("first")
    runtime._tenant("second")
    with ThreadPoolExecutor() as executor:
        with runtime._inference_lock:
            task = executor.submit(save, runtime, url, "reranker")
            time.sleep(0.05)
            assert not task.done()
        task.result(timeout=5)
    assert runtime._tenant("first") is not old
    assert runtime._tenant("second").orchestrator._rerankers["cross_encoder"] is runtime._reranker


@pytest.mark.parametrize("headers", [{"Origin": "https://evil.example"}, {"Host": "remote.example"},
                                    {"X-Forwarded-For": "127.0.0.1"}, {"Sec-Fetch-Site": "cross-site"}])
def test_remote_and_forwarded_settings_actions_are_rejected(runtime, provider, headers):
    url, state = provider
    with local_client(runtime) as client:
        for suffix, method in [("", "PATCH"), ("/models", "POST"), ("/test", "POST")]:
            response = client.request(method, "/v1/model-settings/embedding" + suffix,
                                      headers=headers, json={"base_url": url, "model": "embed-a"})
            assert response.status_code == 403
        assert client.get("/v1/model-settings", headers=headers).status_code == 403
    assert not state["requests"]


def test_remote_peer_and_service_auth_and_validation_redaction(runtime):
    app = create_service_app(runtime, settings=RagHttpSettings(data_root=runtime.data_root, token="service-key"))
    with TestClient(app, base_url="http://127.0.0.1", client=("192.0.2.10", 12)) as client:
        assert client.get("/v1/model-settings").status_code == 403
        headers = {"Authorization": "Bearer service-key"}
        assert client.get("/v1/model-settings", headers=headers).status_code == 403
        assert client.post("/v1/model-settings/embedding/models", headers=headers, json={}).status_code == 403
    with local_client(runtime) as client:
        response = client.patch("/v1/model-settings/embedding", json={"apikey": "never-echo-this", "model": []})
        assert response.status_code == 422 and "never-echo-this" not in response.text


def test_timeout_and_no_automatic_retry(provider):
    url, state = provider
    state["delay"] = 0.2
    with pytest.raises(ModelAPIError, match="超时"):
        HTTPEmbedder(ModelConnection(url, "test"), timeout=0.03).embed_query("test")
    assert len(state["requests"]) == 1


def test_missing_config_and_offline_are_explicit(runtime):
    with pytest.raises(ModelAPIError, match="尚未配置"):
        runtime._embedder.embed_query("test")
    with pytest.raises(ModelAPIError, match="无法连接"):
        HTTPEmbedder(ModelConnection("http://127.0.0.1:1", "test")).embed_query("test")
