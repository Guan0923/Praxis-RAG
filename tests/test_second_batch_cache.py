"""Strict second-batch status caching never replaces authority checks."""

import json
import time

import pytest
from fastapi.testclient import TestClient
from sqlalchemy import event, select, update

from rag_mcp.http_service import RagHttpSettings, create_service_app
from rag_mcp.state.schema import cache_outbox, document_leases
from rag_mcp.state.store import StateStores
from rag_mcp.state.uploads import LeaseLost
from test_cache_synchronization import MemoryCache, Queue
from test_mcp_http import ProtocolRuntime


@pytest.fixture()
def state(tmp_path, monkeypatch):
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    value = StateStores.create(tmp_path)
    value.cache_sync.backend = MemoryCache()
    value.cache_sync.transport = Queue()
    yield value
    value.close()


def test_cached_operation_contains_steps_but_not_ownership_tokens_or_payloads(state):
    journal = state.uploads
    op = journal.begin("alpha", "file.txt", "doc", "collection", idempotency_key="not_public")
    journal.register_workspace(op, "sensitive-local-path")
    journal.step(op, "parse", lambda: None, {"sensitive_detail": "private-content"})
    # Parent flush is synchronous; this explicit call also supports catch-up tests.
    state.cache_sync.flush_pending()
    result = state.views.upload_operation("alpha", op["id"])
    assert result["status"] == "RUNNING"
    assert result["steps"][0]["name"] == "parse"
    assert result["steps"][0]["status"] == "DONE"
    assert "holder_id" in result
    encoded = json.dumps(result)
    for hidden in (op["owner"], "sensitive-local-path", "private-content", "not_public"):
        assert hidden not in encoded
    assert state.views.upload_operation("beta", op["id"]) is None
    assert state.views.upload_operations("beta") == []
    assert state.views.upload_operations("alpha")[0]["id"] == op["id"]
    journal.rollback(op, lambda _: None)


def test_hits_for_status_head_lease_and_list_do_not_query_mysql(state):
    op = state.uploads.begin("alpha", "file.txt", "doc", "collection")
    state.uploads.step(op, "parse", lambda: None)
    state.cache_sync.flush_pending()
    state.views.upload_operation("alpha", op["id"])
    state.views.upload_operations("alpha")
    state.views.leases("alpha")
    state.views.published_versions("alpha")
    calls = []
    def count(*_):
        calls.append(1)
    event.listen(state.engine, "before_cursor_execute", count)
    try:
        assert state.views.upload_operation("alpha", op["id"])["status"] == "RUNNING"
        assert state.views.upload_operations("alpha")
        assert state.views.leases("alpha")
        assert state.views.published_versions("alpha") == {}
        assert calls == []
    finally:
        event.remove(state.engine, "before_cursor_execute", count)
        state.uploads.rollback(op, lambda _: None)


def test_lease_expiry_is_recalculated_without_redis_write_and_cannot_authorize(state):
    state.uploads.close()
    state.uploads.auto_heartbeat = False
    op = state.uploads.begin("alpha", "file.txt", "doc", "collection")
    state.cache_sync.flush_pending()
    snapshot = state.views.leases("alpha")
    assert all(not item["expired"] for item in snapshot)
    # An expired lease still present in cached display never authorizes a commit.
    with state.engine.begin() as conn:
        conn.execute(update(document_leases).where(document_leases.c.operation_id == op["id"]).values(expires_at=0))
    with pytest.raises(LeaseLost):
        state.uploads.assert_owner(op)
    cached = state.cache_sync.backend.entries["leases", "alpha"]
    for item in cached:
        item["expires_at"] = time.time() - 1
    assert all(item["expired"] for item in state.views.leases("alpha"))
    claimed = state.uploads.claim_recovery(op["id"])
    state.uploads.rollback(claimed, lambda _: None)


def test_stale_head_cache_never_changes_authoritative_visibility_or_commit(state):
    op = state.uploads.begin("alpha", "file.txt", "doc", "collection")
    with state.engine.begin() as conn:
        state.uploads.commit(op, {"ok": True}, conn)
    state.cache_sync.flush_pending()
    assert state.views.published_versions("alpha") == {"doc": op["id"]}
    # Poisoning Redis can affect only display, not MySQL head checks.
    state.cache_sync.backend.entries["heads", "alpha"] = {"doc": "fake"}
    assert state.views.published_versions("alpha") == {"doc": "fake"}
    assert state.uploads.active_versions("alpha") == {"doc": op["id"]}
    with pytest.raises(LeaseLost):
        state.uploads.rollback(op, lambda _: pytest.fail("must not roll back committed version"))


def test_head_and_outbox_both_roll_back_when_publish_transaction_aborts(state):
    op = state.uploads.begin("alpha", "file.txt", "doc", "collection")
    state.cache_sync.flush_pending()
    before = state.views.published_versions("alpha")
    with state.engine.connect() as conn:
        events = set(conn.scalars(select(cache_outbox.c.id)))
    with pytest.raises(OSError):
        with state.engine.begin() as conn:
            state.uploads.commit(op, {"ok": True}, conn)
            raise OSError("transaction abort")
    state.cache_sync.flush_pending()
    assert state.views.published_versions("alpha") == before == {}
    with state.engine.connect() as conn:
        assert set(conn.scalars(select(cache_outbox.c.id))) == events
    state.uploads.rollback(op, lambda _: None)


def test_failed_step_and_rollback_retry_have_cached_outcomes(state):
    op = state.uploads.begin("alpha", "a.txt", "doc", "collection")
    def fail():
        raise OSError("do not expose full exception message")
    with pytest.raises(OSError):
        state.uploads.step(op, "embedding", fail)
    state.cache_sync.flush_pending()
    assert state.views.upload_operation("alpha", op["id"])["status"] == "FAILED"
    with pytest.raises(OSError):
        state.uploads.rollback(op, lambda _: fail())
    state.cache_sync.flush_pending()
    assert state.views.upload_operation("alpha", op["id"])["status"] == "ROLLBACK_FAILED"


def test_status_endpoints_namespace_isolation_and_local_admin_policy(tmp_path):
    runtime = ProtocolRuntime(tmp_path)
    runtime.state.cache_sync.backend = MemoryCache()
    op = runtime.state.uploads.begin("alpha", "a.txt", "doc", "collection")
    runtime.state.cache_sync.flush_pending()
    app = create_service_app(runtime, settings=RagHttpSettings(data_root=tmp_path, token="secret"))
    with TestClient(app, base_url="http://127.0.0.1", client=("127.0.0.1", 12345)) as client:
        headers = {"Authorization": "Bearer secret", "X-RAG-Namespace": "alpha"}
        detail = client.get("/v1/uploads/" + op["id"], headers=headers)
        assert detail.status_code == 200 and detail.json()["display_only"]
        assert "owner" not in detail.json()["operation"]
        assert client.get("/v1/uploads/" + op["id"], headers={**headers, "X-RAG-Namespace": "beta"}).status_code == 404
        assert client.get("/v1/uploads/not-an-id", headers=headers).status_code == 404
        assert client.get("/v1/uploads", headers=headers).json()["limit"] == 100
        assert client.get("/v1/admin/published-versions", headers=headers).status_code == 200
        assert client.get("/v1/admin/upload-leases", headers=headers).status_code == 200
        assert client.get("/v1/admin/upload-leases", headers={**headers, "X-Forwarded-For": "192.0.2.1"}).status_code == 403
        assert client.get("/v1/uploads").status_code == 401
    runtime.state.uploads.rollback(op, lambda _: None)
    runtime.state.close()
