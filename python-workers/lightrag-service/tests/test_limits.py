"""Concurrency-gate and configuration-limit tests."""

from __future__ import annotations

import asyncio

import pytest

from app.config import Settings
from app.limits import ConcurrencyGate


def test_gate_serializes_operations_when_limit_is_one():
    gate = ConcurrencyGate(1)
    state = {"active": 0, "peak": 0}

    async def worker() -> None:
        async with gate.slot():
            state["active"] += 1
            state["peak"] = max(state["peak"], state["active"])
            await asyncio.sleep(0.02)
            state["active"] -= 1

    async def run() -> None:
        await asyncio.gather(*(worker() for _ in range(5)))

    asyncio.run(run())
    assert state["peak"] == 1
    assert state["active"] == 0


def test_gate_allows_configured_parallelism():
    gate = ConcurrencyGate(2)
    state = {"active": 0, "peak": 0}

    async def worker() -> None:
        async with gate.slot():
            state["active"] += 1
            state["peak"] = max(state["peak"], state["active"])
            await asyncio.sleep(0.02)
            state["active"] -= 1

    async def run() -> None:
        await asyncio.gather(*(worker() for _ in range(6)))

    asyncio.run(run())
    assert state["peak"] == 2


def test_gate_releases_the_slot_on_failure():
    gate = ConcurrencyGate(1)
    state = {"active": 0}

    async def failing() -> None:
        async with gate.slot():
            state["active"] += 1
            raise RuntimeError("boom")

    async def succeeding() -> None:
        async with gate.slot():
            state["active"] -= 1

    async def run() -> None:
        with pytest.raises(RuntimeError):
            await failing()
        await succeeding()

    asyncio.run(run())
    assert state["active"] == 0


def test_gate_rejects_invalid_limit():
    with pytest.raises(ValueError):
        ConcurrencyGate(0)


def test_settings_validate_concurrency(monkeypatch):
    monkeypatch.setenv("RAG_LIGHTRAG_SERVICE_CONCURRENCY", "0")
    settings = Settings.from_env()
    assert settings.concurrency == 2
    assert settings.config_error is not None
    assert "RAG_LIGHTRAG_SERVICE_CONCURRENCY" in settings.config_error


def test_settings_validate_log_format(monkeypatch):
    monkeypatch.setenv("RAG_LIGHTRAG_SERVICE_LOG_FORMAT", "xml")
    settings = Settings.from_env()
    assert settings.log_format == "text"
    assert settings.config_error is not None
