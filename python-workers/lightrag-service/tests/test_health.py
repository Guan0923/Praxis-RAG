"""Health, configuration reporting and startup-security tests."""

from __future__ import annotations

import pytest

from app import LIGHTRAG_PIN, SERVICE_NAME, __version__
from app.config import Settings, is_loopback_host
from app.errors import ConfigError

from helpers import register_workspace


def test_healthz_matches_the_documented_contract(client):
    response = client.get("/healthz")
    assert response.status_code == 200
    assert response.json() == {
        "status": "ok",
        "service": SERVICE_NAME,
        "version": __version__,
        "upstream": {
            "package": "lightrag-hku",
            "pinned": LIGHTRAG_PIN,
            "installed": None,
        },
        "workspaces": 0,
        "config_error": None,
    }


def test_health_is_also_available_at_root(client):
    payload = client.get("/").json()
    assert payload["service"] == SERVICE_NAME
    assert payload["status"] == "ok"


def test_healthz_counts_registered_workspaces(client):
    register_workspace(client, "legal")
    register_workspace(client, "hr")
    body = client.get("/healthz").json()
    assert body["workspaces"] == 2
    assert body["config_error"] is None


def test_healthz_stays_200_and_reports_config_error(client_factory):
    unsafe_client = client_factory(host="0.0.0.0", token="")
    response = unsafe_client.get("/healthz")
    assert response.status_code == 200
    body = response.json()
    assert body["status"] == "ok"
    assert body["workspaces"] == 0
    assert "RAG_LIGHTRAG_SERVICE_TOKEN" in body["config_error"]


def test_require_startup_security_refuses_non_loopback_without_token():
    settings = Settings(host="0.0.0.0", token="")
    assert settings.config_error is not None
    with pytest.raises(ConfigError):
        settings.require_startup_security()


def test_require_startup_security_accepts_loopback_and_tokened_bind():
    Settings(host="127.0.0.1", token="").require_startup_security()
    Settings(host="0.0.0.0", token="secret").require_startup_security()


def test_loopback_detection():
    assert is_loopback_host("127.0.0.1")
    assert is_loopback_host("127.0.0.42")
    assert is_loopback_host("::1")
    assert is_loopback_host("localhost")
    assert not is_loopback_host("0.0.0.0")
    assert not is_loopback_host("192.168.1.10")
    assert not is_loopback_host("")


def test_settings_from_env_records_invalid_port(monkeypatch):
    monkeypatch.setenv("RAG_LIGHTRAG_SERVICE_PORT", "99999")
    settings = Settings.from_env()
    assert settings.port == 8093
    assert settings.config_error is not None
    assert "RAG_LIGHTRAG_SERVICE_PORT" in settings.config_error


def test_settings_from_env_defaults_are_loopback(monkeypatch):
    for name in (
        "RAG_LIGHTRAG_SERVICE_HOST",
        "RAG_LIGHTRAG_SERVICE_PORT",
        "RAG_LIGHTRAG_SERVICE_TOKEN",
        "RAG_LIGHTRAG_SERVICE_CONCURRENCY",
        "RAG_LIGHTRAG_SERVICE_WORKING_DIR",
    ):
        monkeypatch.delenv(name, raising=False)
    settings = Settings.from_env()
    assert settings.host == "127.0.0.1"
    assert settings.port == 8093
    assert settings.concurrency == 2
    assert settings.config_error is None
