"""Version-scoped upload compensation and journal-driven crash recovery.

Record a ``stage_qdrant`` step (or ``qdrant`` / ``qdrant.<action>`` /
``qdrant_<action>`` / ``qdrant-<action>``) before attempting vector writes.
Even an unfinished or failed step is an intent: delete its version on rollback.
ROLLED_BACK vector intents remain tombstones for one idempotent deletion on each
scheduled recovery pass, containing writes completed late by expired workers.
Rollback workspace paths are intentionally retained as tombstones too: a stale
parser may recreate its directory after removal. Only committed paths are cleared.
Workspaces must be registered before creation, directly below workspace_root
(default: the system temporary directory), as ``rag-txn-{operation_id}`` with
an optional ``-suffix``. Recovery never sweeps unregistered directories.
"""

from __future__ import annotations

import logging
import re
import shutil
import socket
import stat
import tempfile
from pathlib import Path

from sqlalchemy import func, or_, select, update

from .schema import upload_operations, upload_steps
from .uploads import UploadConflict, UploadJournal

logger = logging.getLogger(__name__)


class UploadCoordinator:
    """Own a journal; the runtime schedules recover() and calls close() on exit."""

    def __init__(self, state, *, workspace_root=None):
        self.journal = getattr(state, "uploads", None)
        if self.journal is None:
            self.journal = UploadJournal(state)
        self.workspace_root = Path(
            workspace_root if workspace_root is not None else tempfile.gettempdir()
        ).resolve()

    def close(self):
        """Stop this coordinator's journal lease renewal threads."""
        self.journal.close()

    def _remove_workspace(self, op):
        raw = op.get("workspace")
        if raw is None or op.get("workspace_node") != socket.gethostname():
            return False
        operation_id = op["id"]
        path = Path(raw)
        prefix = f"rag-txn-{operation_id}"
        if (not re.fullmatch(r"[0-9a-f]{32}", operation_id)
                or not path.is_absolute() or ".." in path.parts
                or path.parent != self.workspace_root
                or not (path.name == prefix or path.name.startswith(prefix + "-"))):
            raise ValueError("Unsafe upload workspace")
        try:
            info = path.lstat()
        except FileNotFoundError:
            return True
        if (stat.S_ISLNK(info.st_mode)
                or getattr(info, "st_file_attributes", 0)
                & getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0)
                or not stat.S_ISDIR(info.st_mode)
                or path.resolve().parent != self.workspace_root):
            raise ValueError("Unsafe upload workspace")
        try:
            shutil.rmtree(path)
        except FileNotFoundError:
            # Concurrent committed cleanup is idempotent.
            if path.exists():
                raise
        return True

    def cleanup_workspace(self, op):
        """Remove only this operation's registered local workspace; return bool.

        Active operations require the caller's live ownership. Remote or absent
        registrations return False. Invalid paths and filesystem errors raise;
        committed registrations remain discoverable by recover() for retry.
        Rolled-back registrations remain permanently to reap late parser effects.
        """
        current = self.journal.get(op["id"])
        if current["status"] not in ("COMMITTED", "ROLLED_BACK"):
            self.journal.assert_owner(op)
        removed = self._remove_workspace(current)
        if removed and current["status"] == "COMMITTED":
            self._clear_workspace(current)
        return removed

    def _clear_workspace(self, op):
        with self.journal.engine.begin() as conn:
            conn.execute(update(upload_operations).where(
                upload_operations.c.id == op["id"],
                upload_operations.c.status == "COMMITTED",
                upload_operations.c.workspace == op["workspace"],
                upload_operations.c.workspace_node == op["workspace_node"],
            ).values(workspace=None, workspace_node=None))

    @staticmethod
    def _has_qdrant_intent(op):
        return any(step["name"] == "stage_qdrant"
                   or re.match(r"^qdrant(?:$|[._:-])", step["name"], re.IGNORECASE)
                   for step in op["steps"])

    def _reap_rolled_back(self, op, store_factory):
        # Terminal journal state cannot publish again, but an already-issued
        # external write may finish late. Keep its version as a durable tombstone.
        current = self.journal.get(op["id"])
        if current["status"] != "ROLLED_BACK" or not self._has_qdrant_intent(current):
            return
        if self.journal.head(current["namespace"], current["document_id"]) == current["id"]:
            raise UploadConflict("Cannot reap the active version")
        store_factory(current).delete_version(current["document_id"], current["version"])

    def _rollback(self, op, store_factory):
        def cleanup(current):
            # Read durable steps, not the possibly stale caller's operation dict.
            if self._has_qdrant_intent(self.journal.get(current["id"])):
                store_factory(current).delete_version(
                    current["document_id"], current["version"])
            self._remove_workspace(current)

        result = self.journal.rollback(op, cleanup)
        try:
            self.cleanup_workspace(result)
        except Exception as exc:
            logger.warning("Upload %s workspace cleanup failed (%s); retry pending",
                           op["id"], type(exc).__name__)
        return self.journal.get(op["id"])

    def compensate(self, op, store):
        """Immediately roll back an owned unpublished upload; failures propagate.

        store.delete_version(document_id, version) must be idempotent. No vector
        deletion occurs if the journal contains no Qdrant intent.
        """
        return self._rollback(op, lambda _op: store)

    def _cleanup(self, op, store_factory):
        current = self.journal.get(op["id"])
        if current["status"] != "COMMITTED":
            raise UploadConflict("Committed cleanup requires COMMITTED")
        try:
            # Workspace cleanup is independent of vector cleanup eligibility.
            if self._remove_workspace(current):
                self._clear_workspace(current)

            def cleanup_old(committed):
                if committed["old_version"] is not None:
                    store_factory(committed).delete_version(
                        committed["document_id"], committed["old_version"])
                elif any(step["name"] == "stage_qdrant"
                         for step in self.journal.get(committed["id"])["steps"]):
                    store = store_factory(committed)
                    if hasattr(store, "delete_legacy"):
                        store.delete_legacy(committed["document_id"])

            return self.journal.cleanup(current, cleanup_old)
        except Exception as exc:
            # First-version workspace failures need a retry marker. A no-op
            # commit may retain an old_version that is still the head; don't
            # enable vector cleanup for it. Its registration remains retryable.
            with self.journal.engine.begin() as conn:
                conn.execute(update(upload_operations).where(
                    upload_operations.c.id == current["id"],
                    upload_operations.c.status == "COMMITTED",
                ).values(cleanup_pending=(current["cleanup_pending"]
                                          or current["old_version"] is None)))
            logger.warning("Upload %s cleanup failed (%s); retry pending",
                           current["id"], type(exc).__name__)
            return self.journal.get(current["id"])

    def cleanup(self, op, store):
        """Clean workspace and old_version, or unversioned legacy, after grace.

        First-version vector publications may call store.delete_legacy(doc),
        which must delete only points without upload_version. No-op commits
        have neither vector intent nor pending old-version cleanup.

        Cleanup errors are logged and retained as cleanup_pending, not raised.
        The journal's cleanup delay and namespace fencing remain authoritative.
        """
        return self._cleanup(op, lambda _op: store)

    def recover(self, store_factory, namespace=None):
        """Recover expired unfinished uploads; never steal a live lease.

        store_factory(op) resolves the operation's Qdrant collection. It is lazy:
        neither pre-vector failures nor workspace-only cleanup need a store.
        Reap ROLLED_BACK vector intents once per call, without an upload lease,
        to contain late external writes. Return resulting operation snapshots;
        isolate failures to each upload. The runtime supplies the retry schedule.
        """
        candidates = {op["id"]: op for op in self.journal.unfinished(namespace)}
        # First-version commits and remote rollback workspaces need local cleanup;
        # vector rollback tombstones remain eligible on every scheduled pass.
        step_name = func.lower(upload_steps.c.name)
        qdrant_intent = select(upload_steps.c.operation_id).where(
            upload_steps.c.operation_id == upload_operations.c.id,
            or_(upload_steps.c.name == "stage_qdrant", step_name == "qdrant",
                *(step_name.startswith("qdrant" + delimiter, autoescape=True)
                  for delimiter in "._:-")),
        ).exists()
        query = select(upload_operations.c.id).where(or_(
            (upload_operations.c.status.in_(("COMMITTED", "ROLLED_BACK"))
             & upload_operations.c.workspace.is_not(None)
             & (upload_operations.c.workspace_node == socket.gethostname())),
            (upload_operations.c.status == "ROLLED_BACK") & qdrant_intent,
        ))
        if namespace is not None:
            query = query.where(upload_operations.c.namespace == namespace)
        with self.journal.engine.connect() as conn:
            workspace_ids = list(conn.scalars(query))
        for operation_id in workspace_ids:
            if operation_id not in candidates:
                candidates[operation_id] = self.journal.get(operation_id)
        recovered = []
        for snapshot in candidates.values():
            try:
                # Refresh: another worker may have committed since enumeration.
                current = self.journal.get(snapshot["id"])
                if current["status"] == "COMMITTED":
                    recovered.append(self._cleanup(current, store_factory))
                elif current["status"] == "ROLLED_BACK":
                    self.cleanup_workspace(current)
                    self._reap_rolled_back(current, store_factory)
                    recovered.append(self.journal.get(current["id"]))
                else:
                    claimed = self.journal.claim_recovery(current["id"])
                    if claimed is not None:
                        recovered.append(self._rollback(claimed, store_factory))
            except Exception as exc:
                logger.warning("Upload %s recovery failed (%s); retry pending",
                               snapshot["id"], type(exc).__name__)
        return recovered
