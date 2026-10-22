"""Upload retry-key plumbing without network servers or indexing backends."""

import json
import threading
from contextlib import asynccontextmanager
from pathlib import Path

import anyio
import pytest
from fastapi.testclient import TestClient

from rag_mcp.document_tools import DocumentRequest, DocumentWriteRequest
from rag_mcp.http_service import RagHttpSettings, create_service_app
from rag_mcp.service_knowledge import KnowledgeValidationError
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from rag_mcp.state.uploads import UploadConflict

AUTH = {"Authorization": "Bearer entrypoint-test-token", "X-RAG-Namespace": "manuals"}


class LegacyRuntime:
    def __init__(self, state):
        self.state = state
        self.calls = []

    def list_documents(self, namespace):
        return []

    def upload(self, namespace, items):
        self.calls.append((namespace, [(name, stream.read()) for name, stream in items]))
        return {"imported": [{"name": "a.txt"}], "skipped": [], "failed": [], "documents": []}

    def write_document(self, namespace, name, content, *, overwrite):
        self.calls.append((namespace, name, content, overwrite))
        return {"status": "updated" if overwrite else "added", "document": {"name": name}, "documents": []}


class KeyedRuntime(LegacyRuntime):
    def upload(self, namespace, items, *, idempotency_key=None):
        result = super().upload(namespace, items)
        self.calls.append(idempotency_key)
        result["imported"][0]["operation_id"] = "runtime-operation"
        result["failed"] = [{"name": "failed.txt", "error": "index unavailable", "operation_id": "failed-operation"}]
        return result

    def write_document(self, namespace, name, content, *, overwrite, idempotency_key=None):
        result = super().write_document(namespace, name, content, overwrite=overwrite)
        self.calls.append(idempotency_key)
        result["operation_id"] = "runtime-operation"
        return result


@pytest.fixture
def make_service():
    states = []

    def make(runtime_type=KeyedRuntime):
        state = StateStores(StateSettings(url="sqlite+pysqlite:///:memory:"))
        states.append(state)
        runtime = runtime_type(state)
        app = create_service_app(runtime, settings=RagHttpSettings(
            data_root=Path("unused"), token="entrypoint-test-token",
        ))
        return runtime, TestClient(app), app.state.mcp_endpoint

    yield make
    for state in states:
        state.close()


@pytest.mark.parametrize("key", ["x", "x" * 128])
def test_http_forwards_valid_key_and_preserves_operation_ids(make_service, key):
    runtime, client, _endpoint = make_service()
    response = client.post("/v1/documents", headers={**AUTH, "Idempotency-Key": key},
                           files={"files": ("a.txt", b"hello")})
    assert response.status_code == 200
    assert runtime.calls == [("manuals", [("a.txt", b"hello")]), key]
    assert response.json()["imported"][0]["operation_id"] == "runtime-operation"
    assert response.json()["failed"][0]["operation_id"] == "failed-operation"


@pytest.mark.parametrize("key", ["", "x" * 129])
def test_http_rejects_invalid_key_before_upload(make_service, key):
    runtime, client, _endpoint = make_service()
    response = client.post("/v1/documents", headers={**AUTH, "Idempotency-Key": key},
                           files={"files": ("a.txt", b"hello")})
    assert response.status_code == 422
    assert response.json()["detail"][0]["loc"] == ["header", "Idempotency-Key"]
    assert runtime.calls == []


def test_http_omits_absent_key_for_legacy_runtime(make_service):
    runtime, client, _endpoint = make_service(LegacyRuntime)
    response = client.post("/v1/documents", headers=AUTH, files={"files": ("a.txt", b"hello")})
    assert response.status_code == 200
    assert runtime.calls == [("manuals", [("a.txt", b"hello")])]
    assert "operation_id" not in response.json()["imported"][0]


def test_http_key_is_not_a_credential_or_namespace(make_service):
    runtime, client, _endpoint = make_service()
    for headers, status in [
        ({"X-RAG-Namespace": "manuals", "Idempotency-Key": "retry"}, 401),
        ({"Authorization": AUTH["Authorization"], "Idempotency-Key": "retry"}, 400),
    ]:
        assert client.post("/v1/documents", headers=headers,
                           files={"files": ("a.txt", b"hello")}).status_code == status
    assert runtime.calls == []


def test_http_advertises_optional_bounded_header(make_service):
    _runtime, client, _endpoint = make_service()
    parameters = client.get("/openapi.json").json()["paths"]["/v1/documents"]["post"]["parameters"]
    header = next(item for item in parameters if item["name"] == "Idempotency-Key")
    assert header["in"] == "header" and not header["required"]
    schema = next(item for item in header["schema"]["anyOf"] if item["type"] == "string")
    assert (schema["minLength"], schema["maxLength"]) == (1, 128)


@pytest.mark.parametrize("operation,overwrite", [("add_document", False), ("update_document", True)])
@pytest.mark.parametrize("key", [None, "x", "x" * 128])
def test_mcp_forwards_optional_key_and_preserves_operation_id(make_service, operation, overwrite, key):
    runtime, _client, endpoint = make_service()
    arguments = {"name": "a.txt", "content": "hello"}
    if key is not None:
        arguments["idempotency_key"] = key
    result = anyio.run(endpoint.registry("manuals").invoke_async, operation, arguments)
    assert not result.is_error
    assert runtime.calls == [("manuals", "a.txt", b"hello", overwrite), key]
    assert json.loads(result.content[0].text)["operation_id"] == "runtime-operation"


@pytest.mark.parametrize("operation", ["add_document", "update_document"])
def test_mcp_omits_absent_or_null_key_for_legacy_runtime(make_service, operation):
    runtime, _client, endpoint = make_service(LegacyRuntime)
    for arguments in [{"name": "a.txt", "content": "hello"},
                      {"name": "a.txt", "content_base64": "aGVsbG8=", "idempotency_key": None}]:
        result = anyio.run(endpoint.registry("manuals").invoke_async, operation, arguments)
        assert not result.is_error
        assert "operation_id" not in json.loads(result.content[0].text)
    assert len(runtime.calls) == 2


@pytest.mark.parametrize("operation", ["add_document", "update_document"])
@pytest.mark.parametrize("key", ["", "x" * 129, 1, True, [], {}])
def test_mcp_rejects_invalid_key_before_write(make_service, operation, key):
    runtime, _client, endpoint = make_service()
    result = anyio.run(endpoint.registry("manuals").invoke_async, operation,
                       {"name": "a.txt", "content": "hello", "idempotency_key": key})
    assert result.is_error
    assert runtime.calls == []


def test_mcp_schema_and_delete_boundary(make_service):
    _runtime, _client, endpoint = make_service()
    tools = {item.name: item for item in endpoint.registry("manuals").definitions()}
    for operation in ("add_document", "update_document"):
        schema = tools[operation].input_schema
        key = schema["properties"]["idempotency_key"]
        assert (key["minLength"], key["maxLength"], key["default"]) == (1, 128, None)
        assert "idempotency_key" not in schema["required"]
    assert "idempotency_key" not in tools["delete_document"].input_schema["properties"]
    with pytest.raises(KnowledgeValidationError, match="Unknown document arguments"):
        DocumentRequest.from_arguments({"name": "a.txt", "idempotency_key": "retry"}, writing=False)
    request = DocumentRequest.from_arguments({"name": "a.txt", "content": "hello"}, writing=True)
    assert isinstance(request, DocumentWriteRequest) and request.idempotency_key is None


class ConflictingRuntime(LegacyRuntime):
    def upload(self, namespace, items, *, idempotency_key=None):
        raise UploadConflict("private-runtime-detail")

    def write_document(self, namespace, name, content, *, overwrite, idempotency_key=None):
        raise UploadConflict("private-runtime-detail")


def test_http_conflict_is_sanitized_409(make_service):
    _runtime, client, _endpoint = make_service(ConflictingRuntime)
    response = client.post("/v1/documents", headers={**AUTH, "Idempotency-Key": "retry"},
                           files={"files": ("a.txt", b"hello")})
    assert response.status_code == 409
    assert response.json() == {"detail": "Upload conflicts with another operation or idempotency key."}


@pytest.mark.parametrize("operation", ["add_document", "update_document"])
def test_mcp_conflict_is_bounded_client_error(make_service, operation):
    _runtime, _client, endpoint = make_service(ConflictingRuntime)
    result = anyio.run(endpoint.registry("manuals").invoke_async, operation,
                       {"name": "a.txt", "content": "hello", "idempotency_key": "retry"})
    assert result.is_error
    assert result.content[0].text == "Upload conflicts with another operation or idempotency key."


@pytest.mark.parametrize("fail_start", [False, True])
def test_lifespan_starts_recovery_before_mcp_and_always_stops(make_service, fail_start):
    runtime, client, endpoint = make_service()
    events = []
    loop_thread = None

    def start():
        assert threading.get_ident() != loop_thread
        events.append("start")
        if fail_start:
            raise RuntimeError("startup failure")

    def stop():
        assert threading.get_ident() != loop_thread
        events.append("stop")

    @asynccontextmanager
    async def mcp_run():
        events.append("mcp_start")
        try:
            yield
        finally:
            events.append("mcp_stop")

    runtime.start_upload_recovery = start
    runtime.stop_upload_recovery = stop
    endpoint.run = mcp_run

    async def scenario():
        nonlocal loop_thread
        loop_thread = threading.get_ident()
        async with client.app.router.lifespan_context(client.app):
            events.append("serving")

    if fail_start:
        with pytest.raises(RuntimeError, match="startup failure"):
            anyio.run(scenario)
        assert events == ["start", "stop"]
    else:
        anyio.run(scenario)
        assert events == ["start", "mcp_start", "serving", "mcp_stop", "stop"]


def test_lifespan_supports_legacy_runtime_without_recovery_methods(make_service):
    _runtime, client, _endpoint = make_service(LegacyRuntime)

    async def scenario():
        async with client.app.router.lifespan_context(client.app):
            pass

    anyio.run(scenario)
