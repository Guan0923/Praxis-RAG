"""Document mutation through actual TCP MCP clients and on-disk source storage."""

import base64
import io
import json
import os
import threading
from uuid import UUID

import anyio
import httpx2 as httpx
import pytest
from docx import Document
from mcp.server.subscriptions import ResourcesListChanged, ResourceUpdated
from model_api_test_support import configure_api_runtime
from test_document_readers import _write_text_pdf
from test_mcp_http import (
    ADMIN,
    AUTH,
    MemoryDocumentTransport,
    TestSearchConsumer,
    mcp_client,
    running_service,
)

from rag_mcp.config import RagConfig
from rag_mcp.document_worker import DocumentWorker
from rag_mcp.resources import document_uri
from rag_mcp.service_knowledge import RagServiceRuntime


def payload(result):
    assert not result.is_error, result.content
    return json.loads(result.content[0].text)


async def add_and_index(client, runtime, arguments):
    cursor = len(runtime.state.document_dispatcher.transport.queued)
    admitted = payload(await client.call_tool("add_document", arguments))
    assert set(admitted) == {"artifact_id"}
    artifact_id = admitted["artifact_id"]
    assert str(UUID(artifact_id)) == artifact_id
    queued = payload(await client.call_tool("get_document_status", {"artifact_id": artifact_id}))
    assert queued == {"artifact_id": artifact_id, "status": "queued", "stage": "queued", "error": None}
    envelopes = runtime.state.document_dispatcher.transport.queued[cursor:]
    assert envelopes and all(item["artifact_id"] == artifact_id and item["schema"] == 2 for item in envelopes)
    expected = (arguments["content"].encode() if "content" in arguments
                else base64.b64decode(arguments["content_base64"]))
    assert b"".join(base64.b64decode(item["data"]) for item in envelopes) == expected
    assert all(len(base64.b64decode(item["data"])) <= 512 * 1024 for item in envelopes)
    worker = DocumentWorker(runtime)
    for index, envelope in enumerate(envelopes):
        complete = await anyio.to_thread.run_sync(worker.process, envelope)
        assert complete is (index == len(envelopes) - 1)
    status = payload(await client.call_tool("get_document_status", {"artifact_id": artifact_id}))
    assert status["status"] == "succeeded" and status["error"] is None
    document = next(item for item in runtime.list_documents("manuals") if item["name"] == arguments["name"])
    return {"document": document, "uri": document_uri("manuals", arguments["name"])}


@pytest.mark.parametrize("mode", ["legacy", "auto", "2026-07-28"])
def test_document_lifecycle_and_namespace_isolation(tmp_path, mode):
    with running_service(tmp_path) as (url, _, runtime):
        async def scenario():
            async with mcp_client(url, mode) as client:
                tools = {tool.name: tool for tool in (await client.list_tools()).tools}
                assert not tools["add_document"].annotations.read_only_hint
                assert tools["update_document"].annotations.destructive_hint
                assert tools["delete_document"].annotations.destructive_hint
                added = await add_and_index(client, runtime, {"name": "manual.txt", "content": "original"})
                assert tools["get_document_status"].annotations.read_only_hint
                assert (await client.call_tool("add_document", {"name": "manual.txt", "content": "wrong"})).is_error
                assert (await client.call_tool("add_document", {"name": "MANUAL.txt", "content": "wrong"})).is_error
                assert (await client.call_tool("update_document", {"name": "missing.txt", "content": "wrong"})).is_error
                updated = payload(await client.call_tool("update_document", {"name": "manual.txt", "content": "replacement"}))
                assert updated["status"] == "updated"
                assert updated["document"]["chunking"] == added["document"]["chunking"]
                resource = await client.read_resource(added["uri"])
                assert json.loads(resource.contents[0].text)["text"] == "replacement"
                unchanged = payload(await client.call_tool("update_document", {"name": "manual.txt", "content": "replacement"}))
                assert unchanged["status"] == "unchanged"
                async with mcp_client(url, mode, headers={**AUTH, "X-RAG-Namespace": "other"}) as other:
                    assert (await other.call_tool("delete_document", {"name": "manual.txt"})).is_error
                    assert (await other.call_tool("update_document", {"name": "manual.txt", "content": "wrong"})).is_error
                    assert not (await other.list_resources()).resources
                assert payload(await client.call_tool("delete_document", {"name": "manual.txt"}))["status"] == "deleted"
                assert not (await client.list_resources(cache_mode="refresh")).resources
                assert (await client.call_tool("delete_document", {"name": "manual.txt"})).is_error
                assert runtime.list_documents("manuals") == []
        anyio.run(scenario)


def test_content_validation_base64_and_failed_indexing(tmp_path):
    with running_service(tmp_path) as (url, _, runtime):
        async def scenario():
            async with mcp_client(url) as client:
                for arguments in [
                    {"name": "../escape.txt", "content": "x"},
                    {"name": "CON.txt", "content": "x"},
                    {"name": "a.exe", "content": "x"},
                    {"name": "a.txt", "content": ""},
                    {"name": "a.txt", "content": "x", "namespace": "other"},
                    {"name": "a.txt", "content": "x", "content_base64": "eA=="},
                    {"name": "a.txt", "content_base64": "not base64!"},
                    {"name": "a.txt", "content_base64": ""},
                    {"name": "a.pdf", "content": "x"},
                    {"name": "a.txt", "path": "C:/private.txt"},
                    {"name": "a.txt", "content": "x" * (10 * 1024 * 1024 + 1)},
                ]:
                    assert (await client.call_tool("add_document", arguments)).is_error
                assert runtime.list_documents("manuals") == []
                pdf = tmp_path / "source.pdf"
                _write_text_pdf(pdf, "A real PDF document")
                pdf_result = await add_and_index(client, runtime, {
                    "name": "manual.pdf", "content_base64": base64.b64encode(pdf.read_bytes()).decode(),
                })
                assert "A real PDF document" in json.loads((await client.read_resource(pdf_result["uri"])).contents[0].text)["text"]
                word = Document()
                word.add_paragraph("A real Word document")
                stream = io.BytesIO()
                word.save(stream)
                result = await add_and_index(client, runtime, {
                    "name": "word.docx", "content_base64": base64.b64encode(stream.getvalue()).decode(),
                })
                assert "A real Word document" in json.loads((await client.read_resource(result["uri"])).contents[0].text)["text"]
                assert (await client.call_tool("update_document", {"name": "word.docx", "content_base64": "eA=="})).is_error
                assert "A real Word document" in runtime.read_document("manuals", "word.docx")[0]
                failed = payload(await client.call_tool("add_document", {"name": "fail.txt", "content": "fail"}))
                assert set(failed) == {"artifact_id"}
                assert "fail.txt" not in {item["name"] for item in runtime.list_documents("manuals")}
                envelope = runtime.state.document_dispatcher.transport.queued[-1]
                assert envelope["artifact_id"] == failed["artifact_id"]
                assert not await anyio.to_thread.run_sync(DocumentWorker(runtime).process, envelope)
                assert runtime.get_document_status("manuals", failed["artifact_id"])["stage"] == "retry_wait"
                assert await anyio.to_thread.run_sync(runtime.state.document_tasks.dead_letter, envelope)
                status = payload(await client.call_tool("get_document_status", failed))
                assert status["status"] == "failed" and status["error"]
                assert "fail.txt" not in {item["name"] for item in runtime.list_documents("manuals")}
                assert not (tmp_path / "manuals/knowledge/fail.txt").exists()
                assert (await client.call_tool("delete_document", {"name": "../word.docx"})).is_error
        anyio.run(scenario)


@pytest.mark.parametrize("mode", ["legacy", "auto"])
def test_ten_mib_file_fits_base64_transport(tmp_path, mode):
    from rag_mcp.service_knowledge import MAX_FILE_BYTES

    content = ("\u4e2d" * (MAX_FILE_BYTES // 3)).encode() + b"x" * (MAX_FILE_BYTES % 3)
    with running_service(tmp_path) as (url, _, runtime):
        async def scenario():
            async with mcp_client(url, mode, timeout=30) as client:
                result = await add_and_index(client, runtime, {
                    "name": "large.txt", "content_base64": base64.b64encode(content).decode(),
                })
                assert result["document"]["size"] == MAX_FILE_BYTES
                rejected = await client.call_tool("update_document", {
                    "name": "large.txt", "content_base64": base64.b64encode(content + b"x").decode(),
                })
                assert rejected.is_error
                assert len(runtime.state.blobs.get("manuals", "source", "large.txt")) == MAX_FILE_BYTES
        anyio.run(scenario)


def test_duplicate_content_still_targets_requested_filename(tmp_path):
    with running_service(tmp_path) as (url, _, runtime):
        async def scenario():
            async with mcp_client(url) as client:
                for name, content in [("a.txt", "same"), ("b.txt", "different"), ("c.txt", "same")]:
                    added = await add_and_index(client, runtime, {"name": name, "content": content})
                    assert added["document"]["name"] == name
                result = payload(await client.call_tool("update_document", {"name": "b.txt", "content": "same"}))
                assert result["status"] == "updated" and result["document"]["name"] == "b.txt"
                assert runtime.read_document("manuals", "b.txt")[0] == "same"
                assert len(runtime.list_documents("manuals")) == 3
        anyio.run(scenario)


def test_mutation_notifications_and_admin_disable(tmp_path):
    with running_service(tmp_path) as (url, _, runtime):
        async def scenario():
            async with mcp_client(url) as client, httpx.AsyncClient(headers=ADMIN) as http:
                added = await add_and_index(client, runtime, {"name": "a.txt", "content": "first"})
                async with client.listen(resources_list_changed=True) as changes:
                    async with client.listen(resource_subscriptions=[added["uri"]]) as updates:
                        for tool, arguments in [("update_document", {"name": "a.txt", "content": "second"}),
                                                ("delete_document", {"name": "a.txt"})]:
                            payload(await client.call_tool(tool, arguments))
                            with anyio.fail_after(2):
                                assert isinstance(await anext(changes), ResourcesListChanged)
                                assert isinstance(await anext(updates), ResourceUpdated)
                response = await http.patch(url + "/v1/admin/tools/add_document", json={"enabled": False})
                assert response.status_code == 200
                assert "add_document" not in {tool.name for tool in (await client.list_tools(cache_mode="refresh")).tools}
                assert (await client.call_tool("add_document", {"name": "blocked.txt", "content": "x"})).is_error
                assert runtime.list_documents("manuals") == []
        anyio.run(scenario)


def test_concurrent_adds_do_not_overwrite(tmp_path):
    with running_service(tmp_path) as (url, _, runtime):
        runtime.state.document_dispatcher.stop()

        async def scenario():
            results = []
            async def add(content):
                async with mcp_client(url) as client:
                    results.append(await client.call_tool("add_document", {"name": "race.txt", "content": content}))
            async with anyio.create_task_group() as tasks:
                tasks.start_soon(add, "one")
                tasks.start_soon(add, "two")
            assert sum(not result.is_error for result in results) == 1
            admitted = payload(next(result for result in results if not result.is_error))
            assert set(admitted) == {"artifact_id"}
            assert runtime.list_documents("manuals") == []
            envelopes = runtime.state.document_dispatcher.transport.queued
            assert len(envelopes) == 1 and envelopes[0]["artifact_id"] == admitted["artifact_id"]
            assert runtime.get_document_status("manuals", admitted["artifact_id"])["stage"] == "queued"
            assert await anyio.to_thread.run_sync(DocumentWorker(runtime).process, envelopes[0])
            assert len(runtime.list_documents("manuals")) == 1
        anyio.run(scenario)


@pytest.mark.skipif(os.getenv("RAG_REAL_SMOKE") != "1", reason="Set RAG_REAL_SMOKE=1 for BGE-M3 + Qdrant")
def test_real_mcp_add_update_delete_and_search(tmp_path):
    runtime = RagServiceRuntime(RagConfig(
        backend="qdrant", qdrant_path=tmp_path / "qdrant", collection="mcp_document_acceptance",
        embedding_device="cuda:0",
    ), tmp_path / "sources")
    runtime.release = threading.Event()
    runtime.wire_responses = []
    try:
        configure_api_runtime(runtime)
        runtime.state.document_dispatcher.transport = MemoryDocumentTransport()
        runtime.search_consumer = TestSearchConsumer(runtime)
        with running_service(tmp_path / "sources", runtime=runtime) as (url, _, _):
            async def scenario():
                async with mcp_client(url, timeout=120) as client:
                    original = "ZXOLD731: Disconnect the original sensor cable before maintenance."
                    replacement = "ZXNEW942: Replace the controller and calibrate the display."
                    await add_and_index(client, runtime, {"name": "manual.txt", "content": original})
                    async def search(query, mode="hybrid"):
                        result = payload(await client.call_tool("search", {
                            "query": query, "retrieval": mode, "rerank": "none", "wait_seconds": 0,
                        }))
                        with anyio.fail_after(120):
                            while set(result) == {"result_id"} or result.get("status") == "queued":
                                await anyio.sleep(0.1)
                                result = payload(await client.call_tool("get_search_result", {
                                    "result_id": result["result_id"],
                                }))
                        return result
                    assert any("ZXOLD731" in row["text"] for row in (await search("ZXOLD731 sensor cable"))["results"])
                    payload(await client.call_tool("update_document", {"name": "manual.txt", "content": replacement}))
                    assert not (await search("ZXOLD731", "bm25"))["results"]
                    assert any("ZXNEW942" in row["text"] for row in (await search("ZXNEW942 controller"))["results"])
                    assert "ZXNEW942" in json.loads((await client.read_resource(document_uri("manuals", "manual.txt"))).contents[0].text)["text"]
                    payload(await client.call_tool("delete_document", {"name": "manual.txt"}))
                    assert not (await search("ZXNEW942 controller"))["results"]
                    assert not runtime.list_documents("manuals")
                    print("REAL_MCP_ACCEPTANCE: add -> searchable; update -> old removed/new searchable; delete -> no results")
            anyio.run(scenario)
    finally:
        runtime._client.close()
