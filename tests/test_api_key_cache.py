"""API-key caches are display-only; credentials and authorization stay in SQL."""

import base64
import hashlib
from unittest.mock import Mock

import pytest
from sqlalchemy import event, select
from sqlalchemy.exc import SQLAlchemyError

from rag_mcp.api_keys import ApiKeyError, ApiKeyStore
from rag_mcp.state.cache import CacheUnavailable
from rag_mcp.state.repositories import ApiKeyRepository, StateError
from rag_mcp.state.schema import api_keys, cache_revisions
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores
from test_cache_synchronization import MemoryCache, Queue, rows


class WriteUnavailableCache(MemoryCache):
    def put(self, *args, **kwargs):
        raise CacheUnavailable("writes unavailable")


@pytest.fixture()
def state(monkeypatch):
    monkeypatch.delenv("RAG_REDIS_URL", raising=False)
    monkeypatch.delenv("RAG_ROCKETMQ_ENDPOINT", raising=False)
    monkeypatch.setenv("RAG_REDIS_ENABLED", "0")
    settings = StateSettings(
        url="sqlite+pysqlite:///:memory:",
        encryption_key="base64:" + base64.b64encode(b"k" * 32).decode(),
    )
    value = StateStores(settings)
    value.cache_sync.backend = MemoryCache()
    value.cache_sync.transport = Queue()
    yield value
    value.close()


def imported(secret="prk_legacy_test_key", **overrides):
    return {
        "key_id": "legacy", "name": "Legacy", "prefix": secret[:12],
        "digest": hashlib.sha256(secret.encode()).hexdigest(),
        "namespaces": ["alpha"], "enabled": True,
        "created_at": "2026-01-01", "protected_key": None, **overrides,
    }


def test_facade_cache_hit_is_redacted_and_executes_no_sql(state, monkeypatch):
    store = ApiKeyStore(state)
    created = store.create("Application", ["alpha", "beta"])
    with state.engine.connect() as conn:
        credential = conn.execute(select(api_keys)).one()
    expected = state.api_keys.list()
    monkeypatch.setattr(state.api_keys, "list", Mock(side_effect=AssertionError("repository list used")))
    executed = []

    def count(*_):
        executed.append(True)

    event.listen(state.engine, "before_cursor_execute", count)
    try:
        assert store.list() == expected
        assert store.list() == expected
        assert executed == []
    finally:
        event.remove(state.engine, "before_cursor_execute", count)
    assert set(expected[0]) == {
        "id", "name", "prefix", "namespaces", "enabled", "created_at", "exportable",
    }
    assert expected[0]["enabled"] is True
    assert expected[0]["exportable"] is True
    assert expected[0]["namespaces"] == ["alpha", "beta"]
    payload = repr(state.cache_sync.backend.entries)
    for private in (created["key"], credential.digest, credential.protected_key, "protected_key", "digest"):
        assert private not in payload


def test_mutations_refresh_display_after_commit(state):
    store = ApiKeyStore(state)
    assert store.list() == []
    created = store.create("Original", ["alpha"])
    assert store.list()[0]["id"] == created["id"]
    previous = state.cache_sync.backend.watermarks["api_keys", ""]
    updated = store.update(created["id"], "Renamed", ["beta"], False)
    assert store.list() == [updated]
    assert state.cache_sync.backend.watermarks["api_keys", ""] > previous
    assert store.delete(created["id"]) is True
    assert store.list() == []
    assert store.delete(created["id"]) is False
    assert store.update("missing", "Unused", ["alpha"], False) is None
    assert store.list() == []
    assert all(row["status"] == "DONE" for row in rows(state))


def test_digest_only_imports_refresh_without_exporting_credentials(state):
    store = ApiKeyStore(state)
    assert store.list() == []
    state.api_keys.import_row(**imported())
    assert store.list()[0]["exportable"] is False
    assert store.namespaces_for_key("prk_legacy_test_key") == ["alpha"]
    with pytest.raises(ValueError, match="摘要"):
        store.secret_for_namespace("legacy", "alpha")
    state.api_keys.import_row(**imported(name="Changed", namespaces=["beta"], enabled=False))
    assert store.list()[0]["name"] == "Changed"
    assert store.list()[0]["namespaces"] == ["beta"]
    assert store.list()[0]["enabled"] is False
    payload = repr(state.cache_sync.backend.entries)
    assert "prk_legacy_test_key" not in payload
    assert imported()["digest"] not in payload


@pytest.mark.parametrize("mutation", ["disable", "delete", "permissions"])
def test_write_outage_stale_display_never_grants_authorization(state, mutation):
    store = ApiKeyStore(state)
    created = store.create("Application", ["alpha"])
    cached = store.list()
    old_backend = state.cache_sync.backend
    unavailable = WriteUnavailableCache()
    unavailable.entries = old_backend.entries
    unavailable.watermarks = old_backend.watermarks
    state.cache_sync.backend = unavailable
    if mutation == "delete":
        store.delete(created["id"])
    else:
        store.update(
            created["id"], "Changed",
            ["beta"] if mutation == "permissions" else ["alpha"],
            enabled=mutation != "disable",
        )
    assert store.list() == cached
    assert state.api_keys.list() != cached
    executed = []

    def count(*_):
        executed.append(True)

    event.listen(state.engine, "before_cursor_execute", count)
    try:
        for _ in range(2):
            if mutation == "permissions":
                assert store.namespaces_for_key(created["key"]) == ["beta"]
            else:
                with pytest.raises(ApiKeyError):
                    store.namespaces_for_key(created["key"])
            with pytest.raises((LookupError, PermissionError)):
                store.secret_for_namespace(created["id"], "alpha")
        assert len(executed) == 4
    finally:
        event.remove(state.engine, "before_cursor_execute", count)
    pending = [row for row in rows(state) if row["status"] == "QUEUED"]
    assert pending
    assert set(state.cache_sync.transport.messages) == {row["id"] for row in pending}
    assert created["key"] not in repr(state.cache_sync.transport.messages)
    state.cache_sync.backend = old_backend
    state.cache_sync.transport.consume_once(state.cache_sync.refresh_event)
    assert store.list() == state.api_keys.list()
    assert all(row["status"] == "DONE" for row in rows(state))


def test_secret_export_reads_sql_each_time_without_using_display_cache(state, monkeypatch):
    store = ApiKeyStore(state)
    created = store.create("Application", ["alpha"])
    monkeypatch.setattr(state.views, "api_keys", Mock(side_effect=AssertionError("display cache used")))
    executed = []

    def count(*_):
        executed.append(True)

    event.listen(state.engine, "before_cursor_execute", count)
    try:
        for _ in range(2):
            assert store.namespaces_for_key(created["key"]) == ["alpha"]
            assert store.secret_for_namespace(created["id"], "alpha") == created["key"]
        assert len(executed) == 4
    finally:
        event.remove(state.engine, "before_cursor_execute", count)


def test_total_cache_outage_falls_back_to_redacted_sql(state):
    store = ApiKeyStore(state)
    created = store.create("Application", ["alpha"])
    state.cache_sync.backend.down = True
    store.update(created["id"], "Changed", ["beta"], False)
    assert store.list() == state.api_keys.list()
    assert store.list()[0]["enabled"] is False
    assert created["key"] not in repr(store.list())
    with pytest.raises(ApiKeyError):
        store.namespaces_for_key(created["key"])


@pytest.mark.parametrize("mutation", ["create", "update", "delete", "import"])
def test_failed_mutation_rolls_back_revision_and_outbox(state, monkeypatch, mutation):
    created = state.api_keys.create("Application", ["alpha"])
    before_rows = state.api_keys.list()
    before_outbox = rows(state)
    with state.engine.connect() as conn:
        before_revisions = conn.execute(select(cache_revisions)).all()
    flush = Mock(wraps=state.cache_sync.flush_pending)
    monkeypatch.setattr(state.cache_sync, "flush_pending", flush)
    statements = []

    def fail_business_write(conn, cursor, statement, parameters, context, executemany):
        statements.append(statement.lower())
        if context.compiled is not None and (context.isinsert or context.isupdate or context.isdelete):
            if context.compiled.statement.table.name == "api_keys":
                raise SQLAlchemyError("controlled mutation failure")

    event.listen(state.engine, "before_cursor_execute", fail_business_write)
    try:
        with pytest.raises((StateError, SQLAlchemyError)):
            if mutation == "create":
                state.api_keys.create("Other", ["beta"])
            elif mutation == "update":
                state.api_keys.update(created["id"], "Changed", ["beta"], False)
            elif mutation == "delete":
                state.api_keys.delete(created["id"])
            else:
                state.api_keys.import_row(**imported())
    finally:
        event.remove(state.engine, "before_cursor_execute", fail_business_write)
    first_business = next(i for i, sql in enumerate(statements) if "api_keys" in sql)
    assert any("cache_outbox" in sql for sql in statements[:first_business])
    flush.assert_not_called()
    assert state.api_keys.list() == before_rows
    assert ApiKeyStore(state).list() == before_rows
    assert rows(state) == before_outbox
    with state.engine.connect() as conn:
        assert conn.execute(select(cache_revisions)).all() == before_revisions


@pytest.mark.parametrize("mutation", ["create", "update", "delete", "import"])
def test_refresh_is_scoped_and_runs_only_after_owned_commit(state, monkeypatch, mutation):
    created = state.api_keys.create("Application", ["alpha"])
    committed = []
    dispatched = []

    def commit(conn):
        committed.append(True)

    def flush(*, keys):
        assert committed
        assert keys == (("api_keys", ""),)
        assert rows(state)[-1]["kind"] == "api_keys"
        dispatched.append(True)

    monkeypatch.setattr(state.cache_sync, "flush_pending", flush)
    event.listen(state.engine, "commit", commit)
    try:
        if mutation == "create":
            state.api_keys.create("Other", ["beta"])
        elif mutation == "update":
            state.api_keys.update(created["id"], "Changed", ["beta"], False)
        elif mutation == "delete":
            state.api_keys.delete(created["id"])
        else:
            state.api_keys.import_row(**imported())
    finally:
        event.remove(state.engine, "commit", commit)
    assert dispatched == [True]


def test_optional_cache_sync_preserves_direct_repository_constructor(state):
    repository = ApiKeyRepository(state.engine, state.settings.encryption_key)
    created = repository.create("Application", ["alpha"])
    assert repository.list()[0]["id"] == created["id"]
    assert repository.namespaces_for_key(created["key"]) == ["alpha"]
    assert repository.update(created["id"], "Changed", ["beta"], False)["enabled"] is False
    assert repository.delete(created["id"]) is True
    repository.import_row(**imported())
    assert repository.list()[0]["id"] == "legacy"
    assert ApiKeyRepository(state.engine).list() == repository.list()
