"""Opt-in MCP -> full-request RocketMQ -> Redis spawned-worker acceptance."""

import json
import os
import subprocess
import sys
import time
from pathlib import Path
from uuid import uuid4

import anyio
import pytest
from sqlalchemy import select
from sqlalchemy.engine import make_url

from rag_mcp.config import RagConfig
from rag_mcp.service_knowledge import RagServiceRuntime
from rag_mcp.state.schema import cache_revisions
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores

pytestmark = pytest.mark.skipif(
    not os.getenv("RAG_TEST_SEARCH_MYSQL_URL"), reason="Requires disposable search task MySQL schema")


def test_real_mcp_queued_bm25_search_and_production_worker(tmp_path, monkeypatch):
    from test_document_tasks_live import LiveRuntime
    from test_mcp_http import mcp_client, running_service

    url = os.environ["RAG_TEST_SEARCH_MYSQL_URL"]
    assert make_url(url).database.startswith("praxis_search_test_")
    topic = os.environ["RAG_TEST_SEARCH_TOPIC"]
    group = os.environ["RAG_TEST_SEARCH_GROUP"]
    assert topic.startswith("praxis_search_test_") and group.startswith("praxis_search_test_")
    redis_prefix = "rag:search-test:" + uuid4().hex
    for key, value in {
        "RAG_ROCKETMQ_ENDPOINT": os.getenv("RAG_TEST_ROCKETMQ_ENDPOINT", "127.0.0.1:8081"),
        "RAG_SEARCH_TOPIC": topic, "RAG_SEARCH_GROUP": group,
        "RAG_REDIS_URL": os.getenv("RAG_TEST_REDIS_URL", "redis://127.0.0.1:6379/0"),
        "RAG_REDIS_PREFIX": redis_prefix, "RAG_REDIS_ENABLED": "1",
    }.items():
        monkeypatch.setenv(key, value)
    state = StateStores(StateSettings(url=url))
    collection = "search_test_" + uuid4().hex
    state.knowledge_bases.create({
        "namespace": "manuals", "collection": collection, "embedding_model": "test", "reranker_model": "",
        "tokenizer": "test", "vector_size": 4, "strategy": "character", "chunk_size": 100, "chunk_overlap": 10,
        "embedding": {}, "reranker": {},
    })
    seed = LiveRuntime(state, collection)
    seed.write_document("manuals", "manual.txt", b"ZXQUEUED731 controller calibration manual", overwrite=False)
    seed._client.close()
    runtime = RagServiceRuntime(RagConfig(backend="qdrant", qdrant_url=os.getenv(
        "RAG_TEST_QDRANT_URL", "http://127.0.0.1:6333")), tmp_path / "api", state=state)
    worker = None
    output = tmp_path / "search-worker.log"
    try:
        with output.open("w", encoding="utf-8") as log, running_service(tmp_path, runtime=runtime) as (base_url, _app, _runtime):
            async def scenario():
                nonlocal worker
                async with mcp_client(base_url) as client:
                    pending = await client.call_tool("search", {
                        "query": "ZXQUEUED731", "retrieval": "bm25", "rerank": "none", "wait_seconds": 0})
                    assert not pending.is_error, pending.content
                    admitted = json.loads(pending.content[0].text)
                    assert set(admitted) == {"result_id"}
                    result_id = admitted["result_id"]
                    queued = await client.call_tool("get_search_result", {"result_id": result_id})
                    assert json.loads(queued.content[0].text)["status"] == "queued"
                    config_path = tmp_path / "worker-config.toml"
                    config_path.write_text('[environment]\nRAG_BACKEND = "qdrant"\n', encoding="utf-8")
                    env = dict(os.environ, RAG_STATE_URL=url, RAG_CONFIG_FILE=str(config_path),
                               QDRANT_URL=os.getenv("RAG_TEST_QDRANT_URL", "http://127.0.0.1:6333"),
                               RAG_SERVICE_DATA_ROOT=str(tmp_path / "worker"))
                    env["PYTHONPATH"] = str(Path(__file__).parent.parent / "src")
                    worker = await anyio.to_thread.run_sync(lambda: subprocess.Popen(
                        [sys.executable, "-m", "rag_mcp.search_worker"], env=env,
                        stdout=log, stderr=subprocess.STDOUT))
                    deadline = time.monotonic() + 50
                    while time.monotonic() < deadline:
                        assert worker.poll() is None, output.read_text(encoding="utf-8")[-4000:]
                        reply = await client.call_tool("get_search_result", {"result_id": result_id})
                        assert not reply.is_error, reply.content
                        result = json.loads(reply.content[0].text)
                        if result.get("status") != "queued":
                            break
                        await anyio.sleep(0.1)
                    else:
                        pytest.fail(output.read_text(encoding="utf-8")[-4000:])
                    assert result["result_id"] == result_id
                    assert any("ZXQUEUED731" in hit["text"] for hit in result["results"])
                    raw = state.cache_sync.backend._connection().get(
                        state.results.transient._key("search-result", result_id))
                    assert json.loads(raw)["payload"]["result_id"] == result_id
                    repeated = await client.call_tool("get_search_result", {"result_id": result_id})
                    assert json.loads(repeated.content[0].text)["results"] == result["results"]
                    within_wait = await client.call_tool("search", {
                        "query": "ZXQUEUED731", "retrieval": "bm25", "rerank": "none", "wait_seconds": 15})
                    assert not within_wait.is_error, within_wait.content
                    completed = json.loads(within_wait.content[0].text)
                    assert completed["result_id"] != result_id and completed["results"]
            anyio.run(scenario)
    finally:
        if worker is not None:
            worker.terminate()
            try:
                worker.wait(timeout=10)
            except subprocess.TimeoutExpired:
                worker.kill()
                worker.wait(timeout=5)
        if runtime._client.collection_exists(collection):
            runtime._client.delete_collection(collection)
        backend = state.cache_sync.backend
        with state.engine.connect() as conn:
            keys = list(conn.execute(select(cache_revisions.c.kind, cache_revisions.c.scope)))
        for kind, scope in keys:
            backend._connection().delete(*backend._keys(kind, scope))
        for key in backend._connection().scan_iter(match=f"{redis_prefix}:*"):
            backend._connection().delete(key)
        runtime.close()
