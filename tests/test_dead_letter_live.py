"""Opt-in SDK 5.1.2/Broker 5.3.3 DLQ regression on freshly provisioned resources.

Run with RAG_TEST_DLQ_LIVE=1; defaults to the local disposable Docker broker.
No service configuration/.env is read. SQL uses an isolated pytest SQLite file; transient state uses injected Redis.
"""

import json
import os
import subprocess
import sys
import time
from uuid import uuid4

import pytest
from redis_stub import MemoryRedis

from rag_mcp.dead_letter_worker import DeadLetterWorker, dead_letter_queue_settings
from rag_mcp.state.cache import RedisStateCache, StateCacheSettings
from rag_mcp.state.cache_queue import RocketMQSettings
from rag_mcp.state.document_queue import RocketMQDocumentQueue
from rag_mcp.state.search_queue import RocketMQSearchQueue
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from rag_mcp.state.transient import TransientUnavailable


@pytest.mark.parametrize("kind,physical_namespace", [("document", ""), ("search", "probe_ns%")])
@pytest.mark.skipif(os.getenv("RAG_TEST_DLQ_LIVE") != "1", reason="Requires opt-in disposable Docker RocketMQ")
def test_broker_exhaustion_redis_failure_redelivery_and_terminal_ack(tmp_path, monkeypatch, kind, physical_namespace):
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
        monkeypatch.setenv("RAG_SEARCH_TIMEOUT_SECONDS", "300")
        state = StateStores(StateSettings(url=f"sqlite+pysqlite:///{tmp_path / 'dlq.sqlite3'}"))
        redis = MemoryRedis()
        state.cache_sync.backend = RedisStateCache(
            StateCacheSettings(prefix=marker, enabled=True), client=redis)
        namespace = "business_namespace"
        state.knowledge_bases.create({"namespace": namespace, "collection": "test"})
        tasks = getattr(state, f"{kind}_tasks")
        key = "artifact_id" if kind == "document" else "result_id"
        if kind == "document":
            pending_task = tasks.submit(namespace, "pending.txt", b"pending")
            succeeded_task = tasks.submit(namespace, "succeeded.txt", b"published")
            transient_kind = "document"
            queue_type = RocketMQDocumentQueue
        else:
            pending_task = tasks.submit(namespace, {"query": "pending"})
            succeeded_task = tasks.submit(namespace, {"query": "published"})
            transient_kind = "search-result"
            queue_type = RocketMQSearchQueue
        pending, succeeded = pending_task[key], succeeded_task[key]
        tasks.transient.update(transient_kind, succeeded,
            lambda row: {**row, "status": "succeeded", "stage": "completed"})
        source = queue_type(RocketMQSettings(endpoint=endpoint, topic=topic, group=source_group))
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
        handler = queue_type(dead_letter_queue_settings(kind))
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
            row = tasks.transient.get(transient_kind, task_id)
            assert row["status"] == ("failed" if task_id == pending else "succeeded")
            if task_id == succeeded:
                assert row["stage"] == "completed"
            real_ack(message)
            acknowledged.append(task_id)

        monkeypatch.setattr(handler_consumer, "ack", ack)
        real_dead_letter = tasks.dead_letter
        failed_redis = []

        def fail_redis_once(payload):
            assert isinstance(payload, dict)
            assert queue_type.validate_payload(payload) == payload
            task_id = payload[key]
            if task_id == pending and not failed_redis:
                failed_redis.append(task_id)
                raise TransientUnavailable("simulated Redis transaction failure")
            return real_dead_letter(payload)

        monkeypatch.setattr(tasks, "dead_letter", fail_redis_once)
        source.send(pending_task)
        source.send(succeeded_task)
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline and len(acknowledged) < 2:
            source.consume_once(lambda payload: source_deliveries.append(payload[key]) or False)
            try:
                handler.consume_once(worker.process)
            except TransientUnavailable:
                assert pending not in acknowledged
                assert tasks.transient.get(transient_kind, pending)["status"] == "queued"
        assert set(source_deliveries) == {pending, succeeded}
        assert not source_acks
        assert failed_redis == [pending]
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
