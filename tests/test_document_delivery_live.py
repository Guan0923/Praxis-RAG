"""Opt-in MySQL locking and additive outbox upgrade in a disposable schema."""

import os
import threading
from concurrent.futures import ThreadPoolExecutor
from uuid import uuid4

import pytest
from sqlalchemy import create_engine, inspect, select, text
from sqlalchemy.engine import make_url

from rag_mcp.state.schema import document_task_outbox
from rag_mcp.state.schema_migrations import migrate_document_outbox
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores

pytestmark = pytest.mark.skipif(
    not os.getenv("RAG_TEST_DOCUMENT_MYSQL_URL"), reason="Requires disposable document task MySQL schema")


def test_real_mysql_relays_do_not_send_same_live_generation(monkeypatch):
    url = os.environ["RAG_TEST_DOCUMENT_MYSQL_URL"]
    assert make_url(url).database.startswith("praxis_document_test_")
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    state, other = StateStores(StateSettings(url=url)), StateStores(StateSettings(url=url))
    namespace = "relay_" + uuid4().hex
    state.knowledge_bases.create({"namespace": namespace, "collection": "test"})
    artifact_id = state.document_tasks.submit(namespace, "a.txt", b"source")
    entered, release = threading.Event(), threading.Event()
    seen = []

    class Slow:
        def send(self, incoming):
            seen.append(incoming)
            entered.set()
            assert release.wait(5)

    try:
        with ThreadPoolExecutor(max_workers=1) as pool:
            sending = pool.submit(state.document_tasks.dispatch, artifact_id, Slow())
            try:
                assert entered.wait(2)
                assert other.document_tasks.dispatch(artifact_id, Slow()) is False
                assert other.document_tasks.submit(namespace, "b.txt", b"second")
            finally:
                release.set()
            assert sending.result(timeout=5)
        assert seen == [artifact_id]
        with state.engine.connect() as conn:
            record = conn.execute(select(document_task_outbox).where(
                document_task_outbox.c.artifact_id == artifact_id)).mappings().one()
        assert not record["pending"] and record["owner"] is None
    finally:
        other.close()
        state.close()


def test_real_mysql_additive_upgrade_preserves_pending_records():
    url = os.environ["RAG_TEST_DOCUMENT_MYSQL_URL"]
    assert make_url(url).database.startswith("praxis_document_test_")
    # This test runs last in its dedicated disposable schema; never drop business data.
    engine = create_engine(url)
    try:
        with engine.begin() as conn:
            conn.execute(text("DROP TABLE document_task_outbox"))
            conn.execute(text("CREATE TABLE document_task_outbox (artifact_id VARCHAR(36) PRIMARY KEY, pending BOOLEAN NOT NULL, generation BIGINT NOT NULL, retry_at DOUBLE NOT NULL) ENGINE=InnoDB"))
            conn.execute(text("INSERT INTO document_task_outbox VALUES ('legacy', 1, 9, 12)"))
        migrate_document_outbox(engine)
        migrate_document_outbox(engine)
        assert {"owner", "lease_until", "attempts", "last_error"} <= {
            column["name"] for column in inspect(engine).get_columns("document_task_outbox")}
        with engine.connect() as conn:
            row = conn.execute(text("SELECT * FROM document_task_outbox WHERE artifact_id='legacy'")).mappings().one()
        assert row["pending"] and row["generation"] == 9 and row["retry_at"] == 12
        assert row["owner"] is None and row["lease_until"] == 0 and row["attempts"] == 0
    finally:
        engine.dispose()
