"""Chunk ingestion, replacement, deletion and workspace clearing tests."""

from __future__ import annotations

from helpers import chunks_payload, ingest_document, query_payload, register_workspace


def test_ingest_reports_counts_and_skips_blank_chunks(client):
    register_workspace(client)
    body = ingest_document(client)
    assert body == {
        "document_id": "doc-1",
        "accepted": 2,
        "skipped": 1,
        "entities": 4,
        "relations": 6,
    }


def test_ingest_rejects_duplicate_chunk_ids(client):
    register_workspace(client)
    payload = chunks_payload(
        chunks=[
            {"chunk_id": "c-1", "text": "第一段", "page": 1},
            {"chunk_id": "c-1", "text": "第二段", "page": 2},
        ]
    )
    response = client.post("/v1/workspaces/legal/chunks", json=payload)
    assert response.status_code == 400
    assert "chunks" in response.json()["detail"]


def test_ingest_defaults_missing_page_and_heading(client):
    register_workspace(client)
    ingest_document(
        client,
        chunks=[{"chunk_id": "c-x", "text": "没有页码与标题的块"}],
    )
    response = client.post("/v1/workspaces/legal/query", json=query_payload())
    context = response.json()["contexts"][0]
    assert context["document_name"] == "民法典.pdf"
    assert context["page"] == 0
    assert context["heading"] == ""


def test_reingest_same_document_replaces_previous_chunks(client, fake_engine):
    register_workspace(client)
    ingest_document(client)
    body = ingest_document(
        client,
        chunks=[
            {
                "chunk_id": "c-9",
                "text": "替换后的文本",
                "page": 5,
                "heading": "第三编",
                "upload_version": 3,
            }
        ],
    )
    assert body["accepted"] == 1
    assert ("praxis_legal", "doc-1") in fake_engine.delete_calls
    assert list(fake_engine.documents["praxis_legal"]) == ["doc-1"]

    response = client.post("/v1/workspaces/legal/query", json=query_payload())
    contexts = response.json()["contexts"]
    assert [context["chunk_id"] for context in contexts] == ["c-9"]
    assert [context["text"] for context in contexts] == ["替换后的文本"]
    assert contexts[0]["page"] == 5
    assert contexts[0]["heading"] == "第三编"


def test_ingest_with_only_blank_chunks_accepts_nothing(client):
    register_workspace(client)
    body = ingest_document(client, chunks=[{"chunk_id": "e-1", "text": "   "}])
    assert body["accepted"] == 0
    assert body["skipped"] == 1
    assert body["entities"] == 0
    assert body["relations"] == 0

    response = client.post("/v1/workspaces/legal/query", json=query_payload())
    assert response.json()["contexts"] == []


def test_ingest_accepts_an_empty_chunk_list(client):
    register_workspace(client)
    body = ingest_document(client, chunks=[])
    assert body["accepted"] == 0
    assert body["skipped"] == 0


def test_delete_document_is_idempotent(client):
    register_workspace(client)
    ingest_document(client)

    deleted = client.delete("/v1/workspaces/legal/documents/doc-1")
    assert deleted.status_code == 200
    assert deleted.json() == {"document_id": "doc-1", "deleted": True}

    again = client.delete("/v1/workspaces/legal/documents/doc-1")
    assert again.status_code == 200
    assert again.json() == {"document_id": "doc-1", "deleted": False}

    response = client.post("/v1/workspaces/legal/query", json=query_payload())
    assert response.json()["contexts"] == []


def test_clear_workspace_forgets_everything(client, fake_engine):
    register_workspace(client)
    ingest_document(client)

    cleared = client.delete("/v1/workspaces/legal")
    assert cleared.status_code == 200
    assert cleared.json() == {"namespace": "legal", "cleared": True}
    assert fake_engine.cleared == ["praxis_legal"]
    assert client.get("/healthz").json()["workspaces"] == 0

    again = client.delete("/v1/workspaces/legal")
    assert again.status_code == 404


def test_ingest_rejects_invalid_document_id(client):
    register_workspace(client)
    payload = chunks_payload(document_id="bad id")
    response = client.post("/v1/workspaces/legal/chunks", json=payload)
    assert response.status_code == 400


def test_delete_rejects_invalid_path_document_id(client):
    register_workspace(client)
    response = client.delete("/v1/workspaces/legal/documents/bad%20id")
    assert response.status_code == 400
    assert "detail" in response.json()


def test_deferred_prune_is_fenced_by_registration(client):
    register_workspace(client)
    ingest_document(client)
    path = "/v1/workspaces/legal/documents/doc-1"
    old = client.get(path + "/revision").json()["revision"]
    ingest_document(client)
    page = client.get("/v1/workspaces/legal/inventory").json()["documents"]
    assert [row["document_id"] for row in page] == ["doc-1"]
    current = page[0]["revision"]
    assert current != old
    assert client.post(path + "/prune", params={"revision": old}).json()["outcome"] == "changed"
    assert client.post(path + "/prune", params={"revision": current}).json()["outcome"] == "deleted"
    assert client.get("/v1/workspaces/legal/inventory").json()["documents"] == []


def test_old_mirror_and_workspace_prune_reject_new_registration(client):
    register_workspace(client)
    path = "/v1/workspaces/legal"
    first = client.get(path + "/revision").json()["revision"]
    payload = chunks_payload()
    payload["expected_revision"] = ""
    assert client.post(path + "/chunks", json=payload).status_code == 200
    assert client.post(path + "/chunks", json=payload).status_code == 409
    assert client.post(path + "/prune", params={"revision": first}).json()["outcome"] == "changed"
    current = client.get(path + "/revision").json()["revision"]
    assert client.post(path + "/prune", params={"revision": current}).json()["outcome"] == "deleted"


def test_cleanup_checks_publication_even_with_current_registration(client):
    register_workspace(client)
    path = "/v1/workspaces/legal"
    body = chunks_payload()
    body["publication_version"] = "second"
    body["source_hash"] = "a" * 64
    assert client.post(path + "/chunks", json=body).status_code == 200
    doc = client.get(path + "/inventory").json()["documents"][0]
    assert (doc["publication_version"], doc["source_hash"]) == ("second", "a" * 64)
    response = client.post(path + "/documents/doc-1/prune",
                           params={"revision": doc["revision"], "publication": "first"})
    assert response.json()["outcome"] == "superseded"
    assert client.post(path + "/query", json=query_payload()).json()["contexts"][0]["publication_version"] == "second"
