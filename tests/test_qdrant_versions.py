"""Publication, rollback, and migration boundaries for upload Saga vectors."""

from types import SimpleNamespace

import pytest
from qdrant_client import QdrantClient

from rag_mcp.chunking import DocumentChunk
from rag_mcp.contracts import RagOperationError
from rag_mcp.sparse import SparseEmbedding
from rag_mcp.storage.qdrant_store import QdrantStore

DENSE = [1.0, 0.0]
SPARSE = SparseEmbedding((7,), (1.0,))


@pytest.fixture
def client():
    connection = QdrantClient(":memory:")
    yield connection
    connection.close()


def store(client, namespace="alpha", heads=None, collection="versions"):
    return QdrantStore(
        collection=collection, client=client, namespace=namespace, vector_size=2,
        active_versions=(lambda: dict(heads)) if heads is not None else None,
    )


def chunk(document_id, text="text", chunk_id=None):
    return DocumentChunk(document_id, chunk_id or f"chunk-{document_id}", text, "Title", "source", {})


def asset(document_id, text="asset", asset_id=None):
    return {"record_type": "asset", "document_id": document_id,
            "asset_id": asset_id or f"asset-{document_id}", "text": text}


def stage(index, document_id, version, text=None, *, chunk_id=None, asset_id=None):
    return index.stage_version(
        [chunk(document_id, text or version, chunk_id)], [DENSE], [SPARSE],
        [asset(document_id, text or version, asset_id)], version,
    )


def legacy(index, document_id, text="legacy"):
    index.replace_document([chunk(document_id, text)], [DENSE], [SPARSE], [asset(document_id, text)])


def records(client, collection="versions"):
    points, offset = client.scroll(collection, limit=1000, with_payload=True, with_vectors=True)
    assert offset is None
    return points


def search(index, mode, filters=None, limit=100):
    kwargs = {"limit": limit, "filters": filters or {}}
    if mode == "hybrid":
        return index.hybrid_search(DENSE, SPARSE, **kwargs)
    if mode == "hybrid-empty":
        return index.hybrid_search(DENSE, SparseEmbedding((), ()), **kwargs)
    if mode == "sparse":
        return index.sparse_search(SPARSE, **kwargs)
    return getattr(index, mode)(DENSE, **kwargs)


@pytest.mark.parametrize("mode", ["dense_search", "search", "sparse", "hybrid", "hybrid-empty"])
def test_queries_follow_heads_case_sensitively_and_fail_closed(client, mode):
    heads = {}
    index = store(client, heads=heads)
    legacy(index, "Doc")
    legacy(index, "doc")
    legacy(index, "deleted")
    stage(index, "Doc", "old")
    stage(index, "Doc", "new")
    stage(index, "doc", "unpublished")
    stage(index, "deleted", "staged")
    stage(index, "fresh", "unpublished")
    legacy(store(client, "beta"), "foreign")
    stage(store(client, "beta"), "Doc", "new", "foreign")
    heads.update({"Doc": "old", "deleted": ""})

    hits = search(index, mode)
    assert {(p.payload["document_id"], p.payload["text"]) for p in hits} == {
        ("Doc", "old"), ("doc", "legacy"),
    }
    assert [p.payload["text"] for p in search(index, mode, {"document_id": "Doc"}, limit=1)] == ["old"]
    assert not search(index, mode, {"text": "new"})
    heads["Doc"] = "new"
    assert [p.payload["text"] for p in search(index, mode, {"document_id": "Doc"})] == ["new"]
    heads["Doc"] = ""
    assert not search(index, mode, {"document_id": "Doc"})


@pytest.mark.parametrize("namespace", [None, "alpha", "beta"])
@pytest.mark.parametrize("mode", ["dense_search", "sparse", "hybrid"])
def test_namespace_boundary_also_applies_without_provider(client, namespace, mode):
    for scope in (None, "alpha", "beta"):
        index = store(client, scope)
        legacy(index, "Doc", f"legacy-{scope}")
        stage(index, "Doc", "unpublished")
    index = store(client, namespace)
    assert [p.payload["text"] for p in search(index, mode)] == [f"legacy-{namespace}"]
    assert index.get_assets(["asset-Doc"])["asset-Doc"]["text"] == f"legacy-{namespace}"


def test_assets_resolve_active_point_ids_and_do_not_guess_documents(client):
    heads = {}
    index = store(client, heads=heads)
    for document_id in ("Doc", "doc", "deleted"):
        legacy(index, document_id)
        stage(index, document_id, "new")
    # The same version on a different document must not publish this asset.
    stage(index, "hidden", "new", asset_id="unrelated-asset")
    stage(index, "Doc", "staged", asset_id="only-staged")
    stage(store(client, "beta"), "Doc", "new", "foreign")
    heads.update({"Doc": "new", "deleted": ""})
    ids = ["asset-Doc", "asset-doc", "asset-deleted", "unrelated-asset", "only-staged", "missing", "asset-Doc"]
    found = index.get_assets(ids)
    assert {key: value["text"] for key, value in found.items()} == {
        "asset-Doc": "new", "asset-doc": "legacy",
    }
    heads["Doc"] = "staged"
    assert index.get_assets(["asset-Doc", "only-staged"])["only-staged"]["upload_version"] == "staged"
    heads["Doc"] = ""
    assert not index.get_assets(["asset-Doc", "only-staged"])
    assert index.get_assets([]) == {}


def test_staging_is_idempotent_and_ids_include_document_version_and_namespace(client):
    index = store(client, heads={"Doc": "old"})
    for version in ("old", "new", "new"):
        assert stage(index, "Doc", version, chunk_id="shared", asset_id="shared") == 1
    for document_id in ("doc", "Doc"):
        stage(store(client, "beta"), document_id, "new", chunk_id="shared", asset_id="shared")
    stage(index, "doc", "new", chunk_id="shared", asset_id="shared")
    assert len(records(client)) == 10
    assert [p.payload["text"] for p in search(index, "search")] == ["old"]


@pytest.mark.parametrize("namespace", [None, "alpha"])
def test_cleanup_and_rollback_only_delete_scoped_document_versions(client, namespace):
    heads = {"Doc": "keep"}
    index = store(client, namespace, heads)
    for scope in (None, "alpha", "beta"):
        other = store(client, scope)
        for document_id in ("Doc", "doc"):
            legacy(other, document_id)
            stage(other, document_id, "old")
            stage(other, document_id, "keep")
    original = records(client)
    index.cleanup_versions("Doc", "keep")
    index.cleanup_versions("Doc", "keep")
    expected = [p.id for p in original if not (
        p.payload.get("namespace") == namespace and p.payload["document_id"] == "Doc"
        and p.payload.get("upload_version") != "keep"
    )]
    assert {p.id for p in records(client)} == set(expected)
    index.delete_version("Doc", "keep")
    index.delete_version("Doc", "keep")
    assert not search(index, "search", {"document_id": "Doc"})
    assert all(not (p.payload.get("namespace") == namespace and p.payload["document_id"] == "Doc")
               for p in records(client))


@pytest.mark.parametrize("namespace", [None, "alpha"])
def test_delete_legacy_preserves_versions_other_documents_and_namespaces(client, namespace):
    index = store(client, namespace, heads={"Doc": "published"})
    for scope in (None, "alpha", "beta"):
        other = store(client, scope)
        for document_id in ("Doc", "doc"):
            legacy(other, document_id)
            stage(other, document_id, "published")
            stage(other, document_id, "staged")
    original = records(client)
    index.delete_legacy("Doc")
    index.delete_legacy("Doc")
    expected = {p.id: p.payload for p in original if not (
        p.payload.get("namespace") == namespace and p.payload["document_id"] == "Doc"
        and "upload_version" not in p.payload
    )}
    assert {p.id: p.payload for p in records(client)} == expected
    assert [p.payload["text"] for p in search(index, "search", {"document_id": "Doc"})] == ["published"]
    assert index.get_assets(["asset-Doc"])["asset-Doc"]["upload_version"] == "published"


def test_delete_legacy_rejects_empty_document_id(client):
    index = store(client)
    with pytest.raises(ValueError, match="document_id must not be empty"):
        index.delete_legacy(" ")
    assert not client.collection_exists("versions")


def test_rollback_and_cleanup_do_not_create_a_missing_collection(client):
    index = store(client)
    index.delete_legacy("Doc")
    index.delete_legacy("Doc")
    index.delete_version("Doc", "missing")
    index.cleanup_versions("Doc", "missing")
    index.remove_old_versions("Doc", "missing")
    assert not client.collection_exists("versions")


def test_partial_staging_failure_never_replaces_published_points(client, monkeypatch):
    heads = {"Doc": "old"}
    index = store(client, heads=heads)
    stage(index, "Doc", "old")
    upsert = client.upsert

    def fail_after_first_point(*, collection_name, points, wait):
        upsert(collection_name=collection_name, points=points[:1], wait=wait)
        raise RuntimeError("partial staging failure")

    monkeypatch.setattr(client, "upsert", fail_after_first_point)
    with pytest.raises(RuntimeError, match="partial staging"):
        stage(index, "Doc", "new")
    for mode in ("dense_search", "sparse", "hybrid"):
        assert [p.payload["text"] for p in search(index, mode)] == ["old"]
    assert index.get_assets(["asset-Doc"])["asset-Doc"]["text"] == "old"
    index.delete_version("Doc", "new")
    assert len(records(client)) == 2


class Embedder:
    connection = SimpleNamespace(vector_size=3)

    def __init__(self):
        self.texts = []

    def embed_documents(self, texts):
        assert texts
        self.texts.extend(texts)
        return [[0.0, 1.0, 0.0] for _ in texts]


@pytest.mark.parametrize("namespace", [None, "alpha"])
def test_reembed_copies_only_visible_points_preserving_ids_and_versions(client, namespace):
    heads = {"Doc": "active", "deleted": ""}
    index = store(client, namespace, heads)
    for document_id in ("Doc", "doc", "deleted"):
        legacy(index, document_id, f"legacy-{document_id}")
        stage(index, document_id, "active")
        stage(index, document_id, "staged")
    foreign = "beta" if namespace is None else None
    legacy(store(client, foreign), "foreign")
    stage(store(client, foreign), "Doc", "active")
    source = records(client)
    embedder = Embedder()
    index.reembed_into("target", embedder, {"Doc": 1, "doc": 1})
    copied = records(client, "target")
    expected = [p for p in source if p.payload.get("namespace") == namespace and (
        (p.payload["document_id"] == "Doc" and p.payload.get("upload_version") == "active")
        or (p.payload["document_id"] == "doc" and "upload_version" not in p.payload)
    )]
    assert {p.id: p.payload for p in copied} == {p.id: p.payload for p in expected}
    assert sorted(embedder.texts) == ["active", "legacy-doc"]
    for point in copied:
        original = next(p for p in expected if p.id == point.id)
        if point.payload["record_type"] == "chunk":
            assert point.vector["dense"] == [0.0, 1.0, 0.0]
            assert point.vector["sparse"] == original.vector["sparse"]
        else:
            assert point.vector == original.vector
    assert {p.id: p.payload for p in records(client)} == {p.id: p.payload for p in source}
    with pytest.raises(RagOperationError, match="片段数量"):
        index.reembed_into("wrong-count", embedder, {"Doc": 2, "doc": 1})
