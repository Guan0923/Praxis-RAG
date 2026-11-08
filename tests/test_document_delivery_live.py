"""Opt-in real MySQL upload admission: one durable job plus one outbox row."""

import os
from uuid import uuid4

import pytest
from sqlalchemy import event
from sqlalchemy.engine import make_url

from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores

pytestmark = pytest.mark.skipif(
    not os.getenv("RAG_TEST_DOCUMENT_MYSQL_URL"),
    reason="Requires disposable document MySQL schema")


def test_real_mysql_admission_commits_job_and_outbox_together(monkeypatch):
    url = os.environ["RAG_TEST_DOCUMENT_MYSQL_URL"]
    assert make_url(url).database.startswith("praxis_document_test_")
    # MySQL is the durable acceptance point; Redis must not be required here.
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    monkeypatch.setenv("RAG_OUTBOX_BRIDGE", "relay")
    state = StateStores(StateSettings(url=url))
    namespace = "document_" + uuid4().hex

    class Transport:
        def __init__(self):
            self.sent = []

        def send(self, payload):
            self.sent.append(dict(payload))

        def close(self):
            pass

    transport = Transport()
    state.document_dispatcher.transport = transport
    try:
        state.knowledge_bases.create({"namespace": namespace, "collection": "test"})
        statements = []
        event.listen(state.engine, "before_cursor_execute",
                     lambda c, cur, sql, *rest: statements.append(sql))
        first = state.document_dispatcher.submit(namespace, "a.txt", b"source", "same")
        # A replay of the same idempotency key returns the same artifact and
        # emits no additional notification.
        assert state.document_dispatcher.submit(namespace, "a.txt", b"source", "same") == first
        assert len(transport.sent) == 1
        # The notification carries identifiers only, never the file bytes.
        assert "content" not in transport.sent[0]
        assert transport.sent[0]["artifact_id"] == first["artifact_id"]
        assert state.document_jobs.status(namespace, first["artifact_id"])["status"] == "queued"
        # The complete request is durable in MySQL.
        row = state.document_jobs.get(first["artifact_id"], namespace)
        assert row["content"] == b"source"
        assert row["sha256"]
        assert any("document_jobs" in sql for sql in statements)
        assert any("document_job_outbox" in sql for sql in statements)
        # Retired tables stay untouched.
        assert not any("document_tasks" in sql or "document_task_outbox" in sql for sql in statements)
    finally:
        state.close()
