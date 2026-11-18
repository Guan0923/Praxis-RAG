"""Shared test doubles and payload builders for the LightRAG sidecar suite.

The suite never imports ``lightrag-hku``: :class:`FakeEngine` implements the
:class:`app.engine.GraphEngine` port in memory, and the application under test is
built through ``create_app(settings, engine=fake)``. Chunk ids are derived with
the same helper the production adapter uses, so the Praxis-to-LightRAG mapping
exercised here matches the pinned upstream derivation.
"""

from __future__ import annotations

from typing import Any

from app.config import WorkspaceConfig
from app.engine import (
    DocumentInput,
    EngineChunk,
    EngineEntity,
    EngineQueryResult,
    EngineRelation,
    GraphEngine,
    InsertResult,
    custom_chunk_key,
)

#: Fingerprint used by the default workspace payload.
FINGERPRINT = "sha256:" + "a" * 64
#: Different fingerprint, used to exercise the rebuild path.
ALTERNATE_FINGERPRINT = "sha256:" + "b" * 64
#: Test bearer token used by the authenticated client fixture.
TOKEN = "test-token"


class FakeEngine(GraphEngine):
    """In-memory engine double; no network, no LightRAG import.

    Attributes:
        workspaces: Workspace name to binding, recording ``ensure_workspace``.
        documents: Workspace name to ``document_id`` to ingested document.
        cleared: Workspace names passed to ``clear_workspace``, in order.
        delete_calls: ``(workspace, document_id)`` pairs passed to delete.
        query_calls: Recorded query arguments.
        failure: Exception raised by every operation when set.
        query_result: When set, returned verbatim by ``query``.
        entities_per_chunk: Deterministic entity count multiplier.
        relations_per_chunk: Deterministic relationship count multiplier.
    """

    def __init__(self) -> None:
        """Create an empty engine double."""

        self.workspaces: dict[str, WorkspaceConfig] = {}
        self.documents: dict[str, dict[str, DocumentInput]] = {}
        self.cleared: list[str] = []
        self.delete_calls: list[tuple[str, str]] = []
        self.query_calls: list[dict[str, Any]] = []
        self.failure: Exception | None = None
        self.query_result: EngineQueryResult | None = None
        self.entities_per_chunk = 2
        self.relations_per_chunk = 3

    async def ensure_workspace(self, config: WorkspaceConfig) -> None:
        """Record the binding; idempotent like the production adapter."""

        self._maybe_fail()
        self.workspaces[config.workspace] = config

    async def insert_document(self, workspace: str, document: DocumentInput) -> InsertResult:
        """Store the document and derive chunk ids with the shared helper."""

        self._maybe_fail()
        self.documents.setdefault(workspace, {})[document.document_id] = document
        chunk_keys: dict[str, str] = {}
        skipped = 0
        for chunk in document.chunks:
            if not chunk.text.strip():
                skipped += 1
                continue
            chunk_keys[chunk.chunk_id] = custom_chunk_key(document.document_id, chunk.text)
        accepted = len(chunk_keys)
        return InsertResult(
            accepted=accepted,
            skipped=skipped,
            entities=accepted * self.entities_per_chunk,
            relations=accepted * self.relations_per_chunk,
            chunk_keys=chunk_keys,
        )

    async def delete_document(self, workspace: str, document_id: str) -> bool:
        """Remove the document from the in-memory store."""

        self._maybe_fail()
        self.delete_calls.append((workspace, document_id))
        return self.documents.setdefault(workspace, {}).pop(document_id, None) is not None

    async def query(
        self,
        workspace: str,
        query: str,
        mode: str,
        top_k: int,
        document_ids: tuple[str, ...],
    ) -> EngineQueryResult:
        """Return every stored chunk, ignoring the filter on purpose.

        The service must apply ``filters.document_ids`` itself, so the fake
        deliberately returns chunks from every document.
        """

        self._maybe_fail()
        self.query_calls.append(
            {
                "workspace": workspace,
                "query": query,
                "mode": mode,
                "top_k": top_k,
                "document_ids": document_ids,
            }
        )
        if self.query_result is not None:
            return self.query_result
        chunks: list[EngineChunk] = []
        entities: list[EngineEntity] = []
        relations: list[EngineRelation] = []
        for document in self.documents.get(workspace, {}).values():
            for chunk in document.chunks:
                if not chunk.text.strip():
                    continue
                chunk_key = custom_chunk_key(document.document_id, chunk.text)
                chunks.append(EngineChunk(chunk_id=chunk_key, text=chunk.text))
                entities.append(
                    EngineEntity(name=f"E-{chunk.chunk_id}", chunk_ids=(chunk_key,))
                )
                relations.append(
                    EngineRelation(
                        source=f"A-{chunk.chunk_id}",
                        target=f"B-{chunk.chunk_id}",
                        description="relation",
                        chunk_ids=(chunk_key,),
                    )
                )
        return EngineQueryResult(
            chunks=tuple(chunks), entities=tuple(entities), relations=tuple(relations)
        )

    async def clear_workspace(self, workspace: str) -> None:
        """Record the clear and drop stored documents."""

        self._maybe_fail()
        self.cleared.append(workspace)
        self.documents.pop(workspace, None)
        self.workspaces.pop(workspace, None)

    def _maybe_fail(self) -> None:
        """Raise the configured failure, if any."""

        if self.failure is not None:
            raise self.failure


def workspace_payload(
    namespace: str = "legal",
    fingerprint: str = FINGERPRINT,
    language: str = "Chinese",
) -> dict[str, Any]:
    """Build a valid ``POST /v1/workspaces`` body.

    Args:
        namespace: Praxis namespace.
        fingerprint: Configuration fingerprint.
        language: Extraction language.

    Returns:
        JSON-ready request body.
    """

    return {
        "namespace": namespace,
        "config_fingerprint": fingerprint,
        "language": language,
        "embedding": {
            "base_url": "http://127.0.0.1:11434/v1",
            "model": "bge-m3",
            "api_key": "sk-embedding",
            "dimension": 1024,
        },
        "extract_llm": {
            "base_url": "http://127.0.0.1:11434/v1",
            "model": "qwen2.5:7b",
            "api_key": "sk-extract",
            "max_output_tokens": 9000,
            "timeout_seconds": 300,
        },
        "keyword_llm": {
            "base_url": "http://127.0.0.1:11434/v1",
            "model": "qwen2.5:7b",
            "api_key": "sk-keyword",
            "timeout_seconds": 60,
        },
    }


def chunks_payload(
    document_id: str = "doc-1",
    document_name: str = "民法典.pdf",
    chunks: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    """Build a valid ``POST /v1/workspaces/{namespace}/chunks`` body.

    Args:
        document_id: Praxis document id.
        document_name: Original document name.
        chunks: Chunk list; defaults to two chunks plus one blank chunk.

    Returns:
        JSON-ready request body.
    """

    if chunks is None:
        chunks = default_chunks()
    return {
        "document_id": document_id,
        "document_name": document_name,
        "chunks": chunks,
    }


def default_chunks() -> list[dict[str, Any]]:
    """Return the default chunk list used across tests.

    Returns:
        Two populated chunks and one blank chunk (skipped on insert).
    """

    return [
        {
            "chunk_id": "c-1",
            "text": "用户画像包含用户的偏好与历史行为。",
            "page": 3,
            "heading": "第一编",
            "upload_version": 2,
        },
        {
            "chunk_id": "c-2",
            "text": "合同解除需要满足法定条件。",
            "page": 4,
            "heading": "第二编",
            "upload_version": 2,
        },
        {
            "chunk_id": "c-3",
            "text": "   ",
            "page": None,
            "heading": "",
            "upload_version": 2,
        },
    ]


def query_payload(
    query: str = "用户画像如何生成",
    mode: str = "mix",
    top_k: int = 10,
    filters: dict[str, Any] | None = None,
    include_entities: bool = True,
) -> dict[str, Any]:
    """Build a valid ``POST /v1/workspaces/{namespace}/query`` body.

    Args:
        query: Query text.
        mode: Retrieval mode.
        top_k: Requested context count.
        filters: Optional filters object.
        include_entities: Whether to attach entity/relation evidence.

    Returns:
        JSON-ready request body.
    """

    return {
        "query": query,
        "mode": mode,
        "top_k": top_k,
        "filters": filters if filters is not None else {},
        "include_entities": include_entities,
    }


def auth_headers(token: str = TOKEN) -> dict[str, str]:
    """Return a bearer Authorization header.

    Args:
        token: Token value.

    Returns:
        Header dictionary.
    """

    return {"Authorization": f"Bearer {token}"}


def register_workspace(
    client: Any,
    namespace: str = "legal",
    fingerprint: str = FINGERPRINT,
    **kwargs: Any,
) -> dict[str, Any]:
    """Register a workspace through the API and assert success.

    Args:
        client: Test client.
        namespace: Praxis namespace.
        fingerprint: Configuration fingerprint.
        **kwargs: Extra request headers.

    Returns:
        Response body.
    """

    response = client.post(
        "/v1/workspaces",
        json=workspace_payload(namespace, fingerprint),
        headers=kwargs.get("headers"),
    )
    assert response.status_code == 200, response.text
    return response.json()


def ingest_document(client: Any, namespace: str = "legal", **kwargs: Any) -> dict[str, Any]:
    """Ingest a document through the API and assert success.

    Args:
        client: Test client.
        namespace: Praxis namespace.
        **kwargs: Forwarded to :func:`chunks_payload` and headers.

    Returns:
        Response body.
    """

    headers = kwargs.pop("headers", None)
    response = client.post(
        f"/v1/workspaces/{namespace}/chunks",
        json=chunks_payload(**kwargs),
        headers=headers,
    )
    assert response.status_code == 200, response.text
    return response.json()


__all__ = [
    "ALTERNATE_FINGERPRINT",
    "FINGERPRINT",
    "FakeEngine",
    "TOKEN",
    "auth_headers",
    "chunks_payload",
    "default_chunks",
    "ingest_document",
    "query_payload",
    "register_workspace",
    "workspace_payload",
]
