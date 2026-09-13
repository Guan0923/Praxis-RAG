"""Exercise stored credentials through the HTTP and MCP boundaries."""

from fastapi.testclient import TestClient
import anyio
import httpx2 as httpx
import json
import os

from rag_mcp.http_service import RagHttpSettings, create_service_app


class Runtime:
    def list_documents(self, namespace):
        return []


def test_key_lifecycle_and_persistence(tmp_path):
    settings = RagHttpSettings(data_root=tmp_path)
    admin = {}
    app = create_service_app(Runtime(), settings=settings)
    with TestClient(app, base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        created = client.post("/v1/admin/api-keys", headers=admin,
                              json={"name": "app", "namespaces": ["alpha", "beta"]})
        assert created.status_code == 201
        key = created.json()
        headers = {"Authorization": "Bearer " + key["key"], "X-RAG-Namespace": "alpha"}
        assert client.get("/v1/documents", headers=headers).status_code == 200
        assert client.get("/v1/documents", headers={**headers, "X-RAG-Namespace": "beta"}).status_code == 200
        assert client.get("/v1/documents", headers={**headers, "X-RAG-Namespace": "other"}).status_code == 403
        assert client.get("/v1/admin/api-keys", headers=headers).status_code == 200
        assert key["key"] not in client.get("/v1/admin/api-keys", headers=admin).text
        if os.name == "nt":
            assert key["key"].encode() not in (tmp_path / "api-keys.sqlite3").read_bytes()
        else:
            assert (tmp_path / "api-keys.sqlite3").stat().st_mode & 0o077 == 0
        assert client.post("/v1/admin/api-keys", headers=admin,
                           json={"name": "empty", "namespaces": []}).status_code == 422
    with TestClient(create_service_app(Runtime(), settings=settings), base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        assert client.get("/v1/documents", headers=headers).status_code == 200
        path = "/v1/admin/api-keys/" + key["id"]
        body = {"name": "renamed", "namespaces": ["beta"], "enabled": True}
        assert client.put(path, headers=admin, json=body).status_code == 200
        assert client.get("/v1/documents", headers=headers).status_code == 403
        headers["X-RAG-Namespace"] = "beta"
        assert client.get("/v1/documents", headers=headers).status_code == 200
        client.put(path, headers=admin, json={**body, "enabled": False})
        assert client.get("/v1/documents", headers=headers).status_code == 401
        assert client.get("/mcp", headers=headers).status_code == 401
        client.put(path, headers=admin, json=body)
        assert client.delete(path, headers=admin).status_code == 204
        assert client.get("/v1/documents", headers=headers).status_code == 401


def test_local_management_rejects_remote_and_forwarded_requests(tmp_path):
    app = create_service_app(Runtime(), settings=RagHttpSettings(data_root=tmp_path))
    with TestClient(app, base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        response = client.post("/v1/admin/api-keys", json={"name": "local", "namespaces": ["alpha"]})
        assert response.status_code == 201
        headers = {"Authorization": "Bearer " + response.json()["key"]}
        assert client.get("/v1/admin/api-keys", headers=headers).status_code == 200
        assert client.get("/v1/admin/api-keys", headers={"Origin": "http://evil.test"}).status_code == 403
        assert client.get("/v1/admin/api-keys", headers={"X-Forwarded-For": "127.0.0.1"}).status_code == 403
    with TestClient(app, base_url="http://127.0.0.1", client=("192.0.2.1", 1234)) as client:
        assert client.get("/v1/admin/api-keys").status_code == 403
        assert client.get("/v1/documents", headers={"X-RAG-Namespace": "alpha"}).status_code == 401


def test_scoped_key_over_real_mcp(tmp_path):
    from test_mcp_http import ADMIN, mcp_client, running_service

    with running_service(tmp_path) as (url, _app, _runtime):
        async def scenario():
            async with httpx.AsyncClient() as http:
                response = await http.post(url + "/v1/admin/api-keys", headers=ADMIN,
                                           json={"name": "mcp-client", "namespaces": ["alpha", "beta"]})
                key = response.json()
                headers = {"Authorization": "Bearer " + key["key"], "X-RAG-Namespace": "alpha"}
                async with mcp_client(url, headers=headers) as client:
                    result = await client.call_tool("search", {"query": "hello"})
                    assert json.loads(result.content[0].text)["namespace"] == "alpha"
                async with mcp_client(url, headers={**headers, "X-RAG-Namespace": "beta"}) as client:
                    result = await client.call_tool("search", {"query": "hello"})
                    assert json.loads(result.content[0].text)["namespace"] == "beta"
                denied = await http.post(url + "/mcp", headers={**headers, "X-RAG-Namespace": "other"}, json={})
                assert denied.status_code == 403
                await http.delete(url + "/v1/admin/api-keys/" + key["id"], headers=ADMIN)
                assert (await http.post(url + "/mcp", headers=headers, json={})).status_code == 401
        anyio.run(scenario)


def test_mcp_config_uses_persisted_key_and_checks_scope(tmp_path):
    settings = RagHttpSettings(data_root=tmp_path)
    admin = {"X-RAG-Namespace": "alpha"}
    with TestClient(create_service_app(Runtime(), settings=settings), base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        key = client.post("/v1/admin/api-keys", headers=admin,
                          json={"name": "MCP", "namespaces": ["alpha"]}).json()
    with TestClient(create_service_app(Runtime(), settings=settings), base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        path = "/v1/admin/api-keys/" + key["id"] + "/mcp-config"
        response = client.get(path, headers=admin)
        assert response.json()["headers"] == {"Authorization": "Bearer " + key["key"], "X-RAG-Namespace": "alpha"}
        assert response.headers["cache-control"] == "no-store"
        assert client.get(path, headers={**admin, "X-RAG-Namespace": "other"}).status_code == 403
        assert client.get(path, headers={**admin, "Authorization": "Bearer " + key["key"]}).status_code == 200
        client.put("/v1/admin/api-keys/" + key["id"], headers=admin,
                   json={"name": "MCP", "namespaces": ["alpha"], "enabled": False})
        assert client.get(path, headers=admin).status_code == 403


def test_mcp_requires_authorization_even_on_localhost(tmp_path):
    app = create_service_app(Runtime(), settings=RagHttpSettings(data_root=tmp_path))
    with TestClient(app, base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        for method in ("GET", "POST", "DELETE"):
            response = client.request(method, "/mcp", headers={"X-RAG-Namespace": "alpha"})
            assert response.status_code == 401
            assert response.headers["www-authenticate"] == "Bearer"


def test_digest_only_keys_are_preserved_but_cannot_be_exported(tmp_path):
    import hashlib
    import sqlite3
    from rag_mcp.api_keys import ApiKeyStore

    secret = "prk_legacy_test_key"
    with sqlite3.connect(tmp_path / "api-keys.sqlite3") as db:
        db.execute("CREATE TABLE api_keys (id TEXT PRIMARY KEY, name TEXT, prefix TEXT, digest TEXT, namespaces TEXT, enabled INTEGER, created_at TEXT)")
        db.execute("INSERT INTO api_keys VALUES (?, ?, ?, ?, ?, ?, ?)",
                   ("old", "Old key", secret[:12], hashlib.sha256(secret.encode()).hexdigest(), '["alpha"]', 1, "2026-09-13"))
    store = ApiKeyStore(tmp_path)
    assert store.namespaces_for_key(secret) == ["alpha"]
    assert not store.list()[0]["exportable"]
    settings = RagHttpSettings(data_root=tmp_path)
    with TestClient(create_service_app(Runtime(), settings=settings), base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        response = client.get("/v1/admin/api-keys/old/mcp-config",
                              headers={"X-RAG-Namespace": "alpha"})
        assert response.status_code == 409
