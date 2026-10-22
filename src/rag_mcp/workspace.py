"""Bounded temporary workspaces and subprocess cleanup."""

from __future__ import annotations

import logging
import os
import shutil
import subprocess
import tempfile
import time
from pathlib import Path

WORKSPACE_PREFIXES = ("rag-upload-", "rag-pdf-", "rag-word-")
_MAX_AGE_SECONDS = 24 * 60 * 60

logger = logging.getLogger(__name__)


def create_workspace(prefix: str) -> Path:
    """Create a temporary directory whose name can be reclaimed after a crash."""

    if not prefix.startswith(WORKSPACE_PREFIXES):
        raise ValueError(f"Unexpected workspace prefix: {prefix}")
    return Path(tempfile.mkdtemp(prefix=prefix))


def cleanup_workspace(path: Path | None, *, attempts: int = 5) -> bool:
    """Remove a workspace, retrying briefly for Windows file handles."""

    if path is None:
        return True
    target = Path(path)
    for attempt in range(attempts):
        try:
            shutil.rmtree(target, onerror=_make_writable)
            return True
        except FileNotFoundError:
            return True
        except OSError as exc:
            if attempt + 1 == attempts:
                logger.warning("Could not remove temporary workspace %s: %s", target, exc)
                return False
            time.sleep(0.5 * (attempt + 1))
    return False


def sweep_stale_workspaces(max_age_seconds: int = _MAX_AGE_SECONDS) -> list[Path]:
    """Remove abandoned parse/upload directories left by a killed process."""

    root = Path(tempfile.gettempdir()).resolve()
    removed: list[Path] = []
    try:
        entries = list(root.iterdir())
    except OSError:
        return removed
    cutoff = time.time() - max_age_seconds
    for entry in entries:
        try:
            if entry.is_symlink() or not entry.is_dir():
                continue
            if not entry.name.startswith(WORKSPACE_PREFIXES):
                continue
            if entry.resolve().parent != root:
                continue
            if entry.stat().st_mtime > cutoff:
                continue
            if cleanup_workspace(entry):
                removed.append(entry)
        except OSError:
            continue
    return removed


def run_bounded(command: list[str], *, timeout: int, env: dict[str, str] | None = None) -> subprocess.CompletedProcess:
    """Run a child process and terminate its whole tree on timeout."""

    process = subprocess.Popen(
        command,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        encoding="utf-8",
        errors="replace",
        env=env,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )
    try:
        stdout, stderr = process.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        _terminate_tree(process)
        process.communicate()
        raise
    return subprocess.CompletedProcess(command, process.returncode, stdout, stderr)


def _terminate_tree(process: subprocess.Popen) -> None:
    if os.name == "nt":
        subprocess.run(
            ["taskkill", "/PID", str(process.pid), "/T", "/F"],
            capture_output=True,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
        )
    else:
        process.kill()


def _make_writable(function, path: str, _exc) -> None:
    try:
        os.chmod(path, 0o700)
        function(path)
    except OSError:
        pass
