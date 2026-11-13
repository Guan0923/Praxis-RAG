"""Process configuration and the real multi-process HTTP launcher."""

import io
import os
from pathlib import Path
import re
import socket
import subprocess
import sys
import time
from unittest.mock import patch
import zipfile

import httpx
import pytest

from app.main import Settings, main


@pytest.mark.parametrize("workers", [1, 2])
def test_main_starts_configured_processes(monkeypatch: pytest.MonkeyPatch, workers: int) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_HOST", "127.0.0.1")
    monkeypatch.setenv("PORT", "9123")
    monkeypatch.setenv("RAG_PDF_SERVICE_WORKERS", str(workers))
    monkeypatch.setenv("RAG_PDF_SERVICE_CONCURRENCY", "3")

    with patch("uvicorn.run") as run:
        main()

    run.assert_called_once_with("app.main:app", host="127.0.0.1", port=9123, workers=workers)
    assert Settings.from_env().concurrency == 3


@pytest.mark.parametrize("workers", ["0", "-1", "invalid"])
def test_invalid_process_count_is_reported(monkeypatch: pytest.MonkeyPatch, workers: str) -> None:
    monkeypatch.setenv("RAG_PDF_SERVICE_WORKERS", workers)

    settings = Settings.from_env()

    assert settings.config_error is not None
    assert "RAG_PDF_SERVICE_WORKERS" in settings.config_error


def test_two_service_processes_serve_one_port(tmp_path: Path, sample_pdf: bytes) -> None:
    # Use the real image entrypoint with fake conversion: no vision endpoint is
    # needed to verify that two processes start and preserve the HTTP contract.
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    environment = dict(os.environ)
    environment.update({
        "RAG_PDF_SERVICE_HOST": "127.0.0.1",
        "PORT": str(port),
        "RAG_PDF_SERVICE_WORKERS": "2",
        "RAG_PDF_SERVICE_CONCURRENCY": "1",
        "RAG_PDF_SERVICE_CONVERTER": "fake",
    })
    log_path = tmp_path / "server.log"
    with log_path.open("wb") as log:
        process = subprocess.Popen(
            [sys.executable, "-m", "app.main"],
            cwd=Path(__file__).resolve().parents[1],
            env=environment,
            stdout=log,
            stderr=subprocess.STDOUT,
            creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0,
        )
        try:
            deadline = time.monotonic() + 30
            ready = False
            with httpx.Client(base_url=f"http://127.0.0.1:{port}", trust_env=False, timeout=2) as client:
                while time.monotonic() < deadline:
                    output = log_path.read_text(encoding="utf-8", errors="replace")
                    if process.poll() is not None:
                        pytest.fail(f"PDF launcher exited before readiness:\n{output}")
                    pids = set(re.findall(r"Started server process \[(\d+)\]", output))
                    if len(pids) == 2:
                        try:
                            health = client.get("/healthz")
                            ready = health.status_code == 200 and health.json()["status"] == "ok"
                        except httpx.TransportError:
                            pass
                    if ready:
                        break
                    time.sleep(0.1)
                assert ready, f"Two PDF service processes did not become ready:\n{output}"
                response = client.post(
                    "/v1/parse", files={"file": ("sample.pdf", sample_pdf, "application/pdf")}
                )
                assert response.status_code == 200
                assert response.headers["content-type"] == "application/zip"
                with zipfile.ZipFile(io.BytesIO(response.content)) as archive:
                    assert "manifest.json" in archive.namelist()
        finally:
            if os.name == "nt":
                # Terminate the test-owned process tree, including Uvicorn's
                # spawned workers; Popen.terminate alone only kills the parent.
                subprocess.run(
                    ["taskkill", "/PID", str(process.pid), "/T", "/F"],
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False,
                )
            else:
                process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
