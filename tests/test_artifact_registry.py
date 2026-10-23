"""Binary artifact snapshots stay durable, bounded, and namespace scoped."""

import base64
import json
import shutil
import threading
import uuid
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from pathlib import Path

import pytest
from sqlalchemy import event, func, select
from sqlalchemy.dialects import mysql

from rag_mcp.state.artifacts import ArtifactRegistry, ArtifactUnavailable
from rag_mcp.state.schema import artifact_snapshots
from rag_mcp.state.settings import StateSettings
from rag_mcp.state.store import StateStores


@pytest.fixture()
def state():
    root = Path.cwd() / f".artifact-registry-tests-{uuid.uuid4().hex}"
    root.mkdir()
    stores = StateStores(StateSettings(url="sqlite+pysqlite:///" + (root / "state.sqlite3").as_posix()))
    try:
        yield stores
    finally:
        stores.close()
        shutil.rmtree(root)


def register(registry, **overrides):
    values = {"namespace": "alpha", "document_id": "doc-a", "version": "v1", "kind": "source",
                  "name": "report.pdf", "mime_type": "application/pdf", "content": b"%PDF-1.7\x00\xff"}
    values.update(overrides)
    return registry.register(**values)


def test_stable_uuid_and_redacted_metadata(state):
    registry = ArtifactRegistry(state, clock=lambda: 100)
    first = register(registry)
    assert str(uuid.UUID(first["artifact_id"])) == first["artifact_id"]
    assert uuid.UUID(first["artifact_id"]).version == 5
    assert first == register(ArtifactRegistry(state, clock=lambda: 100))
    assert first["resource_uri"] == f"rag://artifacts/alpha/{first['artifact_id']}"
    assert first["expires_at"] == 3700
    assert not {"content", "identity", "namespace", "created_at"} & first.keys()
    assert "PDF-1.7" not in json.dumps(first)
    assert registry.read("alpha", first["artifact_id"])["content"] == b"%PDF-1.7\x00\xff"
    with state.engine.connect() as conn:
        assert conn.scalar(select(func.count()).select_from(artifact_snapshots)) == 1


@pytest.mark.parametrize("change", [{"namespace": "Alpha"}, {"document_id": "doc-b"},
    {"version": "v2"}, {"version": None}, {"kind": "preview"}, {"name": "another.pdf"},
    {"content": b"other bytes"}])
def test_identity_tracks_snapshot_not_only_document(state, change):
    registry = ArtifactRegistry(state)
    assert register(registry)["artifact_id"] != register(registry, **change)["artifact_id"]


def test_namespace_missing_and_expired_are_indistinguishable(state):
    now = [100.0]
    registry = ArtifactRegistry(state, ttl_seconds=2, clock=lambda: now[0])
    artifact = register(registry)
    errors = []
    for namespace, artifact_id in [("other", artifact["artifact_id"]),
                                   ("Alpha", artifact["artifact_id"]), ("alpha", "not-a-uuid")]:
        with pytest.raises(ArtifactUnavailable) as error:
            registry.read(namespace, artifact_id)
        errors.append(str(error.value))
    now[0] = 102
    with pytest.raises(ArtifactUnavailable) as error:
        registry.read("alpha", artifact["artifact_id"])
    assert errors == [str(error.value)] * 3
    assert isinstance(error.value, ValueError)


@pytest.mark.parametrize("form", ["uppercase", "hex", "braces", "urn", "padding", "bytes", "object", "none"])
def test_noncanonical_uuid_rejected_before_database_access(state, monkeypatch, form):
    artifact_id = register(ArtifactRegistry(state))["artifact_id"]
    invalid = {
        "uppercase": artifact_id.upper(), "hex": artifact_id.replace("-", ""),
        "braces": "{" + artifact_id + "}", "urn": "urn:uuid:" + artifact_id,
        "padding": artifact_id + " ", "bytes": artifact_id.encode(),
        "object": uuid.UUID(artifact_id), "none": None,
    }[form]

    def forbidden_connect():
        raise AssertionError("Noncanonical IDs must not reach SQL")

    monkeypatch.setattr(state.engine, "connect", forbidden_connect)
    with pytest.raises(ArtifactUnavailable, match="^Artifact is unavailable$"):
        ArtifactRegistry(state).read("alpha", invalid)


def test_namespace_is_exact_even_if_database_collation_pads_spaces(state):
    registry = ArtifactRegistry(state)
    artifact = register(registry)

    def pad_space_comparison(conn, cursor, statement, parameters, context, executemany):
        if statement.startswith("SELECT artifact_snapshots."):
            parameters = tuple("alpha" if value == "alpha " else value for value in parameters)
        return statement, parameters

    event.listen(state.engine, "before_cursor_execute", pad_space_comparison, retval=True)
    try:
        with pytest.raises(ArtifactUnavailable):
            registry.read("alpha ", artifact["artifact_id"])
    finally:
        event.remove(state.engine, "before_cursor_execute", pad_space_comparison)


@pytest.mark.parametrize("namespace", [None, b"alpha", "", "x" * 65])
def test_invalid_read_namespace_is_unavailable(state, namespace):
    registry = ArtifactRegistry(state)
    artifact = register(registry)
    with pytest.raises(ArtifactUnavailable):
        registry.read(namespace, artifact["artifact_id"])


def test_renewal_is_monotonic_and_preserves_bytes_and_creation(state):
    registry = ArtifactRegistry(state, clock=lambda: 100)
    original = register(registry, expires_at=200)
    renewed = register(registry, expires_at=300, mime_type="application/octet-stream")
    shortened = register(registry, expires_at=150)
    assert original["artifact_id"] == renewed["artifact_id"] == shortened["artifact_id"]
    assert renewed["expires_at"] == shortened["expires_at"] == 300
    assert renewed["mime_type"] == original["mime_type"]
    with state.engine.connect() as conn:
        row = conn.execute(select(artifact_snapshots)).mappings().one()
    assert row["created_at"] == 100
    assert row["content"] == b"%PDF-1.7\x00\xff"
    assert row["mime_type"] == original["mime_type"]
    assert registry.read("alpha", original["artifact_id"])["content"] == row["content"]


@pytest.mark.parametrize("purge", [False, True])
def test_expired_identity_reregistration_restores_latest_reference(state, purge):
    now = [100.0]
    registry = ArtifactRegistry(state, clock=lambda: now[0], ttl_seconds=10)
    original = register(registry)
    now[0] = original["expires_at"]
    with pytest.raises(ArtifactUnavailable):
        registry.read("alpha", original["artifact_id"])
    if purge:
        assert registry.purge_expired() == 1
    renewed = register(registry)
    assert renewed["artifact_id"] == original["artifact_id"]
    assert renewed["expires_at"] == 120
    assert registry.read("alpha", original["artifact_id"])["content"] == b"%PDF-1.7\x00\xff"
    with state.engine.connect() as conn:
        assert conn.scalar(select(artifact_snapshots.c.created_at)) == (110 if purge else 100)


def test_survives_document_deletion_upload_cleanup_and_reopen(state):
    registry = ArtifactRegistry(state)
    state.documents.upsert("alpha", {"name": "report.pdf", "document_id": "doc-a", "collection": "c"})
    content = b"%PDF-1.7\x00\xff"
    state.blobs.put("alpha", "source", "report.pdf", content, document_id="doc-a")
    state.blobs.put("alpha", "pdf-assets", "doc-a/page.png", b"page", document_id="doc-a")
    artifact = register(registry, content=state.blobs.get("alpha", "source", "report.pdf"))
    state.documents.delete("alpha", "report.pdf")
    state.blobs.delete_document("alpha", "source", "doc-a")
    state.blobs.delete_document("alpha", "pdf-assets", "doc-a")
    another = StateStores(state.settings)
    try:
        assert ArtifactRegistry(another).read("alpha", artifact["artifact_id"])["content"] == b"%PDF-1.7\x00\xff"
    finally:
        another.close()


def test_live_document_replacement_and_blob_deletion_do_not_change_snapshot(state, monkeypatch):
    registry = ArtifactRegistry(state)
    original = b"%PDF-original\x00\xff"
    state.documents.upsert("alpha", {"name": "report.pdf", "document_id": "doc-a", "collection": "c"})
    state.blobs.put("alpha", "source", "report.pdf", original, document_id="doc-a")
    artifact = register(registry, content=state.blobs.get("alpha", "source", "report.pdf"))
    state.documents.upsert("alpha", {"name": "report.pdf", "document_id": "replacement", "collection": "c"})
    state.blobs.put("alpha", "source", "report.pdf", b"replacement", document_id="replacement")
    assert registry.read("alpha", artifact["artifact_id"])["content"] == original
    state.blobs.delete_key("alpha", "source", "report.pdf")
    assert state.blobs.get("alpha", "source", "report.pdf") is None

    def forbidden(*args, **kwargs):
        raise AssertionError("Artifact reads must never query documents or live blobs")

    monkeypatch.setattr(state, "documents", None)
    monkeypatch.setattr(state, "blobs", None)

    def only_artifact_queries(conn, cursor, statement, parameters, context, executemany):
        if "FROM documents" in statement or "FROM blobs" in statement:
            forbidden()

    event.listen(state.engine, "before_cursor_execute", only_artifact_queries)
    try:
        assert registry.read("alpha", artifact["artifact_id"])["content"] == original
    finally:
        event.remove(state.engine, "before_cursor_execute", only_artifact_queries)


def test_binary_chunks_are_json_safe_and_bounded(state):
    registry = ArtifactRegistry(state)
    content = bytes(range(256)) * 9000
    artifact = register(registry, content=content)
    chunks = []
    offset = 0
    while offset is not None:
        envelope = registry.chunk("alpha", artifact["artifact_id"], offset, 1024 * 1024)
        assert envelope["encoding"] == "base64"
        assert envelope["total_bytes"] == len(content)
        assert envelope["offset"] == offset
        assert envelope["name"] == artifact["name"]
        assert envelope["mime_type"] == artifact["mime_type"]
        assert envelope["expires_at"] == artifact["expires_at"]
        assert "content" not in envelope
        json.dumps(envelope)
        chunk = base64.b64decode(envelope["data"], validate=True)
        assert len(chunk) <= 1024 * 1024
        chunks.append(chunk)
        offset = envelope["next_offset"]
    assert b"".join(chunks) == content
    assert len(base64.b64decode(registry.chunk("alpha", artifact["artifact_id"])["data"])) == 65536
    envelope = registry.chunk("alpha", artifact["artifact_id"], len(content))
    assert envelope["data"] == "" and envelope["next_offset"] is None
    with pytest.raises(ValueError, match="offset"):
        registry.chunk("alpha", artifact["artifact_id"], len(content) + 1)


@pytest.mark.parametrize("offset,length", [(-1, 1), (True, 1), (1.0, 1), ("0", 1),
    (0, 0), (0, -1), (0, True), (0, 1.0), (0, 1048577)])
def test_invalid_chunk_ranges_rejected_before_lookup(state, offset, length):
    with pytest.raises(ValueError):
        ArtifactRegistry(state).chunk("alpha", "missing", offset, length)


def test_empty_binary_snapshot_and_uri_escaping(state):
    registry = ArtifactRegistry(state)
    artifact = register(registry, namespace="team/a b", version=None, content=b"")
    assert artifact["resource_uri"].startswith("rag://artifacts/team%2Fa%20b/")
    assert registry.chunk("team/a b", artifact["artifact_id"])["total_bytes"] == 0
    assert registry.chunk("team/a b", artifact["artifact_id"])["next_offset"] is None
    with pytest.raises(ValueError, match="offset"):
        registry.chunk("team/a b", artifact["artifact_id"], offset=1)


def test_size_policy_and_large_pdf_support(state):
    limited = ArtifactRegistry(state, max_bytes=3)
    with pytest.raises(ValueError, match="byte limit"):
        register(limited, content=b"four")
    assert register(limited, content=b"123")
    assert register(ArtifactRegistry(state, max_bytes=None), content=b"1234")
    assert ArtifactRegistry(state).max_bytes == 64 * 1024 * 1024
    assert register(ArtifactRegistry(state), content=b"%PDF" + b"x" * (17 * 1024 * 1024))


@pytest.mark.parametrize("options", [{"ttl_seconds": 0}, {"ttl_seconds": -1},
    {"ttl_seconds": float("nan")}, {"ttl_seconds": float("inf")},
    {"max_bytes": -1}, {"max_bytes": True}])
def test_invalid_registry_policy(state, options):
    with pytest.raises(ValueError):
        ArtifactRegistry(state, **options)


@pytest.mark.parametrize("change", [{"expires_at": float("nan")}, {"expires_at": float("inf")},
    {"content": "text"}, {"name": ""}, {"namespace": "x" * 65}, {"version": "x" * 65}])
def test_invalid_registration_does_not_write(state, change):
    with pytest.raises(ValueError):
        register(ArtifactRegistry(state), **change)
    with state.engine.connect() as conn:
        assert conn.scalar(select(func.count()).select_from(artifact_snapshots)) == 0


@pytest.mark.parametrize("bad_clock", [float("nan"), float("inf"), True, "100"])
def test_invalid_clock_cannot_bypass_expiry_or_purge_live_artifacts(state, bad_clock):
    registry = ArtifactRegistry(state, clock=lambda: 100)
    artifact = register(registry)
    registry.clock = lambda: bad_clock
    with pytest.raises(ValueError, match="clock"):
        registry.read("alpha", artifact["artifact_id"])
    with pytest.raises(ValueError, match="clock"):
        registry.purge_expired()
    assert ArtifactRegistry(state, clock=lambda: 100).read("alpha", artifact["artifact_id"])


def test_concurrent_duplicate_registration_extends_to_maximum(state):
    barrier = threading.Barrier(8)

    def worker(index):
        registry = ArtifactRegistry(state, clock=lambda: 100)
        barrier.wait(timeout=10)
        return register(registry, expires_at=200 + index, mime_type=f"application/worker-{index}")

    with ThreadPoolExecutor(max_workers=8) as pool:
        results = list(pool.map(worker, range(8)))
    assert len({result["artifact_id"] for result in results}) == 1
    assert len({result["mime_type"] for result in results}) == 1
    with state.engine.connect() as conn:
        assert conn.scalar(select(func.count()).select_from(artifact_snapshots)) == 1
        row = conn.execute(select(artifact_snapshots)).mappings().one()
    assert row["expires_at"] == 207
    assert row["content"] == b"%PDF-1.7\x00\xff"
    assert row["mime_type"] == results[0]["mime_type"]


@pytest.mark.parametrize("renew", [False, True])
@pytest.mark.parametrize("rollback", [False, True])
def test_sqlite_read_observes_only_committed_atomic_registration(state, renew, rollback):
    registry = ArtifactRegistry(state, clock=lambda: 100)
    original = register(registry, expires_at=1000)
    name = "report.pdf" if renew else "new.pdf"
    artifact_id = original["artifact_id"] if renew else register(registry, name=name, expires_at=100)["artifact_id"]
    written = threading.Event()
    release = threading.Event()

    def pause_after_write(conn, cursor, statement, parameters, context, executemany):
        if statement.startswith("INSERT INTO artifact_snapshots"):
            written.set()
            assert release.wait(timeout=10)
            if rollback:
                raise RuntimeError("controlled rollback")

    event.listen(state.engine, "after_cursor_execute", pause_after_write)
    try:
        with ThreadPoolExecutor(max_workers=1) as pool:
            future = pool.submit(register, registry, name=name, expires_at=2000)
            try:
                assert written.wait(timeout=10)
                if renew:
                    assert registry.read("alpha", artifact_id)["expires_at"] == 1000
                else:
                    with pytest.raises(ArtifactUnavailable):
                        registry.read("alpha", artifact_id)
            finally:
                release.set()
            if rollback:
                with pytest.raises(RuntimeError, match="controlled rollback"):
                    future.result(timeout=10)
            else:
                assert future.result(timeout=10)["expires_at"] == 2000
    finally:
        event.remove(state.engine, "after_cursor_execute", pause_after_write)
    if rollback and not renew:
        with pytest.raises(ArtifactUnavailable):
            registry.read("alpha", artifact_id)
    else:
        assert registry.read("alpha", artifact_id)["expires_at"] == (1000 if rollback else 2000)


def test_registry_does_not_log_content_or_lookup_identifiers(state, caplog):
    registry = ArtifactRegistry(state)
    secret = b"private-artifact-secret-\x00\xff"
    artifact = register(registry, content=secret)
    registry.read("alpha", artifact["artifact_id"])
    registry.chunk("alpha", artifact["artifact_id"])
    with pytest.raises(ArtifactUnavailable):
        registry.read("other", artifact["artifact_id"])
    assert "private-artifact-secret" not in caplog.text
    assert artifact["artifact_id"] not in caplog.text


def test_failed_renewal_rolls_back_and_can_be_retried(state):
    registry = ArtifactRegistry(state, clock=lambda: 100)
    artifact = register(registry, expires_at=200)

    def fail_after_write(conn, cursor, statement, parameters, context, executemany):
        if statement.startswith("INSERT INTO artifact_snapshots"):
            raise RuntimeError("controlled failure")

    event.listen(state.engine, "after_cursor_execute", fail_after_write)
    try:
        with pytest.raises(RuntimeError, match="controlled failure"):
            register(registry, expires_at=300)
    finally:
        event.remove(state.engine, "after_cursor_execute", fail_after_write)
    assert registry.read("alpha", artifact["artifact_id"])["expires_at"] == 200
    assert register(registry, expires_at=300)["artifact_id"] == artifact["artifact_id"]


def test_purge_is_bounded_and_expiry_is_inclusive(state):
    registry = ArtifactRegistry(state, clock=lambda: 100)
    for index in range(5):
        register(registry, name=f"{index}.pdf", expires_at=100)
    live = register(registry, name="live.pdf", expires_at=101)
    other = register(registry, namespace="other", name="live.pdf", expires_at=1000)
    assert registry.purge_expired(limit=2) == 2
    assert registry.purge_expired(limit=2) == 2
    assert registry.purge_expired() == 1
    assert registry.purge_expired() == 0
    assert registry.read("alpha", live["artifact_id"])
    assert registry.read("other", other["artifact_id"])
    for limit in [0, -1, True, 1.5]:
        with pytest.raises(ValueError):
            registry.purge_expired(limit)


def test_purge_does_not_delete_concurrently_renewed_candidate(state, monkeypatch):
    registry = ArtifactRegistry(state, clock=lambda: 100)
    artifact = register(registry, expires_at=100)
    original_begin = state.engine.begin
    renewed = False

    @contextmanager
    def begin_after_renewal():
        nonlocal renewed
        if not renewed:
            renewed = True
            with ThreadPoolExecutor(max_workers=1) as pool:
                pool.submit(register, registry, expires_at=500).result(timeout=10)
        with original_begin() as conn:
            yield conn

    monkeypatch.setattr(state.engine, "begin", begin_after_renewal)
    assert registry.purge_expired() == 0
    assert registry.read("alpha", artifact["artifact_id"])["expires_at"] == 500


def test_mysql_uses_native_atomic_greatest_upsert(state):
    captured = []
    registry = ArtifactRegistry(state, clock=lambda: 100)

    class Captured(Exception):
        pass

    class Connection:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def execute(self, statement):
            captured.append(str(statement.compile(dialect=mysql.dialect())))
            raise Captured

    class Engine:
        dialect = mysql.dialect()

        def begin(self):
            return Connection()

    class State:
        engine = Engine()

    registry.state = State()
    with pytest.raises(Captured):
        register(registry)
    assert "ON DUPLICATE KEY UPDATE" in captured[0]
    assert "greatest(artifact_snapshots.expires_at, VALUES(expires_at))" in captured[0]
    update = captured[0].split("ON DUPLICATE KEY UPDATE", 1)[1].strip()
    assert update == "expires_at = greatest(artifact_snapshots.expires_at, VALUES(expires_at))"
