"""Persistent namespace-to-workspace binding state.

``POST /v1/workspaces`` is idempotent, and "created" versus "already exists"
must survive a process restart: the Go backend calls it on every ingest and
every search, so answering ``created=true`` again after a restart would make the
caller believe the graph is empty. The binding state is therefore one tiny JSON
file per namespace under ``<working_dir>/state``.

Only non-secret values are stored. Credentials travel in each request and live
in memory inside the engine binding; they are never written here.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path

from .config import validate_namespace_name


@dataclass(frozen=True)
class WorkspaceState:
    """Persisted binding of one Praxis namespace.

    Attributes:
        namespace: Praxis namespace.
        workspace: LightRAG workspace name (``praxis_<namespace>``).
        config_fingerprint: Caller-computed binding fingerprint; a different
            value triggers a wipe-and-rebuild.
        language: Extraction language recorded for diagnostics.
        created_at: When the binding was first created.
        updated_at: When the binding was last written.
    """

    namespace: str
    workspace: str
    config_fingerprint: str
    language: str
    created_at: str
    updated_at: str


class WorkspaceStore:
    """Read/write the per-namespace binding files.

    The store is deliberately not cached: it is tiny, called at most once per
    HTTP request, and reading from disk keeps multiple app instances (tests,
    restarts) consistent without invalidation logic.
    """

    def __init__(self, working_dir: str | Path) -> None:
        """Create a store rooted at ``<working_dir>/state``.

        Args:
            working_dir: Service working directory (settings ``WORKING_DIR``).
        """

        self._directory = Path(working_dir) / "state"

    def load(self, namespace: str) -> WorkspaceState | None:
        """Load a namespace binding, if it exists.

        Args:
            namespace: Praxis namespace.

        Returns:
            The stored state, or ``None`` when the namespace was never created
            or the file is unreadable.
        """

        path = self._path(namespace)
        if not path.exists():
            return None
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return None
        if not isinstance(payload, dict):
            return None
        workspace = payload.get("workspace")
        fingerprint = payload.get("config_fingerprint")
        if not isinstance(workspace, str) or not isinstance(fingerprint, str):
            return None
        return WorkspaceState(
            namespace=namespace,
            workspace=workspace,
            config_fingerprint=fingerprint,
            language=payload.get("language") if isinstance(payload.get("language"), str) else "",
            created_at=payload.get("created_at") if isinstance(payload.get("created_at"), str) else "",
            updated_at=payload.get("updated_at") if isinstance(payload.get("updated_at"), str) else "",
        )

    def save(self, state: WorkspaceState) -> None:
        """Atomically persist a namespace binding.

        Args:
            state: Binding to store.

        Returns:
            ``None``.
        """

        self._directory.mkdir(parents=True, exist_ok=True)
        path = self._path(state.namespace)
        temporary = path.with_suffix(".json.tmp")
        payload = {
            "namespace": state.namespace,
            "workspace": state.workspace,
            "config_fingerprint": state.config_fingerprint,
            "language": state.language,
            "created_at": state.created_at,
            "updated_at": state.updated_at,
        }
        temporary.write_text(json.dumps(payload, ensure_ascii=False), encoding="utf-8")
        os.replace(temporary, path)

    def delete(self, namespace: str) -> None:
        """Remove a namespace binding file.

        Args:
            namespace: Praxis namespace.

        Returns:
            ``None``.
        """

        try:
            self._path(namespace).unlink(missing_ok=True)
        except OSError:
            pass

    def namespaces(self) -> tuple[str, ...]:
        """List every namespace that has a binding on disk.

        Returns:
            Sorted namespace names; unreadable files are skipped.
        """

        if not self._directory.exists():
            return ()
        names: list[str] = []
        for path in sorted(self._directory.glob("*.json")):
            names.append(path.stem)
        return tuple(names)

    def _path(self, namespace: str) -> Path:
        """Return the binding file path after validating the namespace.

        Args:
            namespace: Praxis namespace.

        Returns:
            ``<state dir>/<namespace>.json``.
        """

        validate_namespace_name(namespace)
        return self._directory / f"{namespace}.json"
