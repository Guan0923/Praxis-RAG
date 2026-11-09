"""Shared MySQL fixtures for the state layer.

The state layer is MySQL-only: there is no SQLite fallback. Point
``RAG_TEST_MYSQL_URL`` at a *disposable* schema whose name contains ``test``
(for example ``praxis_unit_test``). The autouse fixture below exports that URL
as ``RAG_STATE_URL`` and truncates every state table before each test, so tests
that call ``StateStores.create(...)`` or ``StateSettings.from_env()`` keep
working without repeating connection details.

When ``RAG_TEST_MYSQL_URL`` is absent the fixture stays inert and state-backed
tests fail with the configuration error raised by ``StateSettings.from_env``.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest
from sqlalchemy import create_engine, delete
from sqlalchemy.engine import make_url

from rag_mcp.state.schema import metadata
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores

ENV_VAR = "RAG_TEST_MYSQL_URL"
_SAFE_FRAGMENT = "test"
_DOTENV = Path(__file__).resolve().parents[1] / ".env"


def _dotenv(name: str) -> str | None:
    """Read one key from the repository .env without requiring python-dotenv."""

    if not _DOTENV.is_file():
        return None
    for line in _DOTENV.read_text(encoding="utf-8").splitlines():
        entry = line.strip()
        if not entry or entry.startswith("#") or "=" not in entry:
            continue
        key, _, value = entry.partition("=")
        if key.strip() == name:
            return value.strip().strip('"').strip("'") or None
    return None


def _configured_url() -> str | None:
    raw = os.getenv(ENV_VAR, "").strip() or _dotenv(ENV_VAR)
    if not raw:
        return None
    url = make_url(raw)
    if url.get_backend_name() not in {"mysql", "mariadb"}:
        pytest.fail(f"{ENV_VAR} 必须是 MySQL/MariaDB 连接串，当前为 {url.get_backend_name()!r}")
    database = (url.database or "").lower()
    if _SAFE_FRAGMENT not in database:
        pytest.fail(
            f"{ENV_VAR} 必须指向名称含 {_SAFE_FRAGMENT!r} 的一次性库，当前为 {url.database!r}；"
            "拒绝清空可能的真实数据"
        )
    return raw


@pytest.fixture(scope="session")
def mysql_url() -> str:
    """Validated connection string of the disposable test schema."""

    url = _configured_url()
    if url is None:
        pytest.skip(f"{ENV_VAR} 未设置：请指向一个可清空的一次性 MySQL 库（名称需含 'test'）")
    return url


@pytest.fixture(scope="session")
def _state_engine(mysql_url: str):
    state = StateStores(StateSettings(url=mysql_url))
    state.close()
    engine = create_engine(mysql_url, pool_pre_ping=True, future=True)
    try:
        yield engine
    finally:
        engine.dispose()


def _truncate(engine) -> None:
    with engine.begin() as conn:
        for table in reversed(metadata.sorted_tables):
            conn.execute(delete(table))


@pytest.fixture(autouse=True)
def _mysql_state_environment(monkeypatch, request):
    """Point the production configuration at the test schema and clear it."""

    url = _configured_url()
    if url is None:
        yield
        return
    engine = request.getfixturevalue("_state_engine")
    _truncate(engine)
    monkeypatch.setenv("RAG_STATE_URL", url)
    monkeypatch.setenv("RAG_STATE_AUTO_SCHEMA", "1")
    yield


@pytest.fixture()
def state_url(mysql_url: str) -> str:
    """Test URL whose schema was truncated for this test."""

    return mysql_url


@pytest.fixture()
def state_settings(state_url: str) -> StateSettings:
    return StateSettings(url=state_url)
