"""Opt-in real MySQL/Redis upload admission without SQL task persistence."""

import os
from uuid import uuid4

import pytest
from sqlalchemy import event
from sqlalchemy.engine import make_url

from rag_mcp.state.document_tasks import document_envelopes
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores

pytestmark = pytest.mark.skipif(
    not os.getenv("RAG_TEST_DOCUMENT_MYSQL_URL") or not os.getenv("RAG_TEST_REDIS_URL"),
    reason="Requires disposable document MySQL schema and Redis")


def test_real_mysql_redis_admission_has_no_task_sql(monkeypatch):
    url = os.environ["RAG_TEST_DOCUMENT_MYSQL_URL"]
    assert make_url(url).database.startswith("praxis_document_test_")
    monkeypatch.setenv("RAG_REDIS_URL", os.environ["RAG_TEST_REDIS_URL"])
    monkeypatch.setenv("RAG_REDIS_PREFIX", "rag:test:documents:" + uuid4().hex)
    state = StateStores(StateSettings(url=url))
    namespace = "document_" + uuid4().hex
    class Transport:
        def __init__(self):
            self.sent = []

        def send(self, task):
            self.sent.extend(document_envelopes(task))
        def close(self):
            pass
    transport = Transport()
    state.document_dispatcher.transport = transport
    try:
        state.knowledge_bases.create({"namespace": namespace, "collection": "test"})
        statements = []
        event.listen(state.engine, "before_cursor_execute", lambda c, cur, sql, *rest: statements.append(sql))
        first = state.document_dispatcher.submit(namespace, "a.txt", b"source", "same")
        assert state.document_dispatcher.submit(namespace, "a.txt", b"source", "same") == first
        assert len(transport.sent) == 1
        assert state.document_tasks.status(namespace, first["artifact_id"])["status"] == "queued"
        assert not any("document_tasks" in sql or "document_task_outbox" in sql for sql in statements)
    finally:
        state.close()
