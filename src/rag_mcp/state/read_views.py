"""Disposable public read models; repository reads remain authoritative."""

from __future__ import annotations

import hashlib
import time
from typing import TYPE_CHECKING

from sqlalchemy import Connection, func, select
from sqlalchemy.exc import SQLAlchemyError

from .repositories import ApiKeyRepository, StateError, _document_record, _document_select, _object
from .schema import (
    api_keys, documents, knowledge_bases, mcp_catalogs, document_heads,
    document_leases, upload_operations, upload_steps,
)

if TYPE_CHECKING:
    from .store import StateStores


def _public(settings: dict) -> dict:
    # Import lazily: the public serializer's compatibility facade imports StateStores.
    from ..knowledge_bases import public_settings

    return public_settings(settings)


class CachedReadViews:
    """Load each cache entry using only the connection supplied by cache synchronization."""

    def __init__(self, state: StateStores) -> None:
        self.state = state
        for kind, loader in (("kb_public", self._load_knowledge_base),
                             ("kb_list", self._load_knowledge_bases),
                             ("documents", self._load_documents),
                             ("catalog", self._load_catalog),
                             ("api_keys", self._load_api_keys),
                             ("heads", self._load_heads),
                             ("leases", self._load_leases),
                             ("upload_list", self._load_upload_list),
                             ("upload_status", self._load_upload_status)):
            state.cache_sync.register(kind, loader)

    def _get_or_load(self, kind: str, scope: str, loader):
        try:
            return self.state.cache_sync.get_or_load(kind, scope, loader)
        except SQLAlchemyError as exc:
            raise StateError("Cached read view cannot be loaded") from exc

    @staticmethod
    def _settings(conn: Connection, namespace: str) -> dict | None:
        row = conn.execute(
            select(knowledge_bases.c.settings).where(knowledge_bases.c.namespace == namespace)
        ).first()
        return _object(row.settings) if row else None

    def _load_knowledge_base(self, conn: Connection, namespace: str) -> dict | None:
        settings = self._settings(conn, namespace)
        return _public(settings) if settings is not None else None

    def knowledge_base(self, namespace: str) -> dict | None:
        return self._get_or_load("kb_public", namespace, lambda conn: self._load_knowledge_base(conn, namespace))

    def _load_knowledge_bases(self, conn: Connection, scope: str = "") -> list[dict]:
        registered = {
            row.namespace: _public(_object(row.settings, {}))
            for row in conn.execute(select(knowledge_bases.c.namespace, knowledge_bases.c.settings))
        }
        counts = {
            str(namespace): int(count)
            for namespace, count in conn.execute(
                select(documents.c.namespace, func.count()).where(documents.c.del_flag.is_(False)).group_by(documents.c.namespace)
            )
        }
        return [
            {**registered.get(namespace, {"namespace": namespace}),
             "configured": namespace in registered, "document_count": counts.get(namespace, 0)}
            for namespace in sorted(set(registered) | set(counts))
        ]

    def knowledge_bases(self) -> list[dict]:
        return self._get_or_load("kb_list", "", self._load_knowledge_bases)

    def _load_documents(self, conn: Connection, namespace: str) -> list[dict[str, object]]:
        settings = self._settings(conn, namespace)
        records = [
            _document_record(row)
            for row in conn.execute(_document_select().where(documents.c.namespace == namespace))
        ]
        if settings:
            for record in records:
                record["collection"] = settings["collection"]
                record["chunking"] = {**record["chunking"], "embedding_model": settings["embedding_model"]}
        return sorted(records, key=lambda item: str(item["name"]).casefold())

    def documents(self, namespace: str) -> list[dict[str, object]]:
        return self._get_or_load("documents", namespace, lambda conn: self._load_documents(conn, namespace))

    def _load_catalog(self, conn: Connection, namespace: str) -> str | None:
        row = conn.execute(
            select(mcp_catalogs.c.data).where(mcp_catalogs.c.namespace == namespace)
        ).first()
        return str(row.data) if row else None

    def catalog(self, namespace: str) -> str | None:
        return self._get_or_load("catalog", namespace, lambda conn: self._load_catalog(conn, namespace))


    def _load_api_keys(self, conn: Connection, scope: str = "") -> list[dict]:
        return [ApiKeyRepository._public(row) for row in conn.execute(
            select(api_keys).order_by(api_keys.c.created_at.desc(), api_keys.c.id.desc()))]

    def api_keys(self) -> list[dict]:
        """Administration display only: never use this snapshot to authorize."""
        return self._get_or_load("api_keys", "", self._load_api_keys)

    def _load_heads(self, conn: Connection, namespace: str) -> dict[str, str]:
        return {document_id: version for document_id, version in conn.execute(
            select(document_heads.c.document_id, document_heads.c.version).select_from(
                document_heads.outerjoin(documents, (documents.c.namespace == document_heads.c.namespace)
                    & (documents.c.document_id == document_heads.c.document_id))).where(
                document_heads.c.namespace == namespace,
                (documents.c.del_flag.is_(False)) | documents.c.document_id.is_(None)))}

    def published_versions(self, namespace: str) -> dict[str, str]:
        """Display snapshot, not the Qdrant visibility provider."""
        return self._get_or_load("heads", namespace, lambda conn: self._load_heads(conn, namespace))

    def _load_leases(self, conn: Connection, namespace: str) -> list[dict]:
        records = []
        query = select(document_leases).where(
            document_leases.c.namespace == namespace,
            document_leases.c.resource != "namespace:guard").order_by(document_leases.c.resource)
        for row in conn.execute(query):
            records.append({
                "resource": row.resource,
                "holder_id": hashlib.sha256(row.owner.encode()).hexdigest()[:16],
                "operation_id": row.operation_id,
                "expires_at": row.expires_at,
            })
        return records

    def leases(self, namespace: str) -> list[dict]:
        records = self._get_or_load("leases", namespace, lambda conn: self._load_leases(conn, namespace))
        now = time.time()
        # Expiry is time-dependent even if no write invalidates the snapshot.
        return [{**record, "expired": record["expires_at"] <= now} for record in records]

    @staticmethod
    def _operation_summary(row) -> dict:
        return {key: getattr(row, key) for key in (
            "id", "namespace", "name", "document_id", "collection", "old_version",
            "status", "error", "cleanup_pending", "created_at", "updated_at")}

    def _load_upload_list(self, conn: Connection, namespace: str) -> list[dict]:
        query = select(upload_operations).where(upload_operations.c.namespace == namespace).order_by(
            upload_operations.c.created_at.desc(), upload_operations.c.id.desc()).limit(100)
        return [self._operation_summary(row) for row in conn.execute(query)]

    def upload_operations(self, namespace: str) -> list[dict]:
        """Latest 100 operations for display; recovery uses UploadJournal.unfinished."""
        return self._get_or_load("upload_list", namespace, lambda conn: self._load_upload_list(conn, namespace))

    def _load_upload_status(self, conn: Connection, operation_id: str) -> dict | None:
        row = conn.execute(select(upload_operations).where(upload_operations.c.id == operation_id)).first()
        if row is None:
            return None
        result = self._operation_summary(row)
        result["version"] = row.id
        result["holder_id"] = hashlib.sha256(row.owner.encode()).hexdigest()[:16]
        query = select(upload_steps).where(upload_steps.c.operation_id == operation_id).order_by(
            upload_steps.c.started_at, upload_steps.c.name).limit(257)
        steps = [{key: getattr(step, key) for key in (
            "name", "status", "error", "started_at", "finished_at")} for step in conn.execute(query)]
        result["steps"] = steps[:256]
        result["steps_truncated"] = len(steps) > 256
        return result

    def upload_operation(self, namespace: str, operation_id: str) -> dict | None:
        result = self._get_or_load("upload_status", operation_id,
            lambda conn: self._load_upload_status(conn, operation_id))
        # UUIDs identify records; they never grant cross-namespace access.
        return result if result is not None and result["namespace"] == namespace else None
