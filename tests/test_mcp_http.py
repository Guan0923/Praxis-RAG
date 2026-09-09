"""Real TCP protocol tests; model inference is replaced by a controllable worker."""

from __future__ import annotations

import json
import io
import socket
import threading
import time
from contextlib import asynccontextmanager, contextmanager
from pathlib import Path
from types import SimpleNamespace

import anyio
import httpx2 as httpx
import pytest
import uvicorn
from mcp.client import Client
from mcp.client.streamable_http import streamable_http_client
from mcp.client.subscriptions import SubscriptionLost
from mcp.server.subscriptions import PromptsListChanged, ResourcesListChanged, ResourceUpdated, ToolsListChanged
from mcp.shared.exceptions import MCPError
from rag_mcp.document_readers import read_document_text
from rag_mcp.config import RagConfig
from rag_mcp.http_service import RagHttpSettings, create_service_app
from rag_mcp.resources import document_uri
from rag_mcp.service_knowledge import IndexedDocument, KnowledgeDocumentStore, KnowledgePaths, RagServiceRuntime

AUTH = {"Authorization": "Bearer test-token", "X-RAG-Namespace": "manuals"}
ADMIN = {**AUTH, "Authorization": "Bearer admin-token"}


class TestIngester:
    __test__ = False

    def ingest(self, path, *, source_root, chunking=None):
        read_document_text(path)
        if path.name == "fail.txt":
            raise ValueError("controlled indexing failure")
        return IndexedDocument(path.name, 1, "test")

    def delete(self, document_id):
        pass


class ProtocolRuntime(RagServiceRuntime):
    """Real document storage/reading; no model or Qdrant required for wire tests."""

    def __init__(self, root):
        self.config = RagConfig()
        self.data_root = root
        self._tenants = {}
        self._tenant_lock = threading.RLock()
        self._inference_lock = threading.RLock()
        self.started = threading.Event()
        self.release = threading.Event()
        self.finished = threading.Event()
        self.wire_responses = []

    def _tenant(self, namespace):
        with self._tenant_lock:
            if namespace not in self._tenants:
                self._tenants[namespace] = SimpleNamespace(
                    documents=KnowledgeDocumentStore(KnowledgePaths(self.data_root / namespace)),
                    ingester=TestIngester(),
                    lock=threading.RLock(),
                )
            return self._tenants[namespace]

    def status(self):
        return {"status": "ok"}

    def search(self, namespace, arguments):
        if arguments["query"] == "slow":
            self.started.set()
            self.release.wait(10)
            self.finished.set()
        return {"namespace": namespace, "query": arguments["query"], "results": []}


@contextmanager
def running_service(root: Path, *, admin_token="admin-token", runtime=None):
    runtime = runtime or ProtocolRuntime(root)
    settings = RagHttpSettings(
        data_root=root,
        token="test-token",
        admin_token=admin_token,
        mcp_allowed_hosts=("127.0.0.1:*",),
        mcp_allowed_origins=("http://127.0.0.1:*",),
    )
    app = create_service_app(runtime, settings=settings)

    class WireRecorder:
        def __init__(self, app):
            self.app = app

        async def __call__(self, scope, receive, send):
            chunks = []
            if scope["type"] == "http" and scope["path"] == "/mcp":
                runtime.wire_responses.append(chunks)

            async def record(message):
                if message["type"] == "http.response.body":
                    chunks.append(message.get("body", b""))
                await send(message)

            await self.app(scope, receive, record)

    app.add_middleware(WireRecorder)
    sock = socket.socket()
    sock.bind(("127.0.0.1", 0))
    sock.listen()
    server = uvicorn.Server(uvicorn.Config(app, log_level="error", lifespan="on", timeout_graceful_shutdown=1))
    app.state.test_server = server
    thread = threading.Thread(target=server.run, kwargs={"sockets": [sock]}, daemon=True)
    thread.start()
    deadline = time.monotonic() + 5
    while not server.started and thread.is_alive() and time.monotonic() < deadline:
        time.sleep(0.01)
    if not server.started:
        raise RuntimeError("Test service failed to start")
    try:
        yield f"http://127.0.0.1:{sock.getsockname()[1]}", app, runtime
    finally:
        runtime.release.set()
        server.should_exit = True
        thread.join(6)
        sock.close()
        if thread.is_alive():
            raise RuntimeError("Test service failed to stop")


@asynccontextmanager
async def mcp_client(url, mode="auto", headers=None, hooks=None, message_handler=None, timeout=5):
    async with httpx.AsyncClient(headers=headers or AUTH, timeout=timeout, event_hooks=hooks or {}) as http:
        async with Client(
            streamable_http_client(url + "/mcp", http_client=http),
            mode=mode,
            read_timeout_seconds=timeout,
            message_handler=message_handler,
        ) as client:
            yield client


@pytest.mark.parametrize("mode", ["legacy", "auto", "2026-07-28"])
def test_both_protocol_lifecycles(tmp_path, mode):
    with running_service(tmp_path) as (url, app, runtime):

        async def scenario():
            requests = []
            session_ids = []

            async def record_request(request):
                if request.method == "POST":
                    requests.append(json.loads(request.content)["method"])

            async def record_response(response):
                if response.headers.get("mcp-session-id"):
                    session_ids.append(response.headers["mcp-session-id"])

            async with mcp_client(
                url, mode, hooks={"request": [record_request], "response": [record_response]}
            ) as client:
                assert client.protocol_version == ("2025-11-25" if mode == "legacy" else "2026-07-28")
                if mode != "2026-07-28":
                    assert client.server_info.version == "0.4.0"
                    assert client.server_capabilities.tools.list_changed
                assert [tool.name for tool in (await client.list_tools()).tools] == [
                    "search", "add_document", "update_document", "delete_document"
                ]
                for query in ("first", "second"):
                    result = await client.call_tool("search", {"query": query})
                    assert json.loads(result.content[0].text)["query"] == query
                assert (await client.call_tool("search", {"query": ""})).is_error
                assert (await client.call_tool("missing", {})).is_error
                assert (await client.list_prompts()).prompts == []
                assert (await client.list_resources()).resources == []
            if mode == "legacy":
                assert requests[:2] == ["initialize", "notifications/initialized"]
                async with httpx.AsyncClient(headers=AUTH) as http:
                    closed = await http.post(
                        url + "/mcp",
                        headers={
                            "Mcp-Session-Id": session_ids[0],
                            "MCP-Protocol-Version": "2025-11-25",
                            "Accept": "application/json, text/event-stream",
                        },
                        json={"jsonrpc": "2.0", "id": 99, "method": "tools/list"},
                    )
                    assert closed.status_code == 404
                async with mcp_client(url, mode, hooks={"response": [record_response]}) as client:
                    await client.list_tools()
                assert session_ids[-1] != session_ids[0]
            else:
                assert "initialize" not in requests
                assert ("server/discover" in requests) == (mode == "auto")

        anyio.run(scenario)


def test_modern_subscriptions_and_real_document_pages(tmp_path):
    with running_service(tmp_path) as (url, app, runtime):

        async def scenario():
            async with httpx.AsyncClient(headers=AUTH) as http, mcp_client(url) as client:
                async with client.listen(
                    tools_list_changed=True, prompts_list_changed=True, resources_list_changed=True
                ) as sub:
                    changed = await http.patch(url + "/v1/admin/tools/search", headers=ADMIN, json={"enabled": False})
                    assert changed.json()["changed"]
                    with anyio.fail_after(2):
                        assert isinstance(await anext(sub), ToolsListChanged)
                    assert "search" not in {tool.name for tool in (await client.list_tools(cache_mode="refresh")).tools}
                    assert (await client.call_tool("search", {"query": "x"})).is_error
                    await http.patch(url + "/v1/admin/tools/search", headers=ADMIN, json={"enabled": True})
                    with anyio.fail_after(2):
                        assert isinstance(await anext(sub), ToolsListChanged)
                    template = {
                        "description": "Summarize",
                        "arguments": [{"name": "topic", "required": True}],
                        "messages": [{"role": "user", "text": "Summarize {{topic}}"}],
                    }
                    assert (
                        await http.put(url + "/v1/admin/prompts/summary", headers=ADMIN, json=template)
                    ).status_code == 200
                    with anyio.fail_after(2):
                        assert isinstance(await anext(sub), PromptsListChanged)
                    prompt = await client.get_prompt("summary", {"topic": "{{do_not_execute}}"})
                    assert prompt.messages[0].content.text == "Summarize {{do_not_execute}}"
                    with pytest.raises(MCPError):
                        await client.get_prompt("summary", {})
                    text = "段落一\n\n" + "长" * 17000 + "\n\n最后一段"
                    uploaded = await http.post(url + "/v1/documents", files={"files": ("手册.txt", text.encode())})
                    assert not uploaded.json()["failed"]
                    with anyio.fail_after(2):
                        assert isinstance(await anext(sub), ResourcesListChanged)
                    uri = (await client.list_resources(cache_mode="refresh")).resources[0].uri
                    next_uri, pieces = uri, []
                    while next_uri:
                        payload = json.loads((await client.read_resource(next_uri)).contents[0].text)
                        assert len(payload["text"]) <= 8000
                        pieces.append(payload["text"])
                        next_uri = payload["next_uri"]
                    assert "".join(pieces) == text
                    first_page = json.loads((await client.read_resource(uri)).contents[0].text)
                    async with client.listen(resource_subscriptions=[uri]) as updates:
                        await http.post(url + "/v1/documents", files={"files": ("手册.txt", b"replacement")})
                        with anyio.fail_after(2):
                            assert isinstance(await anext(sub), ResourcesListChanged)
                            assert (await anext(updates)).uri == uri
                        with pytest.raises(MCPError, match="changed"):
                            await client.read_resource(first_page["next_uri"], cache_mode="refresh")
                        await http.delete(url + "/v1/documents/手册.txt")
                        with anyio.fail_after(2):
                            assert isinstance(await anext(sub), ResourcesListChanged)
                            assert isinstance(await anext(updates), ResourceUpdated)
                        with pytest.raises(MCPError):
                            await client.read_resource(uri, cache_mode="refresh")
                    assert (await http.delete(url + "/v1/admin/prompts/summary", headers=ADMIN)).status_code == 200
                    with anyio.fail_after(2):
                        assert isinstance(await anext(sub), PromptsListChanged)

        anyio.run(scenario)


@pytest.mark.parametrize("mode", ["legacy", "2026-07-28"])
def test_cancellation_keeps_other_calls_usable(tmp_path, mode):
    with running_service(tmp_path) as (url, app, runtime):

        async def scenario():
            async with mcp_client(url, mode) as client:
                successes = []
                scope_ready = anyio.Event()
                finished = anyio.Event()
                scopes = []

                async def slow():
                    with anyio.CancelScope() as scope:
                        scopes.append(scope)
                        scope_ready.set()
                        successes.append(await client.call_tool("search", {"query": "slow"}))
                    finished.set()

                async with anyio.create_task_group() as tasks:
                    tasks.start_soon(slow)
                    await scope_ready.wait()
                    assert await anyio.to_thread.run_sync(runtime.started.wait, 2)
                    scopes[0].cancel()
                    with anyio.fail_after(2):
                        await finished.wait()
                    result = await client.call_tool("search", {"query": "still usable"})
                    assert not result.is_error
                    runtime.release.set()
                    assert await anyio.to_thread.run_sync(runtime.finished.wait, 2)
                    assert not (await client.call_tool("search", {"query": "after worker"})).is_error
                    assert successes == []
                    for chunks in runtime.wire_responses:
                        for line in b"".join(chunks).decode().splitlines():
                            if not line.startswith("data: "):
                                continue
                            result = json.loads(line[6:]).get("result", {})
                            for content in result.get("content", []):
                                if content.get("type") == "text" and content["text"].startswith("{"):
                                    assert json.loads(content["text"]).get("query") != "slow"

        anyio.run(scenario)


def test_http_security_and_admin_credentials(tmp_path):
    with running_service(tmp_path) as (url, app, runtime):

        async def scenario():
            request = {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {
                    "protocolVersion": "2025-11-25",
                    "capabilities": {},
                    "clientInfo": {"name": "test", "version": "1"},
                },
            }
            base = {**AUTH, "Accept": "application/json, text/event-stream"}
            async with httpx.AsyncClient() as http:
                for headers, status in [
                    ({**base, "Authorization": "Bearer wrong"}, 401),
                    ({**base, "X-RAG-Namespace": "../escape"}, 400),
                    ({**base, "Host": "evil.example"}, 421),
                    ({**base, "Origin": "https://evil.example"}, 403),
                    ({**base, "Authorization": "Bearer admin-token"}, 401),
                ]:
                    assert (await http.post(url + "/mcp", headers=headers, json=request)).status_code == status
                assert (await http.get(url + "/v1/admin/tools", headers=AUTH)).status_code == 401
                assert (await http.get(url + "/v1/admin/tools", headers=ADMIN)).status_code == 200
                assert not runtime._tenants  # configuration does not initialize retrieval dependencies
                assert (
                    await http.patch(url + "/v1/admin/tools/nope", headers=ADMIN, json={"enabled": True})
                ).status_code == 404
                assert (
                    await http.patch(url + "/v1/admin/tools/search", headers=ADMIN, json={"code": "unsafe"})
                ).status_code == 422

        anyio.run(scenario)


def test_no_admin_token_disables_management(tmp_path):
    with running_service(tmp_path, admin_token=None) as (url, _, _):
        assert httpx.get(url + "/v1/admin/tools", headers=ADMIN).status_code == 403


def test_namespace_sessions_and_subscription_filters(tmp_path, monkeypatch):
    with running_service(tmp_path) as (url, app, runtime):

        async def scenario():
            other = {**AUTH, "X-RAG-Namespace": "other"}
            async with httpx.AsyncClient() as http, mcp_client(url) as first, mcp_client(url, headers=other) as second:
                released = threading.Event()
                bus = app.state.mcp_endpoint.peers["manuals"].bus
                subscribe = bus.subscribe

                def watch_release(listener):
                    remove = subscribe(listener)

                    def unsubscribe():
                        remove()
                        released.set()

                    return unsubscribe

                monkeypatch.setattr(bus, "subscribe", watch_release)
                async with (
                    first.listen(tools_list_changed=True) as tools,
                    second.listen(tools_list_changed=True) as others,
                ):
                    await http.patch(url + "/v1/admin/tools/search", headers=ADMIN, json={"description": "updated"})
                    with anyio.fail_after(2):
                        assert isinstance(await anext(tools), ToolsListChanged)
                    with anyio.move_on_after(0.15) as timeout:
                        await anext(others)
                    assert timeout.cancel_called
                    prompt = {"messages": [{"role": "user", "text": "Hello"}]}
                    await http.put(url + "/v1/admin/prompts/hello", headers=ADMIN, json=prompt)
                    with anyio.move_on_after(0.15) as timeout:
                        await anext(tools)
                    assert timeout.cancel_called
                    assert not (await second.list_prompts()).prompts
                assert await anyio.to_thread.run_sync(released.wait, 2)
                with pytest.raises(MCPError):
                    await first.read_resource(document_uri("other", "secret.txt"))
                with pytest.raises(MCPError):
                    await first.read_resource("rag://knowledge/manuals/%2e%2e%2fsecret.txt")
            ids = []

            async def record(response):
                if response.headers.get("mcp-session-id"):
                    ids.append(response.headers["mcp-session-id"])

            async with mcp_client(url, "legacy", hooks={"response": [record]}) as client:
                async with httpx.AsyncClient(headers=other) as http:
                    response = await http.post(
                        url + "/mcp",
                        headers={
                            "Mcp-Session-Id": ids[0],
                            "MCP-Protocol-Version": "2025-11-25",
                            "Accept": "application/json, text/event-stream",
                        },
                        json={"jsonrpc": "2.0", "id": 50, "method": "tools/list"},
                    )
                    assert response.status_code == 404
                assert not (await client.call_tool("search", {"query": "still here"})).is_error

        anyio.run(scenario)


def test_noop_and_failed_writes_do_not_notify(tmp_path, monkeypatch):
    with running_service(tmp_path) as (url, app, runtime):

        async def scenario():
            async with httpx.AsyncClient(headers=AUTH) as http, mcp_client(url) as client:
                await http.post(url + "/v1/documents", files={"files": ("same.txt", b"same")})
                async with client.listen(
                    tools_list_changed=True, prompts_list_changed=True, resources_list_changed=True
                ) as sub:
                    assert not (
                        await http.patch(url + "/v1/admin/tools/search", headers=ADMIN, json={"enabled": True})
                    ).json()["changed"]
                    assert (
                        await http.post(url + "/v1/documents", files={"files": ("same.txt", b"same")})
                    ).status_code == 200
                    assert (await http.post(url + "/v1/documents", files={"files": ("fail.txt", b"bad")})).json()[
                        "failed"
                    ]
                    import os

                    catalog = app.state.mcp_endpoint.catalogs.for_namespace("manuals")
                    original_replace = os.replace

                    def fail_replace(source, target):
                        if Path(target).name == "mcp_catalog.json":
                            raise OSError("controlled atomic replacement failure")
                        return original_replace(source, target)

                    monkeypatch.setattr("rag_mcp.catalog.os.replace", fail_replace)
                    failed = await http.patch(url + "/v1/admin/tools/search", headers=ADMIN, json={"enabled": False})
                    assert failed.status_code == 503
                    assert (await client.list_tools(cache_mode="refresh")).tools
                    assert not list(catalog.root.glob(".mcp-catalog-*.tmp"))
                    with anyio.move_on_after(0.2) as timeout:
                        await anext(sub)
                    assert timeout.cancel_called

        anyio.run(scenario)


def test_configuration_survives_service_restart(tmp_path):
    template = {"messages": [{"role": "user", "text": "Persisted"}]}
    with running_service(tmp_path) as (url, _, _):
        assert httpx.patch(url + "/v1/admin/tools/search", headers=ADMIN, json={"enabled": False}).status_code == 200
        assert httpx.put(url + "/v1/admin/prompts/saved", headers=ADMIN, json=template).status_code == 200
    with running_service(tmp_path) as (url, _, runtime):
        tools = httpx.get(url + "/v1/admin/tools", headers=ADMIN).json()["tools"]
        prompts = httpx.get(url + "/v1/admin/prompts", headers=ADMIN).json()["prompts"]
        assert not tools[0]["enabled"]
        assert prompts[0]["messages"][0]["text"] == "Persisted"
        assert not runtime._tenants


def test_legacy_change_notifications_and_unsubscribe(tmp_path):
    with running_service(tmp_path) as (url, app, runtime):

        async def scenario():
            events_send, events = anyio.create_memory_object_stream(20)

            async def record(message):
                if hasattr(message, "method") and message.method.startswith("notifications/"):
                    await events_send.send(message)

            async with (
                httpx.AsyncClient(headers=AUTH) as http,
                mcp_client(url, "legacy", message_handler=record) as client,
            ):
                await http.post(url + "/v1/documents", files={"files": ("legacy.txt", b"first")})
                with anyio.fail_after(2):
                    assert (await events.receive()).method == "notifications/resources/list_changed"
                uri = document_uri("manuals", "legacy.txt")
                await client.subscribe_resource(uri)
                await http.post(url + "/v1/documents", files={"files": ("legacy.txt", b"second")})
                with anyio.fail_after(2):
                    methods = [(await events.receive()).method, (await events.receive()).method]
                assert set(methods) == {"notifications/resources/list_changed", "notifications/resources/updated"}
                await client.unsubscribe_resource(uri)
                await http.delete(url + "/v1/documents/legacy.txt")
                with anyio.fail_after(2):
                    assert (await events.receive()).method == "notifications/resources/list_changed"
                with anyio.move_on_after(0.15) as timeout:
                    await events.receive()
                assert timeout.cancel_called
                await http.patch(url + "/v1/admin/tools/search", headers=ADMIN, json={"description": "changed"})
                with anyio.fail_after(2):
                    assert (await events.receive()).method == "notifications/tools/list_changed"
                await http.put(
                    url + "/v1/admin/prompts/p", headers=ADMIN, json={"messages": [{"role": "user", "text": "Hi"}]}
                )
                with anyio.fail_after(2):
                    assert (await events.receive()).method == "notifications/prompts/list_changed"

        anyio.run(scenario)


def test_word_and_pdf_resources_over_http(tmp_path):
    from docx import Document
    from pypdf import PdfWriter
    from pypdf.generic import DecodedStreamObject, DictionaryObject, NameObject

    word = Document()
    word.add_paragraph("Word paragraph text")
    word_bytes = io.BytesIO()
    word.save(word_bytes)
    writer = PdfWriter()
    page = writer.add_blank_page(width=300, height=300)
    font = DictionaryObject(
        {
            NameObject("/Type"): NameObject("/Font"),
            NameObject("/Subtype"): NameObject("/Type1"),
            NameObject("/BaseFont"): NameObject("/Helvetica"),
        }
    )
    page[NameObject("/Resources")] = DictionaryObject(
        {NameObject("/Font"): DictionaryObject({NameObject("/F1"): writer._add_object(font)})}
    )
    stream = DecodedStreamObject()
    stream.set_data(b"BT /F1 12 Tf 20 250 Td (PDF paragraph text) Tj ET")
    page[NameObject("/Contents")] = writer._add_object(stream)
    pdf_bytes = io.BytesIO()
    writer.write(pdf_bytes)
    with running_service(tmp_path) as (url, _, _):

        async def scenario():
            async with httpx.AsyncClient(headers=AUTH) as http, mcp_client(url) as client:
                for name, blob, expected in [
                    ("read.docx", word_bytes.getvalue(), "Word paragraph text"),
                    ("read.pdf", pdf_bytes.getvalue(), "PDF paragraph text"),
                ]:
                    response = await http.post(url + "/v1/documents", files={"files": (name, blob)})
                    assert not response.json()["failed"]
                    result = await client.read_resource(document_uri("manuals", name))
                    assert expected in json.loads(result.contents[0].text)["text"]

        anyio.run(scenario)


def test_modern_protocol_headers_are_validated(tmp_path):
    with running_service(tmp_path) as (url, _, _):

        async def scenario():
            captured = []

            async def record(request):
                if request.method == "POST":
                    captured.append((dict(request.headers), json.loads(request.content)))

            async with mcp_client(url, "2026-07-28", hooks={"request": [record]}) as client:
                await client.list_tools()
            headers, body = captured[0]
            headers.pop("content-length", None)
            async with httpx.AsyncClient() as http:
                correct = await http.post(url + "/mcp", headers=headers, json=body)
                assert correct.status_code == 200
                for changes in [
                    {"mcp-protocol-version": "2099-01-01"},
                    {"mcp-method": "tools/call"},
                ]:
                    response = await http.post(url + "/mcp", headers={**headers, **changes}, json=body)
                    assert response.status_code == 400

        anyio.run(scenario)


def test_registry_dispatches_an_additional_local_tool():
    from mcp import types
    from rag_mcp.server import RegisteredTool, ToolRegistry

    calls = []

    def execute(arguments):
        calls.append(arguments["name"])
        return {"document": arguments["name"]}

    registry = ToolRegistry(
        [
            RegisteredTool(
                types.Tool(name="test_document", input_schema={"type": "object"}),
                dict,
                execute,
            )
        ]
    )
    result = registry.invoke("test_document", {"name": "manual.txt"})
    assert calls == ["manual.txt"]
    assert json.loads(result.content[0].text) == {"document": "manual.txt"}


def test_shutdown_releases_open_subscription(tmp_path):
    with running_service(tmp_path) as (url, app, _):

        async def scenario():
            async with mcp_client(url) as client:
                async with client.listen(tools_list_changed=True) as sub:
                    app.state.test_server.should_exit = True
                    with anyio.fail_after(4):
                        with pytest.raises((SubscriptionLost, StopAsyncIteration)):
                            await anext(sub)

        anyio.run(scenario)


def test_partial_upload_notifications_and_resource_listing_pages(tmp_path):
    with running_service(tmp_path) as (url, _, _):

        async def scenario():
            async with httpx.AsyncClient(headers=AUTH) as http, mcp_client(url) as client:
                async with client.listen(resources_list_changed=True) as sub:
                    result = await http.post(
                        url + "/v1/documents",
                        files=[
                            ("files", ("ok.txt", b"ok")),
                            ("files", ("fail.txt", b"bad")),
                        ],
                    )
                    assert [item["name"] for item in result.json()["imported"]] == ["ok.txt"]
                    assert [item["name"] for item in result.json()["failed"]] == ["fail.txt"]
                    with anyio.fail_after(2):
                        assert isinstance(await anext(sub), ResourcesListChanged)
                for batch in range(5):
                    result = await http.post(
                        url + "/v1/documents",
                        files=[("files", (f"{batch}-{number}.txt", f"page {batch}-{number}".encode())) for number in range(10)],
                    )
                    assert not result.json()["failed"]
                page1 = await client.list_resources(cache_mode="refresh")
                assert len(page1.resources) == 50 and page1.next_cursor
                page2 = await client.list_resources(cursor=page1.next_cursor)
                assert len(page2.resources) == 1 and page2.next_cursor is None
                assert (await client.list_resource_templates()).resource_templates
                uri = document_uri("manuals", "ok.txt")
                for bad_uri in [uri + "?page=0", uri + "?page=2", uri + "?page=999&version=old"]:
                    with pytest.raises(MCPError):
                        await client.read_resource(bad_uri)
                with pytest.raises(MCPError):
                    async with client.listen(resource_subscriptions=[uri + "?page=1"]):
                        pass

        anyio.run(scenario)
