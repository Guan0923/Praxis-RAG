"""Binary object storage for source documents and parsed artifacts."""

from __future__ import annotations

import hashlib
from datetime import datetime, timezone
from pathlib import Path
from typing import Mapping

from sqlalchemy import Connection, Engine, delete, insert, select, update
from sqlalchemy.exc import IntegrityError

from .schema import blobs as blobs_table


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


class BlobStore:
    """Namespace-scoped, database-backed object storage."""

    def __init__(self, engine: Engine) -> None:
        self.engine = engine

    # -- reads ---------------------------------------------------------------
    def get(self, namespace: str, bucket: str, key: str) -> bytes | None:
        with self.engine.connect() as conn:
            row = conn.execute(
                select(blobs_table.c.content).where(
                    blobs_table.c.namespace == namespace,
                    blobs_table.c.bucket == bucket,
                    blobs_table.c.key == key,
                )
            ).first()
        return bytes(row[0]) if row is not None else None

    def get_text(self, namespace: str, bucket: str, key: str, *, encoding: str = "utf-8") -> str | None:
        content = self.get(namespace, bucket, key)
        return content.decode(encoding) if content is not None else None

    def exists(self, namespace: str, bucket: str, key: str) -> bool:
        with self.engine.connect() as conn:
            return (
                conn.execute(
                    select(blobs_table.c.key).where(
                        blobs_table.c.namespace == namespace,
                        blobs_table.c.bucket == bucket,
                        blobs_table.c.key == key,
                    ).limit(1)
                ).first()
                is not None
            )

    def list_keys(self, namespace: str, bucket: str, *, document_id: str | None = None) -> list[str]:
        statement = select(blobs_table.c.key).where(
            blobs_table.c.namespace == namespace, blobs_table.c.bucket == bucket
        )
        if document_id is not None:
            statement = statement.where(blobs_table.c.document_id == document_id)
        with self.engine.connect() as conn:
            return [str(row[0]) for row in conn.execute(statement.order_by(blobs_table.c.key))]

    # -- writes --------------------------------------------------------------
    def put(
        self,
        namespace: str,
        bucket: str,
        key: str,
        content: bytes,
        *,
        document_id: str | None = None,
        conn: Connection | None = None,
    ) -> None:
        if conn is not None:
            self._put(conn, namespace, bucket, key, content, document_id)
            return
        with self.engine.begin() as own:
            self._put(own, namespace, bucket, key, content, document_id)

    def put_many(
        self,
        namespace: str,
        bucket: str,
        items: Mapping[str, bytes],
        *,
        document_id: str | None = None,
        conn: Connection | None = None,
    ) -> None:
        if conn is not None:
            for key, content in items.items():
                self._put(conn, namespace, bucket, key, content, document_id)
            return
        with self.engine.begin() as own:
            for key, content in items.items():
                self._put(own, namespace, bucket, key, content, document_id)

    def put_file(
        self,
        namespace: str,
        bucket: str,
        key: str,
        path: Path,
        *,
        document_id: str | None = None,
        conn: Connection | None = None,
    ) -> None:
        self.put(namespace, bucket, key, path.read_bytes(), document_id=document_id, conn=conn)

    def put_tree(
        self,
        namespace: str,
        bucket: str,
        document_id: str,
        root: Path,
        *,
        conn: Connection | None = None,
    ) -> int:
        """Store every regular file below ``root`` under ``<document_id>/<relative>``."""

        files = [
            (candidate, candidate.relative_to(root).as_posix())
            for candidate in sorted(root.rglob("*"))
            if candidate.is_file()
        ]
        if conn is not None:
            for candidate, relative in files:
                self._put(conn, namespace, bucket, f"{document_id}/{relative}", candidate.read_bytes(), document_id)
            return len(files)
        with self.engine.begin() as own:
            for candidate, relative in files:
                self._put(own, namespace, bucket, f"{document_id}/{relative}", candidate.read_bytes(), document_id)
        return len(files)

    def delete_key(self, namespace: str, bucket: str, key: str, *, conn: Connection | None = None) -> None:
        statement = delete(blobs_table).where(
            blobs_table.c.namespace == namespace,
            blobs_table.c.bucket == bucket,
            blobs_table.c.key == key,
        )
        if conn is not None:
            conn.execute(statement)
            return
        with self.engine.begin() as own:
            own.execute(statement)

    def delete_document(
        self, namespace: str, bucket: str, document_id: str, *, conn: Connection | None = None
    ) -> None:
        statement = delete(blobs_table).where(
            blobs_table.c.namespace == namespace,
            blobs_table.c.bucket == bucket,
            blobs_table.c.document_id == document_id,
        )
        if conn is not None:
            conn.execute(statement)
            return
        with self.engine.begin() as own:
            own.execute(statement)

    @staticmethod
    def _put(
        conn: Connection,
        namespace: str,
        bucket: str,
        key: str,
        content: bytes,
        document_id: str | None,
    ) -> None:
        values = {
            "namespace": namespace,
            "bucket": bucket,
            "key": key,
            "document_id": document_id,
            "content": content,
            "size": len(content),
            "sha256": hashlib.sha256(content).hexdigest(),
            "created_at": _utc_now(),
        }
        result = conn.execute(
            update(blobs_table)
            .where(
                blobs_table.c.namespace == namespace,
                blobs_table.c.bucket == bucket,
                blobs_table.c.key == key,
            )
            .values(**{name: values[name] for name in ("document_id", "content", "size", "sha256", "created_at")})
        )
        if result.rowcount:
            return
        try:
            conn.execute(insert(blobs_table).values(**values))
        except IntegrityError:
            # Another instance inserted the same object between UPDATE and INSERT.
            conn.execute(
                update(blobs_table)
                .where(
                    blobs_table.c.namespace == namespace,
                    blobs_table.c.bucket == bucket,
                    blobs_table.c.key == key,
                )
                .values(**{name: values[name] for name in ("document_id", "content", "size", "sha256", "created_at")})
            )
