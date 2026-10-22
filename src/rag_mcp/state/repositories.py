"""Repository layer over the shared state database."""

from __future__ import annotations

import hashlib
import json
import secrets
from datetime import datetime, timezone
from typing import Any, Callable

from sqlalchemy import Engine, and_, delete, insert, select, update
from sqlalchemy.exc import IntegrityError, SQLAlchemyError

from . import crypto
from .schema import api_keys, documents, document_heads, knowledge_bases, mcp_catalogs


class StateError(RuntimeError):
    """Base error for state persistence failures."""


class StateConflict(StateError):
    """Raised when a unique key already exists."""


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _json(value: object) -> str:
    return json.dumps(value, ensure_ascii=False)


def _object(value: str | None, default: Any = None) -> Any:
    if not value:
        return default
    try:
        return json.loads(value)
    except json.JSONDecodeError as exc:
        raise StateError("Stored JSON state is invalid") from exc


class ApiKeyRepository:
    def __init__(self, engine: Engine, encryption_key: str | None = None) -> None:
        self.engine = engine
        self.encryption_key = encryption_key

    @staticmethod
    def _public(row) -> dict[str, object]:
        return {
            "id": row.id,
            "name": row.name,
            "prefix": row.prefix,
            "namespaces": _object(row.namespaces, []),
            "enabled": bool(row.enabled),
            "created_at": row.created_at,
            "exportable": bool(row.protected_key),
        }

    def list(self) -> list[dict[str, object]]:
        with self.engine.connect() as conn:
            rows = conn.execute(
                select(api_keys).order_by(api_keys.c.created_at.desc(), api_keys.c.id.desc())
            ).all()
        return [self._public(row) for row in rows]

    def create(self, name: str, namespaces: list[str]) -> dict[str, object]:
        secret = "prk_" + secrets.token_urlsafe(32)
        key_id = secrets.token_hex(12)
        timestamp = _now()
        values = {
            "id": key_id,
            "name": name,
            "prefix": secret[:12],
            "digest": hashlib.sha256(secret.encode()).hexdigest(),
            "namespaces": _json(namespaces),
            "enabled": True,
            "created_at": timestamp,
            "updated_at": timestamp,
            "protected_key": crypto.protect(secret, self.encryption_key),
        }
        try:
            with self.engine.begin() as conn:
                conn.execute(insert(api_keys).values(**values))
        except IntegrityError as exc:
            raise StateConflict("API Key 已存在") from exc
        except SQLAlchemyError as exc:
            raise StateError("API Key 无法保存") from exc
        return {**self._public(_Row(values)), "key": secret}

    def update(self, key_id: str, name: str, namespaces: list[str], enabled: bool) -> dict[str, object] | None:
        try:
            with self.engine.begin() as conn:
                changed = conn.execute(
                    update(api_keys)
                    .where(api_keys.c.id == key_id)
                    .values(name=name, namespaces=_json(namespaces), enabled=enabled, updated_at=_now())
                ).rowcount
                if not changed:
                    return None
                row = conn.execute(select(api_keys).where(api_keys.c.id == key_id)).one()
        except SQLAlchemyError as exc:
            raise StateError("API Key 无法更新") from exc
        return self._public(row)

    def delete(self, key_id: str) -> bool:
        with self.engine.begin() as conn:
            return bool(conn.execute(delete(api_keys).where(api_keys.c.id == key_id)).rowcount)

    def namespaces_for_key(self, secret: str) -> list[str]:
        digest = hashlib.sha256(secret.encode()).hexdigest()
        with self.engine.connect() as conn:
            row = conn.execute(
                select(api_keys.c.namespaces, api_keys.c.enabled).where(api_keys.c.digest == digest)
            ).first()
        if row is None:
            raise LookupError("访问 API Key 在当前服务中不存在或已删除")
        if not row.enabled:
            raise PermissionError("访问 API Key 已停用")
        return list(_object(row.namespaces, []))

    def secret_for_namespace(self, key_id: str, namespace: str) -> str:
        with self.engine.connect() as conn:
            row = conn.execute(select(api_keys).where(api_keys.c.id == key_id)).first()
        if row is None:
            raise LookupError("API Key 不存在")
        if not row.enabled or namespace not in _object(row.namespaces, []):
            raise PermissionError("API Key 已停用或未获授权访问此 Namespace")
        if not row.protected_key:
            raise ValueError("该密钥仅保存摘要，无法显示原值；请在 API Key 页面重新创建")
        return crypto.unprotect(row.protected_key, self.encryption_key)

    def import_row(
        self,
        *,
        key_id: str,
        name: str,
        prefix: str,
        digest: str,
        namespaces: list[str],
        enabled: bool,
        created_at: str,
        protected_key: str | None,
    ) -> None:
        values = {
            "id": key_id,
            "name": name,
            "prefix": prefix,
            "digest": digest,
            "namespaces": _json(namespaces),
            "enabled": enabled,
            "created_at": created_at,
            "updated_at": _now(),
            "protected_key": crypto.protect(crypto.unprotect(protected_key or "", self.encryption_key), self.encryption_key)
            if protected_key
            else None,
        }
        with self.engine.begin() as conn:
            existing = conn.execute(select(api_keys.c.id).where(api_keys.c.id == key_id)).first()
            if existing:
                conn.execute(update(api_keys).where(api_keys.c.id == key_id).values(**values))
            else:
                conn.execute(insert(api_keys).values(**values))


class KnowledgeBaseRepository:
    def __init__(self, engine: Engine) -> None:
        self.engine = engine

    def list(self) -> list[dict]:
        with self.engine.connect() as conn:
            rows = conn.execute(
                select(knowledge_bases.c.settings).order_by(knowledge_bases.c.namespace)
            ).all()
        return [_object(row.settings, {}) for row in rows]

    def get(self, namespace: str) -> dict | None:
        with self.engine.connect() as conn:
            row = conn.execute(
                select(knowledge_bases.c.settings).where(knowledge_bases.c.namespace == namespace)
            ).first()
        return _object(row.settings) if row else None

    def create(self, settings: dict) -> None:
        timestamp = _now()
        try:
            with self.engine.begin() as conn:
                conn.execute(insert(knowledge_bases).values(
                    namespace=settings["namespace"],
                    collection=settings["collection"],
                    settings=_json(settings),
                    version=1,
                    created_at=timestamp,
                    updated_at=timestamp,
                ))
        except IntegrityError as exc:
            raise StateConflict("该知识库已存在") from exc
        except SQLAlchemyError as exc:
            raise StateError("知识库配置无法保存") from exc

    def update(self, settings: dict) -> None:
        with self.engine.begin() as conn:
            changed = conn.execute(
                update(knowledge_bases)
                .where(knowledge_bases.c.namespace == settings["namespace"])
                .values(
                    settings=_json(settings),
                    collection=settings["collection"],
                    version=knowledge_bases.c.version + 1,
                    updated_at=_now(),
                )
            ).rowcount
        if not changed:
            raise LookupError("知识库不存在")

    def snapshot(self, namespace: str):
        with self.engine.connect() as conn:
            row = conn.execute(select(knowledge_bases.c.settings, knowledge_bases.c.version).where(
                knowledge_bases.c.namespace == namespace)).first()
        return (_object(row.settings), int(row.version)) if row else (None, None)

    def version(self, namespace: str) -> int | None:
        with self.engine.connect() as conn:
            row = conn.execute(
                select(knowledge_bases.c.version).where(knowledge_bases.c.namespace == namespace)
            ).first()
        return int(row[0]) if row else None

    def delete(self, namespace: str) -> bool:
        with self.engine.begin() as conn:
            return bool(
                conn.execute(delete(knowledge_bases).where(knowledge_bases.c.namespace == namespace)).rowcount
            )


def _document_record(row) -> dict[str, object]:
    version = row._mapping.get("active_version")
    return {
        **({"operation_id": version} if version else {}),
        "name": row.name,
        "size": int(row.size),
        "modified_at": row.modified_at,
        "indexed_at": row.indexed_at,
        "document_id": row.document_id,
        "chunk_count": int(row.chunk_count),
        "collection": row.collection,
        "diagnostics": _object(row.diagnostics, []),
        "skipped_object_count": int(row.skipped_object_count),
        "sha256": row.sha256,
        "chunking": _object(row.chunking, {}),
    }


def _document_select():
    return select(documents, document_heads.c.version.label("active_version")).select_from(
        documents.outerjoin(document_heads, and_(
            documents.c.namespace == document_heads.c.namespace,
            documents.c.document_id == document_heads.c.document_id)))


class DocumentRepository:
    def __init__(self, engine: Engine) -> None:
        self.engine = engine

    def list(self, namespace: str) -> list[dict[str, object]]:
        with self.engine.connect() as conn:
            rows = conn.execute(
                _document_select().where(documents.c.namespace == namespace)
            ).all()
        return sorted((_document_record(row) for row in rows), key=lambda item: str(item["name"]).casefold())

    def get(self, namespace: str, name: str) -> dict[str, object] | None:
        with self.engine.connect() as conn:
            row = conn.execute(
                _document_select().where(
                    and_(documents.c.namespace == namespace, documents.c.name == name)
                )
            ).first()
        return _document_record(row) if row else None

    def get_by_id(self, namespace: str, document_id: str) -> dict[str, object] | None:
        with self.engine.connect() as conn:
            row = conn.execute(
                _document_select().where(
                    and_(documents.c.namespace == namespace, documents.c.document_id == document_id)
                )
            ).first()
        return _document_record(row) if row else None

    def candidates(self, namespace: str, sha256: str, name: str | None = None) -> list[dict[str, object]]:
        """Records that may match a content hash, including legacy rows without one."""

        from sqlalchemy import or_

        statement = _document_select().where(documents.c.namespace == namespace)
        if name is not None:
            statement = statement.where(documents.c.name == name)
        if sha256:
            statement = statement.where(or_(documents.c.sha256 == sha256, documents.c.sha256 == ""))
        with self.engine.connect() as conn:
            rows = conn.execute(statement).all()
        return [_document_record(row) for row in rows]

    def namespaces(self) -> dict[str, int]:
        from sqlalchemy import func

        with self.engine.connect() as conn:
            rows = conn.execute(
                select(documents.c.namespace, func.count()).group_by(documents.c.namespace)
            ).all()
        return {str(namespace): int(count) for namespace, count in rows}

    def upsert(self, namespace: str, record: dict[str, object], *, conn=None) -> None:
        values = {
            "namespace": namespace,
            "name": record["name"],
            "document_id": record["document_id"],
            "size": int(record.get("size", 0)),
            "sha256": record.get("sha256", ""),
            "chunking": _json(record.get("chunking", {})),
            "collection": record.get("collection", ""),
            "chunk_count": int(record.get("chunk_count", 0)),
            "modified_at": record.get("modified_at", _now()),
            "indexed_at": record.get("indexed_at", _now()),
            "diagnostics": _json(record.get("diagnostics", [])),
            "skipped_object_count": int(record.get("skipped_object_count", 0)),
        }

        def write(connection) -> None:
            changed = connection.execute(
                update(documents)
                .where(and_(documents.c.namespace == namespace, documents.c.name == record["name"]))
                .values(**values)
            ).rowcount
            if changed:
                return
            try:
                connection.execute(insert(documents).values(**values))
            except IntegrityError:
                # Another instance inserted this document after our UPDATE.
                connection.execute(
                    update(documents)
                    .where(and_(documents.c.namespace == namespace, documents.c.name == record["name"]))
                    .values(**values)
                )

        if conn is not None:
            write(conn)
            return
        with self.engine.begin() as own:
            write(own)

    def delete(self, namespace: str, name: str, *, conn=None) -> bool:
        statement = delete(documents).where(
            and_(documents.c.namespace == namespace, documents.c.name == name)
        )
        if conn is not None:
            return bool(conn.execute(statement).rowcount)
        with self.engine.begin() as own:
            return bool(own.execute(statement).rowcount)

    def import_record(self, namespace: str, record: dict[str, object]) -> None:
        self.upsert(namespace, record)


class CatalogRepository:
    def __init__(self, engine: Engine) -> None:
        self.engine = engine

    def load(self, namespace: str) -> str | None:
        with self.engine.connect() as conn:
            row = conn.execute(
                select(mcp_catalogs.c.data).where(mcp_catalogs.c.namespace == namespace)
            ).first()
        return str(row[0]) if row else None

    def mutate(
        self,
        namespace: str,
        transform: Callable[[str | None], tuple[str, Any, bool]],
    ) -> Any:
        """Read-modify-write one catalog row under a row lock."""

        try:
            with self.engine.begin() as conn:
                row = conn.execute(
                    select(mcp_catalogs.c.data)
                    .where(mcp_catalogs.c.namespace == namespace)
                    .with_for_update()
                ).first()
                new_data, result, changed = transform(str(row[0]) if row else None)
                if not changed:
                    return result
                if row is None:
                    conn.execute(insert(mcp_catalogs).values(
                        namespace=namespace, data=new_data, version=1, updated_at=_now()
                    ))
                else:
                    conn.execute(
                        update(mcp_catalogs)
                        .where(mcp_catalogs.c.namespace == namespace)
                        .values(data=new_data, version=mcp_catalogs.c.version + 1, updated_at=_now())
                    )
                return result
        except StateError:
            raise
        except SQLAlchemyError as exc:
            raise StateError("MCP 配置无法保存") from exc


class _Row:
    """Attribute view used to reuse the public serializer for a fresh insert."""

    def __init__(self, values: dict[str, object]) -> None:
        self.__dict__.update(values)
