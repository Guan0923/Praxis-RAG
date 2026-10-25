"""Persistent startup configuration and a real isolated service launch."""

import os
import socket
import subprocess
import sys
import time
from pathlib import Path

import httpx
import pytest

from rag_mcp.config import RagConfigError
from rag_mcp.startup_config import DEFAULT_CONFIG, load_service_config


def test_first_start_creates_config_without_overwriting_edits(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    monkeypatch.setattr(os, "environ", {})
    path = load_service_config()
    assert path == tmp_path / ".praxis-rag" / "config.toml"
    assert os.environ["RAG_BACKEND"] == "qdrant"
    edited = DEFAULT_CONFIG.replace('"8011"', '"8099"')
    path.write_text(edited, encoding="utf-8")
    os.environ.clear()
    load_service_config()
    assert os.environ["RAG_SERVICE_PORT"] == "8099"
    assert path.read_text(encoding="utf-8") == edited


@pytest.mark.parametrize("content", ['[environment\n', '[environment]\nRAG_SERVICE_PORRT = "8011"',
                                     '[environment]\nRAG_SERVICE_PORT = 8011'])
def test_invalid_config_is_rejected(tmp_path, monkeypatch, content):
    path = tmp_path / "config.toml"
    path.write_text(content, encoding="utf-8")
    monkeypatch.setattr(os, "environ", {"RAG_CONFIG_FILE": str(path)})
    with pytest.raises(RagConfigError):
        load_service_config()


def test_document_worker_settings_can_be_loaded_from_config(tmp_path, monkeypatch):
    path = tmp_path / "config.toml"
    path.write_text('''[environment]
RAG_DOCUMENT_TOPIC = "documents"
RAG_DOCUMENT_GROUP = "document_workers"
RAG_DOCUMENT_DLQ_GROUP = "document_workers_dlq"
RAG_DOCUMENT_BROKER_MAX_RETRIES = "24"
RAG_RESULT_TTL_SECONDS = "7200"
''', encoding="utf-8")
    monkeypatch.setattr(os, "environ", {"RAG_CONFIG_FILE": str(path)})
    load_service_config()
    assert os.environ["RAG_DOCUMENT_TOPIC"] == "documents"
    assert os.environ["RAG_DOCUMENT_GROUP"] == "document_workers"
    assert os.environ["RAG_DOCUMENT_DLQ_GROUP"] == "document_workers_dlq"
    from rag_mcp.state.document_queue import document_queue_settings
    from rag_mcp.state.result_snapshots import SnapshotSettings

    queue = document_queue_settings()
    assert (queue.topic, queue.group) == ("documents", "document_workers")
    assert os.environ["RAG_DOCUMENT_BROKER_MAX_RETRIES"] == "24"
    assert SnapshotSettings.from_env().ttl_seconds == 7200


def test_legacy_document_task_settings_are_accepted_but_ignored(tmp_path, monkeypatch):
    legacy = {
        "RAG_DOCUMENT_MAX_ATTEMPTS": "5", "RAG_DOCUMENT_LEASE_SECONDS": "40",
        "RAG_DOCUMENT_RETRY_SECONDS": "3", "RAG_DOCUMENT_DISPATCH_LEASE_SECONDS": "40",
        "RAG_DOCUMENT_DISPATCH_RETRY_SECONDS": "3", "RAG_DOCUMENT_DISPATCH_MAX_RETRY_SECONDS": "180",
        "RAG_DOCUMENT_DISPATCH_ALERT_SECONDS": "90",
    }
    path = tmp_path / "config.toml"
    path.write_text('[environment]\n' + '\n'.join(f'{key} = "{value}"' for key, value in legacy.items()),
                    encoding="utf-8")
    monkeypatch.setattr(os, "environ", {"RAG_CONFIG_FILE": str(path)})
    load_service_config()
    assert not legacy.keys() & os.environ.keys()


def test_search_worker_settings_can_be_loaded_from_config(tmp_path, monkeypatch):
    path = tmp_path / "config.toml"
    path.write_text('''[environment]
RAG_SEARCH_TOPIC = "searches"
RAG_SEARCH_GROUP = "search_workers"
RAG_SEARCH_DLQ_GROUP = "search_workers_dlq"
RAG_SEARCH_TIMEOUT_SECONDS = "90"
''', encoding="utf-8")
    monkeypatch.setattr(os, "environ", {"RAG_CONFIG_FILE": str(path)})
    load_service_config()
    assert os.environ["RAG_SEARCH_TOPIC"] == "searches"
    assert os.environ["RAG_SEARCH_GROUP"] == "search_workers"
    assert os.environ["RAG_SEARCH_DLQ_GROUP"] == "search_workers_dlq"
    assert os.environ["RAG_SEARCH_TIMEOUT_SECONDS"] == "90"


def test_real_service_reads_config_and_environment_override(tmp_path):
    path = tmp_path / "config.toml"
    data = tmp_path / "data"
    path.write_text(DEFAULT_CONFIG.replace("~/.praxis-rag/data", data.as_posix()), encoding="utf-8")
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(("RAG_", "QDRANT_"))}
    env.update(RAG_CONFIG_FILE=str(path), RAG_SERVICE_PORT=str(port), QDRANT_PATH=str(tmp_path / "qdrant"))
    log = tmp_path / "service.log"
    with log.open("w", encoding="utf-8") as output:
        python = os.getenv("PRAXIS_TEST_PYTHON", sys.executable)
        process = subprocess.Popen([python, "-m", "rag_mcp.http_service"], env=env,
                                   stdout=output, stderr=subprocess.STDOUT)
        try:
            with httpx.Client(base_url=f"http://127.0.0.1:{port}", trust_env=False, timeout=2) as client:
                deadline = time.monotonic() + 30
                while time.monotonic() < deadline and process.poll() is None:
                    try:
                        response = client.get("/health")
                        response.raise_for_status()
                        break
                    except httpx.HTTPError:
                        time.sleep(0.2)
                else:
                    pytest.fail(log.read_text(encoding="utf-8"))
                assert response.json()["backend"] == "qdrant"
                assert client.get("/console").status_code == 200
                assert data.is_dir()
                assert (tmp_path / "qdrant").is_dir()
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=10)
