"""SQLite-only orchestration tests; no Qdrant service or global temp sweep."""

import shutil
import socket
from pathlib import Path

import pytest
from sqlalchemy import select, update

from rag_mcp.state.schema import document_heads, document_leases, upload_operations
from rag_mcp.state.store import StateStores
from rag_mcp.state.upload_coordinator import UploadCoordinator
from rag_mcp.state.uploads import LeaseLost, UploadConflict


class FakeQdrant:
    def __init__(self):
        self.versions = set()
        self.deleted = []
        self.failures = 0

    def delete_version(self, document_id, version):
        self.deleted.append((document_id, version))
        if self.failures:
            self.failures -= 1
            raise OSError("controlled Qdrant outage")
        self.versions.discard((document_id, version))


@pytest.fixture()
def coordinator(tmp_path, monkeypatch):
    monkeypatch.delenv("RAG_STATE_URL", raising=False)
    monkeypatch.delenv("RAG_STATE_BACKEND", raising=False)
    monkeypatch.setenv("RAG_UPLOAD_CLEANUP_DELAY_SECONDS", "0")
    state = StateStores.create(tmp_path / "state")
    state.uploads.auto_heartbeat = False
    state.uploads.lease_seconds = 30
    coordinator = UploadCoordinator(state, workspace_root=tmp_path)
    assert coordinator.journal is state.uploads
    yield coordinator
    coordinator.close()
    state.close()


def begin(coordinator, *, namespace="ns", name="file.txt", document_id="doc"):
    return coordinator.journal.begin(namespace, name, document_id, "collection")


def expire(coordinator, op):
    with coordinator.journal.engine.begin() as conn:
        conn.execute(update(document_leases).where(
            document_leases.c.operation_id == op["id"]
        ).values(expires_at=0))


def commit(coordinator, op):
    with coordinator.journal.engine.begin() as conn:
        return coordinator.journal.commit(op, {"ok": True}, conn)


def workspace(coordinator, op, *, node=None):
    path = coordinator.workspace_root / f"rag-txn-{op['id']}"
    coordinator.journal.register_workspace(op, path, node=node)
    path.mkdir()
    (path / "source.txt").write_text("staged", encoding="utf-8")
    return path


def no_store(_op):
    pytest.fail("Qdrant factory must not be called")


def test_uses_shared_journal_and_closes_it(coordinator, monkeypatch):
    seen = []
    monkeypatch.setattr(coordinator.journal, "close", lambda: seen.append(True))
    coordinator.close()
    assert seen == [True]


def test_expired_recovery_is_version_scoped_and_fences_old_owner(coordinator):
    old = begin(coordinator)
    commit(coordinator, old)
    op = begin(coordinator)
    journal = coordinator.journal
    journal.intent(op, "stage_qdrant")  # Crash after intent, before DONE.
    journal.put_object(op, "source", "file.txt", b"staged")
    path = workspace(coordinator, op)
    unrelated = coordinator.workspace_root / "rag-txn-unregistered"
    unrelated.mkdir()
    store = FakeQdrant()
    store.versions.update(("doc", item) for item in (old["id"], op["id"], "future"))
    expire(coordinator, op)
    seen = []

    def factory(current):
        seen.append((current["collection"], current["namespace"]))
        return store

    assert coordinator.recover(factory)[0]["status"] == "ROLLED_BACK"
    assert seen == [("collection", "ns")]
    assert store.deleted == [("doc", op["id"])]
    assert store.versions == {("doc", old["id"]), ("doc", "future")}
    assert journal.head("ns", "doc") == old["id"]
    assert journal.get_object(op["id"], "source", "file.txt") is None
    assert not path.exists() and unrelated.exists()
    assert coordinator.recover(factory)[0]["status"] == "ROLLED_BACK"
    assert store.deleted == [("doc", op["id"])] * 2
    with pytest.raises(LeaseLost):
        coordinator.compensate(op, store)


@pytest.mark.parametrize("failed", [False, True])
def test_live_uploads_are_never_claimed(coordinator, failed):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    coordinator.journal.intent(op, "stage_qdrant")
    if failed:
        coordinator.journal.fail(op, ValueError("controlled"))
    assert coordinator.recover(no_store) == []
    assert path.exists()
    assert coordinator.journal.get(op["id"])["owner"] == op["owner"]


@pytest.mark.parametrize("step", [None, "receive_source", "embedding", "stage_mysql"])
def test_pre_vector_rollback_never_resolves_qdrant(coordinator, step):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    if step:
        coordinator.journal.intent(op, step)
    expire(coordinator, op)
    assert coordinator.recover(no_store)[0]["status"] == "ROLLED_BACK"
    assert not path.exists()


@pytest.mark.parametrize("step", ["stage_qdrant", "qdrant", "qdrant.upsert", "qdrant_write"])
def test_immediate_compensation_honors_uncertain_vector_intents(coordinator, step):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    coordinator.journal.intent(op, step)
    store = FakeQdrant()
    # Pass the original INTENT snapshot: durable journal steps are authoritative.
    result = coordinator.compensate(op, store)
    assert result["status"] == "ROLLED_BACK"
    assert store.deleted == [("doc", op["id"])]
    assert not path.exists()


def test_rollback_outage_retains_objects_and_retries_after_expiry(coordinator, caplog):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    journal = coordinator.journal
    journal.intent(op, "stage_qdrant")
    journal.put_object(op, "source", "file.txt", b"recoverable")
    store = FakeQdrant()
    store.failures = 1
    expire(coordinator, op)
    assert coordinator.recover(lambda _: store) == []
    assert journal.get(op["id"])["status"] == "ROLLBACK_FAILED"
    assert journal.get_object(op["id"], "source", "file.txt") == b"recoverable"
    assert path.exists()
    assert "retry pending" in caplog.text
    assert coordinator.recover(no_store) == []  # Failed rollback still has a lease.
    expire(coordinator, op)
    assert coordinator.recover(lambda _: store)[0]["status"] == "ROLLED_BACK"
    assert store.deleted == [("doc", op["id"])] * 2
    assert not path.exists()


def test_workspace_failure_preserves_rollback_retry(coordinator, monkeypatch):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    coordinator.journal.intent(op, "stage_qdrant")
    coordinator.journal.put_object(op, "source", "file.txt", b"staged")
    store = FakeQdrant()

    def fail(_path):
        raise PermissionError("controlled file lock")

    with monkeypatch.context() as patch:
        patch.setattr("rag_mcp.state.upload_coordinator.shutil.rmtree", fail)
        with pytest.raises(PermissionError):
            coordinator.compensate(op, store)
    assert coordinator.journal.get(op["id"])["status"] == "ROLLBACK_FAILED"
    assert coordinator.journal.get_object(op["id"], "source", "file.txt") == b"staged"
    assert path.exists()
    expire(coordinator, op)
    assert coordinator.recover(lambda _: store)[0]["status"] == "ROLLED_BACK"
    assert store.deleted == [("doc", op["id"])] * 2
    assert not path.exists()


def test_factory_outage_is_recorded_as_rollback_failure(coordinator):
    op = begin(coordinator)
    coordinator.journal.intent(op, "stage_qdrant")
    expire(coordinator, op)

    def outage(_op):
        raise OSError("factory unavailable")

    coordinator.recover(outage)
    assert coordinator.journal.get(op["id"])["status"] == "ROLLBACK_FAILED"


def test_one_failed_recovery_does_not_block_other_operations(coordinator):
    first = begin(coordinator)
    second = begin(coordinator, document_id="other", name="other.txt")
    coordinator.journal.intent(first, "stage_qdrant")
    expire(coordinator, first)
    expire(coordinator, second)
    store = FakeQdrant()
    store.failures = 1
    coordinator.recover(lambda _: store)
    assert coordinator.journal.get(first["id"])["status"] == "ROLLBACK_FAILED"
    assert coordinator.journal.get(second["id"])["status"] == "ROLLED_BACK"


def test_committed_cleanup_only_deletes_its_old_version_after_newer_commit(coordinator):
    first = begin(coordinator)
    commit(coordinator, first)
    second = begin(coordinator)
    second_path = workspace(coordinator, second)
    commit(coordinator, second)
    third = begin(coordinator)
    commit(coordinator, third)
    store = FakeQdrant()
    store.versions.update(("doc", op["id"]) for op in (first, second, third))
    # Only run second's delayed cleanup: never broad "remove old versions".
    result = coordinator.cleanup(second, store)
    assert not result["cleanup_pending"]
    assert store.deleted == [("doc", first["id"])]
    assert store.versions == {("doc", second["id"]), ("doc", third["id"])}
    assert coordinator.journal.head("ns", "doc") == third["id"]
    assert not second_path.exists()
    with pytest.raises(LeaseLost):
        coordinator.compensate(second, store)
    coordinator.recover(lambda _: store)
    assert store.deleted == [("doc", first["id"]), ("doc", second["id"])]
    assert store.versions == {("doc", third["id"])}


def test_committed_cleanup_failure_stays_pending_without_rollback(coordinator, caplog):
    first = begin(coordinator)
    commit(coordinator, first)
    second = begin(coordinator)
    path = workspace(coordinator, second)
    commit(coordinator, second)
    store = FakeQdrant()
    store.failures = 1
    result = coordinator.cleanup(second, store)
    assert result["status"] == "COMMITTED" and result["cleanup_pending"]
    assert not path.exists()
    assert "cleanup failed (OSError)" in caplog.text
    coordinator.recover(lambda _: store)
    assert not coordinator.journal.get(second["id"])["cleanup_pending"]
    assert store.deleted == [("doc", first["id"])] * 2
    assert coordinator.journal.head("ns", "doc") == second["id"]


def test_cleanup_grace_defers_vectors_but_not_workspace(coordinator):
    first = begin(coordinator)
    commit(coordinator, first)
    second = begin(coordinator)
    path = workspace(coordinator, second)
    commit(coordinator, second)
    coordinator.journal.cleanup_delay_seconds = 3600
    result = coordinator.recover(no_store)[0]
    assert result["cleanup_pending"] and result["status"] == "COMMITTED"
    assert not path.exists()


def test_first_commit_workspace_is_recovered_without_old_version(coordinator):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    committed = commit(coordinator, op)
    assert not committed["cleanup_pending"]
    assert coordinator.journal.unfinished() == []
    result = coordinator.recover(no_store)[0]
    assert result["status"] == "COMMITTED"
    assert result["workspace"] is None and not path.exists()
    assert coordinator.recover(no_store) == []


def test_first_commit_workspace_failure_is_retryable(coordinator, monkeypatch):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    commit(coordinator, op)
    remove = shutil.rmtree

    def fail(_path):
        raise PermissionError("controlled file lock")

    with monkeypatch.context() as patch:
        patch.setattr("rag_mcp.state.upload_coordinator.shutil.rmtree", fail)
        result = coordinator.cleanup(op, None)
    assert result["status"] == "COMMITTED" and result["cleanup_pending"]
    assert path.exists()
    assert shutil.rmtree is remove
    result = coordinator.recover(no_store)[0]
    assert not result["cleanup_pending"] and not path.exists()


def test_workspace_cleanup_never_removes_remote_node_directory(coordinator):
    op = begin(coordinator)
    path = workspace(coordinator, op, node=socket.gethostname() + "-remote")
    expire(coordinator, op)
    assert coordinator.recover(no_store)[0]["status"] == "ROLLED_BACK"
    assert path.exists()


@pytest.mark.parametrize("kind", ["unrelated", "other_id", "nested", "relative", "traversal"])
def test_unsafe_registered_workspace_is_not_deleted(coordinator, kind):
    op = begin(coordinator)
    root = coordinator.workspace_root
    if kind == "unrelated":
        path = root / "not-a-transaction"
    elif kind == "other_id":
        path = root / ("rag-txn-" + "0" * 32)
    elif kind == "nested":
        path = root / "nested" / f"rag-txn-{op['id']}"
    elif kind == "relative":
        path = Path(f"rag-txn-{op['id']}")
    else:
        (root / "nested").mkdir()
        path = root / "nested" / ".." / f"rag-txn-{op['id']}"
    if path.is_absolute():
        path.mkdir(parents=True, exist_ok=True)
    coordinator.journal.register_workspace(op, path)
    with pytest.raises(ValueError, match="Unsafe"):
        coordinator.compensate(op, None)
    assert coordinator.journal.get(op["id"])["status"] == "ROLLBACK_FAILED"
    if path.is_absolute():
        assert path.exists()


def test_remote_rollback_workspace_recovers_on_original_host(coordinator, monkeypatch):
    op = begin(coordinator)
    path = workspace(coordinator, op, node="original-upload-node")
    expire(coordinator, op)
    assert coordinator.recover(no_store)[0]["status"] == "ROLLED_BACK"
    assert coordinator.journal.get(op["id"])["workspace"] == str(path)
    assert path.exists()
    monkeypatch.setattr("rag_mcp.state.upload_coordinator.socket.gethostname",
                        lambda: "original-upload-node")
    result = coordinator.recover(no_store)[0]
    assert result["status"] == "ROLLED_BACK" and result["workspace"] == str(path)
    assert not path.exists()
    path.mkdir()  # A parser on the original node completes after rollback.
    (path / "late.txt").write_text("late", encoding="utf-8")
    assert coordinator.recover(no_store)[0]["workspace"] == str(path)
    assert not path.exists()


def test_missing_registered_workspace_is_idempotent(coordinator):
    op = begin(coordinator)
    path = coordinator.workspace_root / f"rag-txn-{op['id']}-parse"
    coordinator.journal.register_workspace(op, path)
    assert coordinator.compensate(op, None)["status"] == "ROLLED_BACK"
    assert not path.exists()


def test_namespace_filter_applies_to_unfinished_and_committed_workspaces(coordinator):
    left = begin(coordinator, namespace="left")
    left_path = workspace(coordinator, left)
    commit(coordinator, left)
    right = begin(coordinator, namespace="right")
    right_path = workspace(coordinator, right)
    expire(coordinator, right)
    coordinator.recover(no_store, namespace="right")
    assert left_path.exists() and not right_path.exists()
    assert coordinator.journal.get(left["id"])["status"] == "COMMITTED"


def test_restart_uses_persisted_journal_not_in_memory_snapshots(coordinator):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    coordinator.journal.intent(op, "stage_qdrant")
    expire(coordinator, op)
    coordinator.close()
    state = StateStores.create(coordinator.workspace_root / "state")
    restarted = UploadCoordinator(state, workspace_root=coordinator.workspace_root)
    try:
        store = FakeQdrant()
        assert restarted.recover(lambda _: store)[0]["status"] == "ROLLED_BACK"
        assert store.deleted == [("doc", op["id"])]
        assert not path.exists()
    finally:
        restarted.close()
        state.close()


def test_cleanup_workspace_uses_registered_path_not_caller_override(coordinator):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    unrelated = coordinator.workspace_root / "unrelated"
    unrelated.mkdir()
    supplied = dict(op, workspace=str(unrelated))
    assert coordinator.cleanup_workspace(supplied) is True
    assert not path.exists() and unrelated.exists()
    assert coordinator.cleanup_workspace(supplied) is True


def test_cleanup_workspace_rejects_stale_live_owner(coordinator):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    expire(coordinator, op)
    coordinator.journal.claim_recovery(op["id"])
    with pytest.raises(LeaseLost):
        coordinator.cleanup_workspace(op)
    assert path.exists()


def test_cleanup_workspace_clears_committed_registration(coordinator):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    commit(coordinator, op)
    assert coordinator.cleanup_workspace(op) is True
    assert not path.exists()
    assert coordinator.journal.get(op["id"])["workspace"] is None
    assert coordinator.cleanup_workspace(op) is False


def test_symlink_workspace_never_removes_target(coordinator):
    op = begin(coordinator)
    target = coordinator.workspace_root / "outside"
    target.mkdir()
    (target / "keep.txt").write_text("keep", encoding="utf-8")
    path = coordinator.workspace_root / f"rag-txn-{op['id']}"
    try:
        path.symlink_to(target, target_is_directory=True)
    except OSError:
        pytest.skip("Directory symlinks require privileges on this platform")
    coordinator.journal.register_workspace(op, path)
    with pytest.raises(ValueError, match="Unsafe"):
        coordinator.compensate(op, None)
    assert (target / "keep.txt").read_text(encoding="utf-8") == "keep"
    path.unlink()


def test_failed_qdrant_step_still_requires_version_deletion(coordinator):
    op = begin(coordinator)

    def fail():
        raise OSError("partial write")

    with pytest.raises(OSError):
        coordinator.journal.run_step(op, "stage_qdrant", fail)
    store = FakeQdrant()
    assert coordinator.compensate(op, store)["status"] == "ROLLED_BACK"
    assert store.deleted == [("doc", op["id"])]


def test_noop_commit_never_deletes_existing_head_even_after_workspace_failure(
        coordinator, monkeypatch):
    first = begin(coordinator)
    commit(coordinator, first)
    noop = begin(coordinator)
    path = workspace(coordinator, noop)
    coordinator.journal.commit_noop(noop, {"deduplicated": True})

    def fail(_path):
        raise PermissionError("controlled file lock")

    with monkeypatch.context() as patch:
        patch.setattr("rag_mcp.state.upload_coordinator.shutil.rmtree", fail)
        coordinator.recover(no_store)
    assert path.exists()
    assert not coordinator.journal.get(noop["id"])["cleanup_pending"]
    coordinator.recover(no_store)
    assert not path.exists()
    assert coordinator.journal.head("ns", "doc") == first["id"]


@pytest.mark.parametrize("step", ["stage_qdrant", "qdrant.upsert"])
def test_tombstone_reaper_deletes_late_vectors_without_upload_lease(coordinator, step):
    op = begin(coordinator)
    coordinator.journal.intent(op, step)
    path = workspace(coordinator, op)
    store = FakeQdrant()
    expire(coordinator, op)
    coordinator.recover(lambda _: store)
    assert not path.exists()
    assert coordinator.journal.get(op["id"])["workspace"] == str(path)
    newer = begin(coordinator)
    commit(coordinator, newer)
    # An old in-flight request completes after rollback and its worker dies.
    store.versions.update({("doc", op["id"]), ("doc", newer["id"]), ("other", op["id"])})
    seen = []

    def factory(current):
        seen.append((current["namespace"], current["collection"],
                     current["document_id"], current["version"]))
        return store

    coordinator.recover(factory)
    assert seen == [("ns", "collection", "doc", op["id"])]
    assert store.versions == {("doc", newer["id"]), ("other", op["id"])}
    assert coordinator.journal.head("ns", "doc") == newer["id"]
    assert coordinator.journal.get(op["id"])["status"] == "ROLLED_BACK"
    # Every scheduled pass is one bounded retry; no terminal lease is needed.
    coordinator.recover(factory)
    assert store.deleted == [("doc", op["id"])] * 3
    with coordinator.journal.engine.connect() as conn:
        assert conn.execute(select(document_leases).where(
            document_leases.c.operation_id == op["id"])).first() is None


def test_tombstone_reaper_never_deletes_active_head(coordinator, caplog):
    op = begin(coordinator)
    coordinator.journal.intent(op, "stage_qdrant")
    store = FakeQdrant()
    coordinator.compensate(op, store)
    # Defensive corruption/inconsistent-state guard: terminal must not imply safe.
    with coordinator.journal.engine.begin() as conn:
        conn.execute(document_heads.insert().values(
            namespace="ns", document_id="doc", version=op["id"], updated_at=0))
    assert coordinator.recover(no_store) == []
    assert "recovery failed (UploadConflict)" in caplog.text
    assert store.deleted == [("doc", op["id"])]


def test_tombstone_reaper_filters_namespace_and_retries_outage(coordinator, caplog):
    left = begin(coordinator, namespace="left")
    right = begin(coordinator, namespace="right")
    store = FakeQdrant()
    for op in (left, right):
        coordinator.journal.intent(op, "stage_qdrant")
        coordinator.compensate(op, store)
    store.deleted.clear()
    store.versions.update({("doc", left["id"]), ("doc", right["id"])})
    store.failures = 1
    assert coordinator.recover(lambda _: store, namespace="left") == []
    assert "retry pending" in caplog.text
    assert coordinator.journal.get(left["id"])["status"] == "ROLLED_BACK"
    coordinator.recover(lambda _: store, namespace="left")
    assert store.deleted == [("doc", left["id"])] * 2
    assert store.versions == {("doc", right["id"])}


def test_non_vector_tombstone_reaps_late_workspace_without_resolving_qdrant(coordinator):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    coordinator.compensate(op, None)
    assert coordinator.journal.get(op["id"])["workspace"] == str(path)
    assert not path.exists()
    unrelated = coordinator.workspace_root / "unrelated-parser"
    unrelated.mkdir()
    for _ in range(2):
        path.mkdir()
        (path / "late.txt").write_text("late parser result", encoding="utf-8")
        assert coordinator.recover(no_store)[0]["workspace"] == str(path)
        assert not path.exists() and unrelated.exists()
    assert coordinator.recover(no_store)[0]["status"] == "ROLLED_BACK"


class LegacyQdrant(FakeQdrant):
    def __init__(self):
        super().__init__()
        self.legacy_deleted = []

    def delete_legacy(self, document_id):
        self.legacy_deleted.append(document_id)
        if self.failures:
            self.failures -= 1
            raise OSError("controlled legacy cleanup outage")


def pending_cleanup(coordinator, op):
    # Exercise the callback independently of the journal's publication policy.
    with coordinator.journal.engine.begin() as conn:
        conn.execute(update(upload_operations).where(
            upload_operations.c.id == op["id"]).values(cleanup_pending=True))


@pytest.mark.parametrize("supports_legacy", [True, False])
def test_first_vector_commit_only_cleans_unversioned_legacy(coordinator, supports_legacy):
    op = begin(coordinator)
    coordinator.journal.intent(op, "stage_qdrant")
    commit(coordinator, op)
    pending_cleanup(coordinator, op)
    store = LegacyQdrant() if supports_legacy else FakeQdrant()
    store.versions.update({("doc", op["id"]), ("doc", "future")})
    result = coordinator.cleanup(op, store)
    assert not result["cleanup_pending"]
    assert store.deleted == []
    assert store.versions == {("doc", op["id"]), ("doc", "future")}
    if supports_legacy:
        assert store.legacy_deleted == ["doc"]
    assert coordinator.journal.head("ns", "doc") == op["id"]


def test_legacy_cleanup_failure_retries_after_publication(coordinator, caplog):
    op = begin(coordinator)
    coordinator.journal.intent(op, "stage_qdrant")
    commit(coordinator, op)
    pending_cleanup(coordinator, op)
    store = LegacyQdrant()
    store.failures = 1
    result = coordinator.cleanup(op, store)
    assert result["status"] == "COMMITTED" and result["cleanup_pending"]
    assert "cleanup failed (OSError)" in caplog.text
    coordinator.recover(lambda _: store)
    assert store.legacy_deleted == ["doc", "doc"]
    assert store.deleted == []
    assert not coordinator.journal.get(op["id"])["cleanup_pending"]


def test_replacement_cleanup_never_calls_legacy_delete(coordinator):
    first = begin(coordinator)
    commit(coordinator, first)
    second = begin(coordinator)
    coordinator.journal.intent(second, "stage_qdrant")
    commit(coordinator, second)
    store = LegacyQdrant()
    coordinator.cleanup(second, store)
    assert store.deleted == [("doc", first["id"])]
    assert store.legacy_deleted == []


def test_noop_and_pre_vector_commit_never_clean_legacy(coordinator):
    noop = begin(coordinator)
    workspace(coordinator, noop)
    coordinator.journal.commit_noop(noop, {"deduplicated": True})
    store = LegacyQdrant()
    coordinator.cleanup(noop, store)
    assert not coordinator.journal.get(noop["id"])["cleanup_pending"]
    first = begin(coordinator)
    commit(coordinator, first)
    pending_cleanup(coordinator, first)  # E.g. a workspace-only cleanup retry.
    coordinator.cleanup(first, store)
    assert store.legacy_deleted == [] and store.deleted == []


def test_uncommitted_cleanup_rejected_without_mutation(coordinator):
    op = begin(coordinator)
    path = workspace(coordinator, op)
    with pytest.raises(UploadConflict, match="COMMITTED"):
        coordinator.cleanup(op, None)
    assert path.exists()
    with coordinator.journal.engine.connect() as conn:
        assert conn.scalar(select(upload_operations.c.cleanup_pending).where(
            upload_operations.c.id == op["id"])) is False
