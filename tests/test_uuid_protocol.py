"""UUID retrieval is tenant-bound, bounded, and never repeats search."""

from __future__ import annotations

import base64
import json
from types import SimpleNamespace
from unittest.mock import Mock
from urllib.parse import quote

import anyio
import pytest
from fastapi.testclient import TestClient
from mcp import types
from mcp.shared.exceptions import MCPError

from rag_mcp.http_service import RagHttpSettings, create_service_app
from rag_mcp.service_knowledge import KnowledgeError
from rag_mcp.mcp_http import NamespaceEndpoint
from rag_mcp.server import create_registry
from rag_mcp.state.result_snapshots import SnapshotUnavailable
from rag_mcp.state.store import StateStores

RESULT_ID = "9f663eb9-bc24-4485-b544-373cfac26732"
ARTIFACT_ID = "d1feec53-1036-4af0-8035-86ef6990fa6a"
URI = f"rag://artifacts/alpha/{ARTIFACT_ID}"
AUTH = {"Authorization": "Bearer test-token", "X-RAG-Namespace": "alpha"}


class Runtime:
    def __init__(self, root):
        self.state = StateStores.create(root)
        self.search = Mock(side_effect=AssertionError("Snapshot reads must not run retrieval"))
        self.submit_search = Mock(return_value={"result_id": RESULT_ID})
        self.get_search_result = Mock(side_effect=self._result)
        self.get_artifact = Mock(side_effect=self._artifact)
        self.download_artifact = Mock(side_effect=self._download)

    def list_documents(self, namespace):
        return [{"name": "manual.md"}]

    def read_document(self, namespace, name):
        return "Existing document text", "version-1"

    def _result(self, namespace, result_id, *, page=1, page_size=10):
        if namespace != "alpha" or result_id != RESULT_ID:
            raise SnapshotUnavailable("Search result expired or unavailable")
        if page > 2:
            raise ValueError("Result page does not exist")
        return {"result_id": result_id, "page": page, "page_size": page_size,
                "results": [{"text": "full result", "metadata": {"source": "manual.md"}}]}

    def _artifact(self, namespace, artifact_id, *, offset=0, length=65536):
        if namespace != "alpha" or artifact_id != ARTIFACT_ID:
            raise SnapshotUnavailable("Artifact expired or unavailable")
        content = b"abcdef"
        data = content[offset:offset + length]
        end = offset + len(data)
        return {"artifact_id": artifact_id, "mime_type": "application/octet-stream", "encoding": "base64",
                "data": base64.b64encode(data).decode(), "total_bytes": len(content), "offset": offset,
                "next_offset": end if end < len(content) else None, "expires_at": 2000000000,
                "name": "artifact.bin"}

    def _download(self, namespace, artifact_id):
        self._artifact(namespace, artifact_id)
        return b"abcdef", "application/octet-stream", '..\\private\\报告"\r\n.bin'


@pytest.fixture
def service(tmp_path, monkeypatch):
    monkeypatch.setenv("RAG_STATE_ENCRYPTION_KEY", "base64:" + base64.b64encode(b"k" * 32).decode())
    runtime = Runtime(tmp_path)
    app = create_service_app(runtime, settings=RagHttpSettings(data_root=tmp_path, token="test-token"))
    try:
        with TestClient(app, base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
            yield runtime, app, client
    finally:
        runtime.state.close()


def test_search_only_registry_retains_legacy_tool_count():
    search = Mock(return_value={"results": [{"text": "full", "metadata": {"key": "value"}}]})
    registry = create_registry(SimpleNamespace(search=search))
    assert [item.name for item in registry.definitions()] == ["search"]
    result = registry.invoke("search", {"query": "hello"})
    assert not result.is_error
    assert json.loads(result.content[0].text)["results"] == [{"text": "full", "metadata": {"key": "value"}}]


@pytest.mark.parametrize("wait", [0, 5, 30])
def test_local_search_rejects_queue_wait_argument(wait):
    search = Mock()
    registry = create_registry(SimpleNamespace(search=search))
    assert "wait_seconds" not in registry.definitions()[0].input_schema["properties"]
    assert registry.invoke("search", {"query": "hello", "wait_seconds": wait}).is_error
    search.assert_not_called()


@pytest.mark.parametrize("wait", [-1, 30.1, True, False, "5", None, float("nan"), float("inf"), -float("inf")])
def test_service_search_rejects_invalid_wait_before_submission(service, wait):
    runtime, app, _ = service
    result = anyio.run(app.state.mcp_endpoint.registry("alpha").invoke_async, "search",
                       {"query": "hello", "wait_seconds": wait})
    assert result.is_error and "wait_seconds" in result.content[0].text
    runtime.submit_search.assert_not_called()
    runtime.get_search_result.assert_not_called()
    runtime.search.assert_not_called()


@pytest.mark.parametrize("wait", [0, 0.0])
def test_service_search_zero_wait_only_returns_id_without_polling(service, wait):
    runtime, app, _ = service
    registry = app.state.mcp_endpoint.registry("alpha")
    schema = registry.definitions()[0].input_schema["properties"]["wait_seconds"]
    assert (schema["minimum"], schema["maximum"], schema["default"]) == (0, 30, 10)
    result = anyio.run(registry.invoke_async, "search", {"query": "hello", "wait_seconds": wait})
    assert not result.is_error
    assert json.loads(result.content[0].text) == {"result_id": RESULT_ID}
    namespace, arguments = runtime.submit_search.call_args.args
    assert namespace == "alpha" and arguments["query"] == "hello"
    assert "wait_seconds" not in arguments
    runtime.submit_search.assert_called_once()
    runtime.get_search_result.assert_not_called()
    runtime.search.assert_not_called()


@pytest.mark.parametrize("arguments", [{"query": "hello"}, {"query": "hello", "wait_seconds": 30}])
def test_service_search_waits_for_snapshot_without_repeated_submission(service, arguments):
    runtime, app, _ = service
    snapshot = runtime._result("alpha", RESULT_ID)
    runtime.get_search_result.side_effect = [
        {"result_id": RESULT_ID, "status": "queued", "stage": stage}
        for stage in ("queued", "processing", "retry_wait")
    ] + [snapshot]
    result = anyio.run(app.state.mcp_endpoint.registry("alpha").invoke_async, "search", arguments)
    assert not result.is_error and json.loads(result.content[0].text) == snapshot
    runtime.submit_search.assert_called_once()
    assert runtime.get_search_result.call_count == 4
    assert all(call.args == ("alpha", RESULT_ID) for call in runtime.get_search_result.call_args_list)
    runtime.search.assert_not_called()


def test_service_search_wait_expiry_only_returns_id_then_poll_completes(service):
    runtime, app, _ = service
    snapshot = runtime._result("alpha", RESULT_ID)
    runtime.get_search_result.side_effect = None
    runtime.get_search_result.return_value = {"result_id": RESULT_ID, "status": "queued", "stage": "processing"}
    registry = app.state.mcp_endpoint.registry("alpha")
    result = anyio.run(registry.invoke_async, "search", {"query": "hello", "wait_seconds": 0.02})
    assert not result.is_error and json.loads(result.content[0].text) == {"result_id": RESULT_ID}
    assert runtime.get_search_result.called
    runtime.get_search_result.return_value = snapshot
    polled = anyio.run(registry.invoke_async, "get_search_result", {"result_id": RESULT_ID})
    assert not polled.is_error and json.loads(polled.content[0].text) == snapshot
    runtime.submit_search.assert_called_once()
    runtime.search.assert_not_called()


@pytest.mark.parametrize("stage", ["queued", "processing", "retry_wait"])
def test_service_result_poll_exposes_pending_stage_without_inference(service, stage):
    runtime, app, _ = service
    pending = {"result_id": RESULT_ID, "status": "queued", "stage": stage}
    runtime.get_search_result.side_effect = None
    runtime.get_search_result.return_value = pending
    result = anyio.run(app.state.mcp_endpoint.registry("alpha").invoke_async,
                       "get_search_result", {"result_id": RESULT_ID})
    assert not result.is_error and json.loads(result.content[0].text) == pending
    runtime.submit_search.assert_not_called()
    runtime.search.assert_not_called()


@pytest.mark.parametrize("error", ["Search failed", "Search task deadline exceeded"])
@pytest.mark.parametrize("tool", ["search", "get_search_result"])
def test_service_search_failure_and_deadline_are_tool_errors(service, error, tool):
    runtime, app, _ = service
    runtime.get_search_result.side_effect = SnapshotUnavailable(error)
    arguments = {"query": "hello"} if tool == "search" else {"result_id": RESULT_ID}
    result = anyio.run(app.state.mcp_endpoint.registry("alpha").invoke_async, tool, arguments)
    assert result.is_error and error in result.content[0].text
    assert runtime.submit_search.call_count == (tool == "search")
    runtime.search.assert_not_called()


def test_service_submission_failure_never_falls_back_to_local_search(service):
    runtime, app, _ = service
    runtime.submit_search.side_effect = KnowledgeError("Queue acceptance unavailable")
    result = anyio.run(app.state.mcp_endpoint.registry("alpha").invoke_async,
                       "search", {"query": "hello"})
    assert result.is_error
    runtime.submit_search.assert_called_once()
    runtime.get_search_result.assert_not_called()
    runtime.search.assert_not_called()


def test_registry_binds_snapshot_reader_without_search():
    read = Mock(return_value={"results": [{"text": "full", "metadata": {"key": "value"}}]})
    executor = SimpleNamespace(search=Mock(), get_search_result=read)
    registry = create_registry(executor)
    assert [item.name for item in registry.definitions()] == ["search", "get_search_result"]
    result = registry.invoke("get_search_result", {"result_id": RESULT_ID, "page": 2, "page_size": 3})
    assert not result.is_error
    read.assert_called_once_with(result_id=RESULT_ID, page=2, page_size=3)
    assert json.loads(result.content[0].text)["results"][0]["metadata"] == {"key": "value"}
    executor.search.assert_not_called()


@pytest.mark.parametrize("arguments", [
    {}, {"result_id": "../private"}, {"result_id": RESULT_ID.replace("-", "")},
    {"result_id": RESULT_ID.upper()}, {"result_id": RESULT_ID, "page": 2**31},
    {"result_id": "{" + RESULT_ID + "}"}, {"result_id": RESULT_ID, "page": True},
    {"result_id": RESULT_ID, "page": 0}, {"result_id": RESULT_ID, "page": "1"},
    {"result_id": RESULT_ID, "page_size": 0}, {"result_id": RESULT_ID, "page_size": 101},
    {"result_id": RESULT_ID, "page_size": 1.5}, {"result_id": RESULT_ID, "path": "private"},
])
def test_registry_validates_before_reader(arguments):
    executor = SimpleNamespace(search=Mock(), get_search_result=Mock())
    result = create_registry(executor).invoke("get_search_result", arguments)
    assert result.is_error
    executor.search.assert_not_called()
    executor.get_search_result.assert_not_called()


def test_snapshot_missing_is_mcp_tool_error():
    executor = SimpleNamespace(search=Mock(), get_search_result=Mock(side_effect=SnapshotUnavailable("expired")))
    result = anyio.run(create_registry(executor).invoke_async, "get_search_result", {"result_id": RESULT_ID})
    assert result.is_error and "expired" in result.content[0].text
    executor.search.assert_not_called()


def test_rest_snapshot_and_chunks_are_tenant_scoped(service):
    runtime, _app, client = service
    result = client.get(f"/v1/search-results/{RESULT_ID}?page=2&page_size=3", headers=AUTH)
    assert result.status_code == 200
    assert result.headers["cache-control"] == "no-store"
    assert result.json()["results"][0]["text"] == "full result"
    runtime.get_search_result.assert_called_once_with("alpha", RESULT_ID, page=2, page_size=3)
    chunk = client.get(f"/v1/artifacts/{ARTIFACT_ID}/content?offset=1&length=2", headers=AUTH)
    assert chunk.status_code == 200 and base64.b64decode(chunk.json()["data"]) == b"bc"
    assert chunk.headers["cache-control"] == "no-store"
    runtime.get_artifact.assert_called_once_with("alpha", ARTIFACT_ID, offset=1, length=2)
    runtime.search.assert_not_called()


@pytest.mark.parametrize("path", [f"/v1/search-results/{RESULT_ID}", f"/v1/artifacts/{ARTIFACT_ID}",
                                  f"/v1/artifacts/{ARTIFACT_ID}/content"])
def test_rest_auth_scope_and_expired_results(service, path):
    runtime, _app, client = service
    assert client.get(path).status_code == 401
    assert client.get(path, headers={"Authorization": "Bearer test-token"}).status_code == 400
    assert client.get(path, headers={**AUTH, "X-RAG-Namespace": "../alpha"}).status_code == 400
    key = client.post("/v1/admin/api-keys", json={"name": "scoped", "namespaces": ["alpha"]}).json()["key"]
    scoped = {**AUTH, "Authorization": "Bearer " + key}
    assert client.get(path, headers={**scoped, "X-RAG-Namespace": "beta"}).status_code == 403
    missing = client.get(path, headers={**AUTH, "X-RAG-Namespace": "beta"})
    assert missing.status_code == 410 and missing.headers["cache-control"] == "no-store"
    assert client.get(path, headers=scoped).status_code == 200
    runtime.search.assert_not_called()


@pytest.mark.parametrize("path", [
    "/v1/search-results/not-a-uuid", f"/v1/search-results/{RESULT_ID}?page=0",
    f"/v1/search-results/{RESULT_ID.upper()}", f"/v1/search-results/{RESULT_ID}?page={2**31}",
    f"/v1/search-results/{RESULT_ID}?page=true", f"/v1/search-results/{RESULT_ID}?page_size=101",
    f"/v1/artifacts/{ARTIFACT_ID.replace('-', '')}", f"/v1/artifacts/{ARTIFACT_ID}/content?offset=-1",
    f"/v1/artifacts/{ARTIFACT_ID}/content?length=0", f"/v1/artifacts/{ARTIFACT_ID}/content?length=1048577",
])
def test_rest_rejects_invalid_uuid_and_limits_before_runtime(service, path):
    runtime, _app, client = service
    response = client.get(path, headers=AUTH)
    assert response.status_code == 400
    assert response.headers["cache-control"] == "no-store"
    runtime.get_search_result.assert_not_called()
    runtime.get_artifact.assert_not_called()
    runtime.download_artifact.assert_not_called()
    runtime.search.assert_not_called()


def test_rest_invalid_page_and_download_headers(service):
    runtime, _app, client = service
    assert client.get(f"/v1/search-results/{RESULT_ID}?page=3", headers=AUTH).status_code == 400
    response = client.get(f"/v1/artifacts/{ARTIFACT_ID}", headers=AUTH)
    assert response.status_code == 200 and response.content == b"abcdef"
    assert response.headers["cache-control"] == "no-store"
    assert response.headers["x-content-type-options"] == "nosniff"
    disposition = response.headers["content-disposition"]
    assert disposition.startswith("attachment;") and "private" not in disposition
    assert "\r" not in disposition and "\n" not in disposition
    assert "filename*=UTF-8''" + quote('报告".bin', safe="") in disposition
    runtime.download_artifact.assert_called_once_with("alpha", ARTIFACT_ID)
    runtime.search.assert_not_called()


def test_mcp_artifact_chunks_templates_and_document_compatibility(service):
    runtime, app, _client = service
    peer = NamespaceEndpoint("alpha", runtime, app.state.mcp_endpoint.catalogs.for_namespace("alpha"),
                             app.state.mcp_endpoint.security, None)

    async def scenario():
        templates = await peer.list_templates(None, None)
        assert "rag://artifacts/{namespace}/{artifact_id}{?offset,length}" in {
            item.uri_template for item in templates.resource_templates}
        resources = await peer.list_resources(None, None)
        assert len(resources.resources) == 1 and "artifacts" not in str(resources.resources[0].uri)
        result = await peer.read_resource(None, SimpleNamespace(uri=URI + "?length=2"))
        assert isinstance(result.contents[0], types.TextResourceContents)
        payload = json.loads(result.contents[0].text)
        assert payload["encoding"] == "base64" and base64.b64decode(payload["data"]) == b"ab"
        assert payload["next_uri"] == URI + "?offset=2&length=2"
        final = await peer.read_resource(None, SimpleNamespace(uri=URI + "?offset=4&length=1048576"))
        assert json.loads(final.contents[0].text)["next_uri"] is None
        document = await peer.read_resource(None, SimpleNamespace(uri="rag://knowledge/alpha/manual.md"))
        assert json.loads(document.contents[0].text)["text"] == "Existing document text"
    anyio.run(scenario)
    runtime.search.assert_not_called()


@pytest.mark.parametrize("uri", [
    URI.replace("alpha", "beta"), URI.replace("alpha", "%61lpha"), URI.replace("alpha", "alpha%2F.."),
    URI.replace(ARTIFACT_ID, "..%2Fprivate"), URI + "/extra", URI + "#fragment",
    URI + "?offset=-1", URI + "?length=1048577", URI + "?length=0", URI + "?length=1&length=2",
    URI + "?offset=%2B1", URI + "?path=private", URI + "?offset=", URI.replace("rag:", "https:"),
    URI + "\n", URI + "#", URI + "?", URI + "?offset=00", URI + "?length=01",
    URI + "?%6fffset=1", URI + "?offset=%31", URI.replace(ARTIFACT_ID, ARTIFACT_ID.upper()),
])
def test_mcp_rejects_invalid_artifact_uris_before_runtime(service, uri):
    runtime, app, _client = service
    peer = NamespaceEndpoint("alpha", runtime, app.state.mcp_endpoint.catalogs.for_namespace("alpha"),
                             app.state.mcp_endpoint.security, None)

    async def scenario():
        with pytest.raises(ValueError):
            await peer.read_resource(None, SimpleNamespace(uri=uri))
    anyio.run(scenario)
    runtime.get_artifact.assert_not_called()
    runtime.search.assert_not_called()


def test_namespace_and_admin_registry_forward_without_search(service):
    runtime, app, _client = service
    registry = app.state.mcp_endpoint.registry("alpha")
    result = registry.invoke("get_search_result", {"result_id": RESULT_ID})
    assert not result.is_error
    runtime.get_search_result.assert_called_once_with("alpha", RESULT_ID, page=1, page_size=10)
    peer = NamespaceEndpoint("alpha", runtime, app.state.mcp_endpoint.catalogs.for_namespace("alpha"),
                             app.state.mcp_endpoint.security, None)
    assert peer.get_search_result(RESULT_ID, page=2, page_size=1)["page"] == 2
    runtime.get_search_result.assert_called_with("alpha", RESULT_ID, page=2, page_size=1)
    runtime.search.assert_not_called()


@pytest.mark.parametrize("mode", ["legacy", "auto", "2026-07-28"])
def test_uuid_reads_over_real_mcp(tmp_path, mode):
    from test_mcp_http import mcp_client, running_service

    runtime = Runtime(tmp_path)
    try:
        with running_service(tmp_path, runtime=runtime) as (url, _app, _runtime):
            async def scenario():
                async with mcp_client(url, mode, headers=AUTH) as client:
                    result = await client.call_tool("get_search_result", {"result_id": RESULT_ID})
                    assert not result.is_error and json.loads(result.content[0].text)["result_id"] == RESULT_ID
                    assert (await client.call_tool("get_search_result", {"result_id": ARTIFACT_ID})).is_error
                    resource = await client.read_resource(URI + "?length=2")
                    assert json.loads(resource.contents[0].text)["next_uri"] == URI + "?offset=2&length=2"
                    with pytest.raises(MCPError):
                        await client.read_resource(URI.replace("alpha", "beta"))
                    with pytest.raises(MCPError):
                        await client.read_resource(URI.replace(ARTIFACT_ID, RESULT_ID))
                    import httpx
                    async with httpx.AsyncClient() as http:
                        disabled = await http.patch(url + "/v1/admin/tools/get_search_result", headers=AUTH,
                                                    json={"enabled": False})
                    assert disabled.status_code == 200
                    assert (await client.call_tool("get_search_result", {"result_id": RESULT_ID})).is_error
            anyio.run(scenario)
        runtime.search.assert_not_called()
    finally:
        runtime.state.close()


@pytest.mark.parametrize("mime_type", ["text/html", "application/javascript", "image/svg+xml", "text/plain\r\nInjected: yes", "报告"])
def test_artifact_download_always_forces_safe_attachment(service, mime_type):
    runtime, _app, client = service
    runtime.download_artifact.side_effect = None
    runtime.download_artifact.return_value = (b"untrusted", mime_type, "file.html")
    response = client.get(f"/v1/artifacts/{ARTIFACT_ID}", headers=AUTH)
    assert response.status_code == 200
    assert response.headers["content-disposition"].startswith("attachment;")
    assert response.headers["x-content-type-options"] == "nosniff"
    assert response.headers["cache-control"] == "no-store"
    assert "Injected" not in response.headers["content-type"]


def test_disabled_snapshot_tool_blocks_existing_registry(service):
    runtime, app, client = service
    registry = app.state.mcp_endpoint.registry("alpha")
    response = client.patch("/v1/admin/tools/get_search_result", headers=AUTH, json={"enabled": False})
    assert response.status_code == 200
    assert "get_search_result" not in {item.name for item in registry.definitions()}
    assert registry.invoke("get_search_result", {"result_id": RESULT_ID}).is_error
    assert anyio.run(registry.invoke_async, "get_search_result", {"result_id": RESULT_ID}).is_error
    runtime.get_search_result.assert_not_called()


@pytest.mark.parametrize("path", [f"/v1/search-results/{RESULT_ID}", f"/v1/artifacts/{ARTIFACT_ID}",
                                  f"/v1/artifacts/{ARTIFACT_ID}/content"])
def test_revoked_api_key_cannot_read_existing_uuid(service, path):
    runtime, _app, client = service
    key = client.post("/v1/admin/api-keys", json={"name": "scoped", "namespaces": ["alpha"]}).json()
    headers = {**AUTH, "Authorization": "Bearer " + key["key"]}
    assert client.get(path, headers=headers).status_code == 200
    assert client.delete("/v1/admin/api-keys/" + key["id"]).status_code == 204
    response = client.get(path, headers=headers)
    assert response.status_code == 401
    assert response.headers["cache-control"] == "no-store"
    runtime.search.assert_not_called()


def test_mcp_auth_errors_are_not_cacheable(service):
    _runtime, _app, client = service
    response = client.post("/mcp", json={})
    assert response.status_code == 401
    assert response.headers["cache-control"] == "no-store"


def test_mcp_missing_resource_maps_to_protocol_error(service):
    runtime, app, _client = service
    peer = NamespaceEndpoint("beta", runtime, app.state.mcp_endpoint.catalogs.for_namespace("beta"),
                             app.state.mcp_endpoint.security, None)

    async def scenario():
        async def read(_ctx):
            return await peer.read_resource(None, SimpleNamespace(uri=URI.replace("alpha", "beta")))
        with pytest.raises(MCPError, match="unavailable"):
            await peer.observe(SimpleNamespace(), read)
    anyio.run(scenario)
    runtime.search.assert_not_called()
