"""Opt-in SDK 5.1.2/Broker 5.3.3 DLQ regression on freshly provisioned resources.

Run with RAG_TEST_DLQ_LIVE=1; defaults to the local disposable Docker broker.
No service configuration/.env is read. SQL uses an isolated pytest SQLite file.
"""

import json
import os
import subprocess
import sys
import time
from uuid import uuid4

import pytest
from sqlalchemy import select, update
from sqlalchemy.exc import SQLAlchemyError

from rag_mcp.dead_letter_worker import DeadLetterWorker, dead_letter_queue_settings
from rag_mcp.state.cache_queue import RocketMQSettings
from rag_mcp.state.document_queue import RocketMQDocumentQueue
from rag_mcp.state.schema import document_tasks, search_tasks
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores


@pytest.mark.parametrize("kind,physical_namespace", [("document", ""), ("search", "probe_ns%")])
@pytest.mark.skipif(os.getenv("RAG_TEST_DLQ_LIVE") != "1", reason="Requires opt-in disposable Docker RocketMQ")
def test_broker_exhaustion_sql_failure_redelivery_and_terminal_ack(tmp_path, monkeypatch, kind, physical_namespace):
    pytest.importorskip("rocketmq")
    marker = "praxis_dlq_test_" + uuid4().hex
    topic = marker + "_topic"
    source_group = physical_namespace + marker + "_source"
    handler_group = marker + "_handler"
    dlq = "%DLQ%" + source_group
    container = os.getenv("RAG_TEST_DLQ_BROKER_CONTAINER", "praxis-rag-rocketmq")
    namesrv = os.getenv("RAG_TEST_DLQ_NAMESRV", "praxis-rag-rocketmq-namesrv:9876")
    cluster = os.getenv("RAG_TEST_DLQ_CLUSTER", "DefaultCluster")
    executable = os.getenv("RAG_TEST_DLQ_MQADMIN", "/home/rocketmq/rocketmq-5.3.3/bin/mqadmin")
    endpoint = os.getenv("RAG_TEST_ROCKETMQ_ENDPOINT", "127.0.0.1:8081")
    clients = []
    state = None

    def admin(*args):
        result = subprocess.run(["docker", "exec", container, executable, *args, "-n", namesrv],
                                capture_output=True, text=True, timeout=30, check=False)
        # Admin/SDK error output may include connection details; never print it.
        assert result.returncode == 0, f"Disposable broker admin command failed: {args[0]}"
        return result.stdout

    try:
        admin("updateTopic", "-c", cluster, "-t", topic, "-p", "6", "-r", "1", "-w", "1")
        admin("updateSubGroup", "-c", cluster, "-g", source_group, "-r", "1")
        admin("updateSubGroup", "-c", cluster, "-g", handler_group, "-r", "16")
        # Automatic DLQ creation is WRITE-only, unsuitable for direct receive.
        admin("updateTopic", "-c", cluster, "-t", dlq, "-p", "6", "-r", "1", "-w", "1")
        monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
        monkeypatch.setenv("RAG_REDIS_ENABLED", "0")
        state = StateStores(StateSettings(url=f"sqlite+pysqlite:///{tmp_path / 'dlq.sqlite3'}"))
        namespace = "business_namespace"
        state.knowledge_bases.create({"namespace": namespace, "collection": "test"})
        tasks = getattr(state, f"{kind}_tasks")
        table = document_tasks if kind == "document" else search_tasks
        column = table.c.artifact_id if kind == "document" else table.c.result_id
        key = "artifact_id" if kind == "document" else "result_id"
        if kind == "document":
            pending = tasks.submit(namespace, "pending.txt", b"pending")
            succeeded = tasks.submit(namespace, "succeeded.txt", b"published")
        else:
            pending = tasks.submit(namespace, {"query": "pending"})
            succeeded = tasks.submit(namespace, {"query": "published"})
        with state.engine.begin() as conn:
            conn.execute(update(table).where(column == succeeded).values(status="succeeded", stage="published"))
            if kind == "search":
                conn.execute(update(table).values(deadline=32503680000))
        source = RocketMQDocumentQueue(RocketMQSettings(endpoint=endpoint, topic=topic, group=source_group))
        source.message_key = key
        source.invisible_seconds = 10
        clients.append(source)
        monkeypatch.setenv("RAG_ROCKETMQ_ENDPOINT", endpoint)
        for name in ("RAG_ROCKETMQ_NAMESPACE", "RAG_ROCKETMQ_ACCESS_KEY", "RAG_ROCKETMQ_SECRET_KEY"):
            monkeypatch.setenv(name, "")
        monkeypatch.setenv("RAG_ROCKETMQ_TLS", "0")
        monkeypatch.setenv(f"RAG_{kind.upper()}_TOPIC", topic)
        monkeypatch.setenv(f"RAG_{kind.upper()}_GROUP", source_group)
        monkeypatch.setenv(f"RAG_{kind.upper()}_DLQ_GROUP", handler_group)
        handler = RocketMQDocumentQueue(dead_letter_queue_settings(kind))
        handler.message_key = key
        handler.invisible_seconds = 10
        clients.append(handler)
        worker = DeadLetterWorker(state, kind)
        source_deliveries = []
        handler_consumer = handler._start_consumer()
        source_consumer = source._start_consumer()
        source_acks = []
        monkeypatch.setattr(source_consumer, "ack", lambda message: source_acks.append(message))
        real_ack = handler_consumer.ack
        acknowledged = []

        def ack(message):
            task_id = json.loads(message.body)[key]
            with state.engine.connect() as conn:
                row = conn.execute(select(table).where(column == task_id)).mappings().one()
            assert row["status"] == ("failed" if task_id == pending else "succeeded")
            assert row["stage"] == ("dead_letter" if task_id == pending else "published")
            real_ack(message)
            acknowledged.append(task_id)

        monkeypatch.setattr(handler_consumer, "ack", ack)
        real_dead_letter = tasks.dead_letter
        failed_sql = []

        def fail_sql_once(task_id):
            if task_id == pending and not failed_sql:
                failed_sql.append(task_id)
                raise SQLAlchemyError("simulated SQL transaction failure")
            return real_dead_letter(task_id)

        monkeypatch.setattr(tasks, "dead_letter", fail_sql_once)
        source.send(pending)
        source.send(succeeded)
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline and len(acknowledged) < 2:
            source.consume_once(lambda task_id: source_deliveries.append(task_id) or False)
            try:
                handler.consume_once(worker.process)
            except SQLAlchemyError:
                assert pending not in acknowledged
                with state.engine.connect() as conn:
                    assert conn.scalar(select(table.c.status).where(column == pending)) == "queued"
        assert set(source_deliveries) == {pending, succeeded}
        assert not source_acks
        assert failed_sql == [pending]
        assert set(acknowledged) == {pending, succeeded}
        # A successful broker ACK must prevent another handler delivery.
        handler.consume_once(lambda _: pytest.fail("ACKed DLQ message redelivered"))
    finally:
        try:
            for client in reversed(clients):
                client.close()
        finally:
            if state is not None:
                state.close()
            # POP retries may create additional topic variants. Delete only this
            # invocation's unpredictable marker, never pre-existing resources.
            topics = set(admin("topicList").splitlines())
            topics.update((topic, dlq))
            for name in sorted(name.strip() for name in topics if marker in name):
                admin("deleteTopic", "-c", cluster, "-t", name)
            for group in (source_group, handler_group):
                admin("deleteSubGroup", "-c", cluster, "-g", group, "-r", "true")


def test_dead_letter_module_cli_help_needs_no_broker_or_runtime():
    result = subprocess.run([sys.executable, "-m", "rag_mcp.dead_letter_worker", "--help"],
                            capture_output=True, text=True, timeout=15, check=True)
    assert "--kind {document,search}" in result.stdout
