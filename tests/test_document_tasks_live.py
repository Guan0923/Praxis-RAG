"""Opt-in disposable MySQL/RocketMQ/Qdrant and separate-process worker test."""

import json
import os
import subprocess
import sys
import threading
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace
from uuid import uuid4

import pytest
from qdrant_client import QdrantClient
from sqlalchemy.engine import make_url

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.config import RagConfig
from rag_mcp.service_knowledge import (
    DenseDocumentIngester,
    KnowledgeDocumentStore,
    RagServiceRuntime,
)
from rag_mcp.sparse import Bm25SparseEncoder
from rag_mcp.state.cache_queue import RocketMQSettings
from rag_mcp.state.document_queue import RocketMQDocumentQueue
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from rag_mcp.storage.qdrant_store import QdrantStore

pytestmark = pytest.mark.skipif(
    not os.getenv("RAG_TEST_DOCUMENT_MYSQL_URL"), reason="Requires disposable document task MySQL schema")
VECTOR = [1.0, 0.0, 0.0, 0.0]


class LiveRuntime(RagServiceRuntime):
    def __init__(self, state, collection):
        self.state = state
        self._inference_lock = threading.RLock()
        self._client = QdrantClient(url=os.getenv("RAG_TEST_QDRANT_URL", "http://127.0.0.1:6333"))
        self.store = QdrantStore(
            collection=collection, vector_size=4, namespace="manuals", client=self._client,
            active_versions=lambda: state.documents.visible_versions("manuals"))
        ingester = DenseDocumentIngester(
            RagConfig(backend="qdrant", collection=collection, vector_size=4),
            SimpleNamespace(embed_documents=lambda texts: [VECTOR for _ in texts]),
            Bm25SparseEncoder(), self.store)
        self.tenant = SimpleNamespace(documents=KnowledgeDocumentStore(state, "manuals"),
                                      ingester=ingester, lock=threading.RLock())

    def _tenant(self, _namespace):
        return self.tenant


def test_real_mcp_admission_and_separate_process_consumer(tmp_path, monkeypatch):
    import anyio
    from test_mcp_http import mcp_client, running_service

    url = os.environ["RAG_TEST_DOCUMENT_MYSQL_URL"]
    assert make_url(url).database.startswith("praxis_document_test_"), "Disposable schema only"
    topic = os.environ["RAG_TEST_DOCUMENT_TOPIC"]
    group = os.environ["RAG_TEST_DOCUMENT_GROUP"]
    assert topic.startswith("praxis_document_test_") and group.startswith("praxis_document_test_")
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    monkeypatch.setenv("RAG_REDIS_URL", os.getenv("RAG_TEST_REDIS_URL", "redis://127.0.0.1:6379/0"))
    monkeypatch.setenv("RAG_REDIS_PREFIX", "rag:document-test:" + uuid4().hex)
    monkeypatch.setenv("RAG_REDIS_ENABLED", "1")
    state = StateStores(StateSettings(url=url))
    collection = "document_test_" + uuid4().hex
    state.knowledge_bases.create({
        "namespace": "manuals", "collection": collection, "embedding_model": "test", "tokenizer": "test",
        **ChunkingConfig("character", 100, 10).metadata("test")})
    queue = RocketMQDocumentQueue(replace(
        RocketMQSettings.from_env(), endpoint=os.getenv("RAG_TEST_ROCKETMQ_ENDPOINT", "127.0.0.1:8081"),
        topic=topic, group=group))
    state.document_dispatcher.transport = queue
    runtime = LiveRuntime(state, collection)
    try:
        with running_service(tmp_path, runtime=runtime) as (base_url, _app, _runtime):
            async def scenario():
                async with mcp_client(base_url) as client:
                    result = await client.call_tool("add_document", {
                        "name": "live.txt", "content": "Live asynchronous MySQL document", "idempotency_key": "once"})
                    assert not result.is_error, result.content
                    payload = json.loads(result.content[0].text)
                    assert set(payload) == {"artifact_id"}
                    artifact_id = payload["artifact_id"]
                    assert state.documents.list("manuals") == []
                    queued = await client.call_tool("get_document_status", {"artifact_id": artifact_id})
                    assert json.loads(queued.content[0].text)["status"] == "queued"
                    code = '''
import os, time
from dataclasses import replace
from test_document_tasks_live import LiveRuntime
from rag_mcp.document_worker import DocumentWorker
from rag_mcp.state.store import StateStores
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.cache_queue import RocketMQSettings
from rag_mcp.state.document_queue import RocketMQDocumentQueue
state = StateStores(StateSettings(url=os.environ['RAG_TEST_DOCUMENT_MYSQL_URL']))
runtime = LiveRuntime(state, os.environ['DOCUMENT_TEST_COLLECTION'])
queue = RocketMQDocumentQueue(replace(RocketMQSettings.from_env(),
    endpoint=os.getenv('RAG_TEST_ROCKETMQ_ENDPOINT', '127.0.0.1:8081'),
    topic=os.environ['RAG_TEST_DOCUMENT_TOPIC'], group=os.environ['RAG_TEST_DOCUMENT_GROUP']))
worker = DocumentWorker(runtime)
try:
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        queue.consume_once(worker.process)
        if state.document_tasks.terminal(os.environ['DOCUMENT_TEST_ARTIFACT']):
            break
    assert state.document_tasks.status('manuals', os.environ['DOCUMENT_TEST_ARTIFACT'])['status'] == 'succeeded'
finally:
    queue.close()
    runtime.close()
'''
                    env = dict(os.environ, DOCUMENT_TEST_COLLECTION=collection, DOCUMENT_TEST_ARTIFACT=artifact_id)
                    env["PYTHONPATH"] = os.pathsep.join([str(Path(__file__).parent), str(Path(__file__).parent.parent / "src")])
                    process = await anyio.to_thread.run_sync(lambda: subprocess.run(
                        [sys.executable, "-c", code], env=env, capture_output=True, text=True, timeout=65, check=False))
                    assert process.returncode == 0, process.stderr[-4000:]
                    status = await client.call_tool("get_document_status", {"artifact_id": artifact_id})
                    assert json.loads(status.content[0].text)["status"] == "succeeded"
                    replay = await client.call_tool("add_document", {
                        "name": "live.txt", "content": "Live asynchronous MySQL document", "idempotency_key": "once"})
                    assert json.loads(replay.content[0].text) == payload
                    assert state.blobs.get("manuals", "source", "live.txt") == b"Live asynchronous MySQL document"
                    hits = runtime.store.dense_search(VECTOR, limit=10, filters={})
                    assert any(hit.payload["text"] == "Live asynchronous MySQL document" for hit in hits)
            anyio.run(scenario)
    finally:
        if runtime._client.collection_exists(collection):
            runtime._client.delete_collection(collection)
        from sqlalchemy import select

        from rag_mcp.state.schema import cache_revisions

        backend = state.cache_sync.backend
        with state.engine.connect() as conn:
            keys = list(conn.execute(select(cache_revisions.c.kind, cache_revisions.c.scope)))
        for kind, scope in keys:
            backend._connection().delete(*backend._keys(kind, scope))
        for key in backend._connection().scan_iter(match=backend.settings.prefix + ":*"):
            backend._connection().delete(key)
        runtime.close()


def test_redis_duplicate_claims_share_execution_state_across_mysql_instances(tmp_path, monkeypatch):
    from concurrent.futures import ThreadPoolExecutor

    url = os.environ["RAG_TEST_DOCUMENT_MYSQL_URL"]
    assert make_url(url).database.startswith("praxis_document_test_")
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    monkeypatch.setenv("RAG_REDIS_URL", os.getenv("RAG_TEST_REDIS_URL", "redis://127.0.0.1:6379/0"))
    monkeypatch.setenv("RAG_REDIS_ENABLED", "1")
    prefix = "rag:document-test:" + uuid4().hex
    monkeypatch.setenv("RAG_REDIS_PREFIX", prefix)
    state = StateStores(StateSettings(url=url))
    other = StateStores(StateSettings(url=url))
    try:
        namespace = "claim_" + uuid4().hex
        state.knowledge_bases.create({"namespace": namespace, "collection": "test"})
        task = state.document_tasks.submit(namespace, "claim.txt", b"claim")
        artifact_id = task["artifact_id"]
        transient = state.document_tasks.transient
        token = transient.lock("document-execution", artifact_id, 30)
        assert token is not None
        assert other.document_tasks.transient.lock("document-execution", artifact_id, 30) is None
        assert not other.document_tasks.transient.unlock("document-execution", artifact_id, "wrong-owner")
        assert transient.unlock("document-execution", artifact_id, token)
        barrier = threading.Barrier(2)

        def claim(tasks):
            barrier.wait()
            return tasks.claim(task)

        with ThreadPoolExecutor(max_workers=2) as pool:
            futures = [pool.submit(claim, instance.document_tasks) for instance in (state, other)]
            claims = [future.result(timeout=10) for future in futures]
        assert all(item is not None for item in claims)
        assert claims[0]["execution_id"] != claims[1]["execution_id"]
        row = state.document_tasks.transient.get("document", artifact_id)
        assert row == other.document_tasks.transient.get("document", artifact_id)
        winner = next(item for item in claims if item["execution_id"] == row["execution_id"])
        loser = next(item for item in claims if item["execution_id"] != row["execution_id"])
        from rag_mcp.state.uploads import UploadConflict
        with pytest.raises(UploadConflict):
            state.document_tasks.progress(loser, "indexing")
        other.document_tasks.progress(winner, "indexing")
        assert state.document_tasks.status(namespace, artifact_id)["stage"] == "indexing"
        # Persisted document publication is tested by the separate-process worker above.
        assert state.documents.list(namespace) == []
    finally:
        backend = state.cache_sync.backend
        for key in backend._connection().scan_iter(match=prefix + ":*"):
            backend._connection().delete(key)
        other.close()
        state.close()
