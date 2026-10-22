"""Opt-in real MySQL and Qdrant upload transactions in a disposable schema."""

import io
import os
import subprocess
import sys
from types import SimpleNamespace
from uuid import uuid4

import pytest
from qdrant_client import QdrantClient
from sqlalchemy import delete, update

from rag_mcp.chunk_strategies import ChunkingConfig
from rag_mcp.config import RagConfig
from rag_mcp.service_knowledge import DenseDocumentIngester, KnowledgeDocumentStore
from rag_mcp.sparse import Bm25SparseEncoder
from rag_mcp.state.schema import document_leases, metadata
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from rag_mcp.state.upload_coordinator import UploadCoordinator
from rag_mcp.storage.qdrant_store import QdrantStore

pytestmark = pytest.mark.skipif(not os.getenv("RAG_TEST_MYSQL_URL"), reason="Requires disposable MySQL schema")
VECTOR = [1.0] + [0.0] * 1023
CONFIG = ChunkingConfig("character", 100, 10)


@pytest.fixture()
def real_stack():
    from sqlalchemy.engine import make_url

    url = os.environ["RAG_TEST_MYSQL_URL"]
    assert make_url(url).database.startswith("praxis_txn_test_"), "Never use a production schema"
    state = StateStores(StateSettings(url=url))
    with state.engine.begin() as conn:
        for table in reversed(metadata.sorted_tables):
            conn.execute(delete(table))
    client = QdrantClient(url=os.getenv("RAG_TEST_QDRANT_URL", "http://127.0.0.1:6333"))
    collection = "txn_test_" + uuid4().hex
    vectors = QdrantStore(collection=collection, namespace="alpha", client=client,
                          active_versions=lambda: state.uploads.active_versions("alpha"))
    ingester = DenseDocumentIngester(RagConfig(backend="qdrant", collection=collection),
        SimpleNamespace(embed_documents=lambda texts: [VECTOR for _ in texts]), Bm25SparseEncoder(), vectors)
    documents = KnowledgeDocumentStore(state, "alpha")
    try:
        yield state, client, vectors, ingester, documents
    finally:
        state.close()
        if client.collection_exists(collection):
            client.delete_collection(collection)
        client.close()


def test_mysql_atomic_publication_and_idempotency(real_stack):
    state, client, vectors, ingester, documents = real_stack
    record = documents.store_and_index("a.txt", io.BytesIO(b"first"), ingester, CONFIG, idempotency_key="once")
    replay = documents.store_and_index("a.txt", io.BytesIO(b"first"), ingester, CONFIG, idempotency_key="once")
    assert replay["operation_id"] == record["operation_id"] and replay["replayed"]
    assert state.uploads.get(record["operation_id"])["status"] == "COMMITTED"
    assert [p.payload["text"] for p in vectors.dense_search(VECTOR, limit=10, filters={})] == ["first"]


def test_mysql_partial_qdrant_failure_keeps_old_version(real_stack, monkeypatch):
    state, client, vectors, ingester, documents = real_stack
    first = documents.store_and_index("a.txt", io.BytesIO(b"first"), ingester, CONFIG)
    original = vectors.stage_version

    def fail(*args):
        original(*args)
        raise OSError("simulated connection loss after vector write")

    monkeypatch.setattr(vectors, "stage_version", fail)
    with pytest.raises(OSError):
        documents.store_and_index("a.txt", io.BytesIO(b"replacement"), ingester, CONFIG)
    assert documents.read_source("a.txt") == b"first"
    assert state.uploads.head("alpha", first["document_id"]) == first["operation_id"]
    assert [p.payload["text"] for p in vectors.dense_search(VECTOR, limit=10, filters={})] == ["first"]


def test_mysql_publish_transaction_failure_rolls_back_all_blobs(real_stack, monkeypatch):
    state, client, vectors, ingester, documents = real_stack
    first = documents.store_and_index("a.txt", io.BytesIO(b"first"), ingester, CONFIG)
    original = state.uploads.commit

    def fail(*args):
        original(*args)
        raise OSError("crash before database transaction commit")

    monkeypatch.setattr(state.uploads, "commit", fail)
    with pytest.raises(OSError):
        documents.store_and_index("a.txt", io.BytesIO(b"replacement"), ingester, CONFIG)
    assert documents.read_source("a.txt") == b"first"
    assert state.uploads.head("alpha", first["document_id"]) == first["operation_id"]
    assert [p.payload["text"] for p in vectors.dense_search(VECTOR, limit=10, filters={})] == ["first"]


def test_mysql_abrupt_worker_exit_is_recoverable(real_stack):
    state, client, vectors, ingester, documents = real_stack
    first = documents.store_and_index("a.txt", io.BytesIO(b"first"), ingester, CONFIG)
    env = dict(os.environ, TXN_COLLECTION=vectors.collection, TXN_DOCUMENT=first["document_id"])
    code = '''
import os
from rag_mcp.state.store import StateStores
from rag_mcp.state.settings import StateSettings
from rag_mcp.storage.qdrant_store import QdrantStore
from rag_mcp.chunking import DocumentChunk
state=StateStores(StateSettings(url=os.environ['RAG_TEST_MYSQL_URL']))
state.uploads.auto_heartbeat=False
op=state.uploads.begin('alpha','a.txt',os.environ['TXN_DOCUMENT'],os.environ['TXN_COLLECTION'])
state.uploads.intent(op,'stage_qdrant')
store=QdrantStore(collection=op['collection'],namespace='alpha',url=os.environ.get('RAG_TEST_QDRANT_URL','http://127.0.0.1:6333'))
store.stage_version([DocumentChunk(op['document_id'],'crashed','unpublished','a','a.txt',{})],[[1.0]+[0.0]*1023],None,[],op['id'])
os._exit(17)
'''
    result = subprocess.run([sys.executable, "-c", code], env=env, timeout=30, capture_output=True)
    assert result.returncode == 17, result.stderr.decode(errors="replace")
    unfinished = [op for op in state.uploads.unfinished("alpha") if op["status"] != "COMMITTED"]
    assert len(unfinished) == 1
    operation = unfinished[0]
    assert documents.read_source("a.txt") == b"first"
    assert [p.payload["text"] for p in vectors.dense_search(VECTOR, limit=10, filters={})] == ["first"]
    # Simulate the durable lease expiry without waiting for the production timeout.
    with state.engine.begin() as conn:
        conn.execute(update(document_leases).where(document_leases.c.operation_id == operation["id"]).values(expires_at=0))
    UploadCoordinator(state).recover(lambda _: vectors)
    assert state.uploads.get(operation["id"])["status"] == "ROLLED_BACK"
    UploadCoordinator(state).recover(lambda _: vectors)
    assert state.uploads.head("alpha", first["document_id"]) == first["operation_id"]
