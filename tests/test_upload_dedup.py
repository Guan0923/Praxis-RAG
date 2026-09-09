"""Real HTTP handlers, source storage and tenant locks; no model inference."""

import hashlib
import io
from concurrent.futures import ThreadPoolExecutor

from fastapi.testclient import TestClient

from rag_mcp.chunk_strategies import ChunkingConfig, LEGACY_CONFIG
from rag_mcp.http_service import RagHttpSettings, create_service_app
from test_mcp_http import ProtocolRuntime


def test_dedup_configuration_rename_and_namespace(tmp_path):
    runtime = ProtocolRuntime(tmp_path)
    config = ChunkingConfig()
    first = runtime.upload("alpha", [("first.txt", io.BytesIO(b"unique content"))], config)
    renamed = runtime.upload("alpha", [("renamed.txt", io.BytesIO(b"unique content"))], config)
    assert len(first["imported"]) == 1 and len(renamed["skipped"]) == 1
    assert renamed["skipped"][0]["name"] == "first.txt"
    assert not (tmp_path / "alpha/knowledge/renamed.txt").exists()
    changed = runtime.upload("alpha", [("first.txt", io.BytesIO(b"unique content"))], ChunkingConfig("recursive", 256, 32))
    assert changed["imported"][0]["chunking"]["chunk_size"] == 256
    other = runtime.upload("beta", [("first.txt", io.BytesIO(b"unique content"))], config)
    assert len(other["imported"]) == 1
    changed_strategy = runtime.upload("alpha", [("first.txt", io.BytesIO(b"unique content"))], ChunkingConfig("sentence"))
    assert len(changed_strategy["imported"]) == 1


def test_batch_and_concurrent_duplicates(tmp_path):
    runtime = ProtocolRuntime(tmp_path)
    config = ChunkingConfig()
    batch = runtime.upload("batch", [("a.txt", io.BytesIO(b"same")), ("b.txt", io.BytesIO(b"same"))], config)
    assert (len(batch["imported"]), len(batch["skipped"])) == (1, 1)
    def upload(index):
        return runtime.upload("parallel", [(f"{index}.txt", io.BytesIO(b"parallel"))], config)
    with ThreadPoolExecutor(max_workers=4) as pool:
        results = list(pool.map(upload, range(4)))
    assert sum(len(item["imported"]) for item in results) == 1
    assert sum(len(item["skipped"]) for item in results) == 3


def test_legacy_records_and_missing_sources(tmp_path):
    runtime = ProtocolRuntime(tmp_path)
    runtime.upload("legacy", [("old.txt", io.BytesIO(b"old"))])
    store = runtime._tenant("legacy").documents
    manifest = store._read_manifest()
    manifest["old.txt"].pop("sha256")
    manifest["old.txt"].pop("chunking")
    store._write_manifest(manifest)
    fingerprint = hashlib.sha256(b"old").hexdigest()
    assert store.find_duplicate(fingerprint, LEGACY_CONFIG.metadata("BAAI/bge-m3"))["name"] == "old.txt"
    assert store._read_manifest()["old.txt"]["sha256"] == fingerprint
    (store.paths.knowledge_dir / "old.txt").unlink()
    assert store.find_duplicate(fingerprint, LEGACY_CONFIG.metadata("BAAI/bge-m3")) is None


def test_changed_embedding_model_invalidates_character_dedup(tmp_path):
    from dataclasses import replace
    runtime = ProtocolRuntime(tmp_path)
    runtime.upload("models", [("same.txt", io.BytesIO(b"same model test"))], LEGACY_CONFIG)
    runtime.config = replace(runtime.config, embedding_model="other-model")
    result = runtime.upload("models", [("same.txt", io.BytesIO(b"same model test"))], LEGACY_CONFIG)
    assert len(result["imported"]) == 1 and not result["skipped"]
    assert result["imported"][0]["chunking"]["embedding_model"] == "other-model"
    assert result["imported"][0]["chunking"]["tokenizer"] == "BAAI/bge-m3"


def test_preflight_api_auth_limits_and_server_hash(tmp_path):
    runtime = ProtocolRuntime(tmp_path)
    client = TestClient(create_service_app(runtime, settings=RagHttpSettings(data_root=tmp_path, token="secret")))
    headers = {"Authorization": "Bearer secret", "X-RAG-Namespace": "alpha"}
    fingerprint = hashlib.sha256(b"actual bytes").hexdigest()
    payload = {"files": [{"name": "renamed.txt", "sha256": fingerprint}], "strategy": "recursive", "chunk_size": 512, "chunk_overlap": 64}
    assert client.post("/v1/documents/check", json=payload).status_code == 401
    assert len(client.get("/v1/chunking/strategies", headers=headers).json()["strategies"]) == 5
    assert client.post("/v1/documents/check", headers=headers, json=payload).json()["files"][0]["document"] is None
    response = client.post("/v1/documents", headers=headers, data={"sha256": "0" * 64}, files={"files": ("original.txt", b"actual bytes")})
    assert response.json()["imported"][0]["sha256"] == fingerprint
    checked = client.post("/v1/documents/check", headers=headers, json=payload)
    assert checked.json()["files"][0]["document"]["name"] == "original.txt"
    assert client.post("/v1/documents/check", headers={**headers, "X-RAG-Namespace": "beta"}, json=payload).json()["files"][0]["document"] is None
    assert client.post("/v1/documents/check", headers=headers, json={**payload, "files": payload["files"] * 11}).status_code == 422
    assert client.post("/v1/documents/check", headers=headers, json={**payload, "chunk_overlap": 512}).status_code == 400
    assert client.post("/v1/documents", headers=headers, data={"strategy": "bad"}, files={"files": ("a.txt", b"text")}).status_code == 400
