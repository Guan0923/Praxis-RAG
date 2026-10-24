"""Fault injection at construction, SQL execution, COMMIT and post-commit boundaries."""

from contextlib import contextmanager

import pytest
from sqlalchemy import event

pytest_plugins = ["test_knowledge_publication"]

from rag_mcp.service_knowledge import KnowledgeError, KnowledgeValidationError


def test_lost_commit_ack_is_confirmed_and_returns_success(publication, monkeypatch):
    state, runtime, built, deleted = publication
    original = state.knowledge_bases.publish

    def lost(*args, **kwargs):
        original(*args, **kwargs)
        raise OSError("commit acknowledgement lost")

    monkeypatch.setattr(state.knowledge_bases, "publish", lost)
    result = runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert result["collection"] == built[0] == state.knowledge_bases.get("alpha")["collection"]
    assert state.knowledge_bases.version("alpha") == 2
    assert deleted == []


@pytest.mark.parametrize("committed", [False, True])
def test_unreadable_commit_outcome_always_retains_index(publication, monkeypatch, committed):
    state, runtime, built, deleted = publication
    original = state.knowledge_bases.publish

    def lost(*args, **kwargs):
        if committed:
            original(*args, **kwargs)
        raise OSError("unconfirmed commit")

    def unavailable(*_args, **_kwargs):
        raise OSError("database unavailable")

    monkeypatch.setattr(state.knowledge_bases, "publish", lost)
    monkeypatch.setattr(state.knowledge_bases, "confirm_publication", unavailable)
    with pytest.raises(KnowledgeError, match="unconfirmed"):
        runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert built and deleted == []
    assert state.knowledge_bases.get("alpha")["collection"] == (built[0] if committed else "old")
    assert "alpha" not in runtime._tenants


def test_current_old_config_does_not_prove_original_commit_cannot_complete(publication, monkeypatch):
    state, runtime, built, deleted = publication

    def uncertain(*_args, **_kwargs):
        raise TimeoutError("commit may still be in progress")

    monkeypatch.setattr(state.knowledge_bases, "publish", uncertain)
    with pytest.raises(KnowledgeError, match="unconfirmed"):
        runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert state.knowledge_bases.get("alpha")["collection"] == "old"
    assert built and deleted == []


def test_explicit_sql_rollback_allows_cleanup(publication):
    state, runtime, built, deleted = publication

    def fail(_conn, _cursor, statement, _parameters, _context, _many):
        if statement.startswith("UPDATE knowledge_bases SET"):
            raise OSError("statement failed before COMMIT")

    event.listen(state.engine, "before_cursor_execute", fail)
    try:
        with pytest.raises(KnowledgeValidationError, match="回滚"):
            runtime.update_knowledge_base("alpha", {}, reembed=True)
    finally:
        event.remove(state.engine, "before_cursor_execute", fail)
    assert state.knowledge_bases.get("alpha")["collection"] == "old"
    assert state.knowledge_bases.version("alpha") == 1
    assert deleted == built


def test_failed_rollback_is_uncertain_and_retains_index(publication):
    state, runtime, built, deleted = publication

    def fail(conn, _cursor, statement, _parameters, _context, _many):
        if statement.startswith("UPDATE knowledge_bases SET"):
            conn.info["publication_rollback_fault"] = True
            raise OSError("statement failed")

    def rollback_failed(conn):
        if conn.info.pop("publication_rollback_fault", False):
            raise OSError("rollback acknowledgement lost")

    event.listen(state.engine, "before_cursor_execute", fail)
    event.listen(state.engine, "rollback", rollback_failed)
    try:
        with pytest.raises(KnowledgeError, match="unconfirmed"):
            runtime.update_knowledge_base("alpha", {}, reembed=True)
        assert built and deleted == []
    finally:
        event.remove(state.engine, "before_cursor_execute", fail)
        event.remove(state.engine, "rollback", rollback_failed)


def test_config_version_change_rejects_even_with_valid_lease(publication, monkeypatch):
    from rag_mcp.storage.qdrant_store import QdrantStore

    state, runtime, built, deleted = publication

    def concurrent(_self, collection, *_):
        built.append(collection)
        current = state.knowledge_bases.get("alpha")
        state.knowledge_bases.update({**current, "reranker_model": "newer"})

    monkeypatch.setattr(QdrantStore, "reembed_into", concurrent)
    with pytest.raises(KnowledgeValidationError, match="回滚"):
        runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert state.knowledge_bases.get("alpha")["reranker_model"] == "newer"
    assert state.knowledge_bases.get("alpha")["collection"] == "old"
    assert deleted == built


def test_lost_ack_does_not_treat_same_collection_different_settings_as_success(publication, monkeypatch):
    state, runtime, built, deleted = publication
    original = state.knowledge_bases.publish

    def changed(settings, **kwargs):
        original({**settings, "reranker_model": "different"}, **kwargs)
        raise OSError("acknowledgement lost")

    monkeypatch.setattr(state.knowledge_bases, "publish", changed)
    with pytest.raises(KnowledgeError, match="unconfirmed"):
        runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert state.knowledge_bases.get("alpha")["collection"] == built[0]
    assert state.knowledge_bases.get("alpha")["reranker_model"] == "different"
    assert deleted == []


def test_unknown_cleanup_failure_does_not_mask_original_error(publication, monkeypatch):
    from rag_mcp.storage.qdrant_store import QdrantStore

    _state, runtime, _built, _deleted = publication

    def build_failed(*_args, **_kwargs):
        raise ValueError("construction failed")

    def cleanup_failed(*_args, **_kwargs):
        raise OSError("cleanup failed")

    monkeypatch.setattr(QdrantStore, "reembed_into", build_failed)
    runtime._client.delete_collection = cleanup_failed
    with pytest.raises(ValueError, match="construction failed"):
        runtime.update_knowledge_base("alpha", {}, reembed=True)


def test_unconfirmed_result_is_not_masked_by_release_failure(publication, monkeypatch):
    state, runtime, built, deleted = publication
    original = state.uploads.lock

    @contextmanager
    def broken_release(*args, **kwargs):
        with original(*args, **kwargs) as token:
            try:
                yield token
            finally:
                raise OSError("lease release unavailable")

    def uncertain(*_args, **_kwargs):
        raise TimeoutError("commit outcome unknown")

    monkeypatch.setattr(state.uploads, "lock", broken_release)
    monkeypatch.setattr(state.knowledge_bases, "publish", uncertain)
    with pytest.raises(KnowledgeError, match="unconfirmed"):
        runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert built and deleted == []


def test_post_commit_lease_exit_failure_does_not_undo_success(publication, monkeypatch):
    state, runtime, built, deleted = publication
    original = state.uploads.lock

    @contextmanager
    def fail_after(*args, **kwargs):
        with original(*args, **kwargs) as token:
            yield token
        raise OSError("lease cleanup failed after publication")

    monkeypatch.setattr(state.uploads, "lock", fail_after)
    result = runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert result["collection"] == built[0] == state.knowledge_bases.get("alpha")["collection"]
    assert deleted == []


def test_uncertain_commit_superseded_by_newer_config_does_not_return_false_success(publication, monkeypatch):
    state, runtime, _built, deleted = publication
    original = state.knowledge_bases.publish

    def superseded(settings, **kwargs):
        original(settings, **kwargs)
        state.knowledge_bases.update({**settings, "collection": "newer"})
        raise OSError("acknowledgement lost, meanwhile another config committed")

    monkeypatch.setattr(state.knowledge_bases, "publish", superseded)
    with pytest.raises(KnowledgeError, match="unconfirmed"):
        runtime.update_knowledge_base("alpha", {}, reembed=True)
    assert state.knowledge_bases.get("alpha")["collection"] == "newer"
    assert deleted == []


def test_unconfirmed_publication_http_response_is_503(publication, monkeypatch, tmp_path):
    from fastapi.testclient import TestClient

    from rag_mcp.http_service import RagHttpSettings, create_service_app

    state, runtime, built, deleted = publication

    def uncertain(*_args, **_kwargs):
        raise TimeoutError("commit outcome unknown")

    monkeypatch.setattr(state.knowledge_bases, "publish", uncertain)
    app = create_service_app(runtime, settings=RagHttpSettings(data_root=tmp_path))
    with TestClient(app, base_url="http://127.0.0.1", client=("127.0.0.1", 1234)) as client:
        response = client.patch("/v1/admin/knowledge-bases/alpha", json={"reembed": True})
    assert response.status_code == 503
    assert "unconfirmed" in response.json()["detail"]
    assert built and deleted == []


def test_lease_expires_during_sql_write_rolls_back_pointer(publication):
    from sqlalchemy import update

    from rag_mcp.state.schema import document_leases

    state, runtime, built, deleted = publication

    def expire(conn, _cursor, statement, _parameters, _context, _many):
        if statement.startswith("UPDATE knowledge_bases SET"):
            conn.execute(update(document_leases).where(
                document_leases.c.resource == "namespace:exclusive").values(expires_at=0))

    event.listen(state.engine, "after_cursor_execute", expire)
    try:
        with pytest.raises(KnowledgeValidationError, match="回滚"):
            runtime.update_knowledge_base("alpha", {}, reembed=True)
    finally:
        event.remove(state.engine, "after_cursor_execute", expire)
    assert state.knowledge_bases.get("alpha")["collection"] == "old"
    assert state.knowledge_bases.version("alpha") == 1
    assert deleted == built


def test_non_reembed_update_uses_same_safe_publication_path(publication, monkeypatch):
    state, runtime, built, deleted = publication
    original = state.knowledge_bases.publish

    def lost(*args, **kwargs):
        original(*args, **kwargs)
        raise OSError("commit acknowledgement lost")

    monkeypatch.setattr(state.knowledge_bases, "publish", lost)
    result = runtime.update_knowledge_base("alpha", {}, reembed=False)
    assert result["collection"] == "old"
    assert state.knowledge_bases.version("alpha") == 2
    assert built == deleted == []
