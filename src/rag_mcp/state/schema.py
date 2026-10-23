"""Portable schema for all service state (MySQL in production, SQLite for tests)."""

from __future__ import annotations

from sqlalchemy import (
    BigInteger,
    Boolean,
    Column,
    Double,
    Index,
    Integer,
    LargeBinary,
    MetaData,
    String,
    Table,
    Text,
    UniqueConstraint,
)
from sqlalchemy.dialects import mysql

_MYSQL_TABLE = {"mysql_engine": "InnoDB", "mysql_charset": "utf8mb4", "mysql_collate": "utf8mb4_bin"}


def _ci_string(length: int) -> String:
    """Case-sensitive identifier column; MySQL collations are otherwise case-insensitive."""

    return String(length).with_variant(String(length, collation="utf8mb4_bin"), "mysql")


def _long_blob():
    return LargeBinary().with_variant(mysql.LONGBLOB(), "mysql")


def _long_text():
    return Text().with_variant(mysql.LONGTEXT(), "mysql")


metadata = MetaData()

api_keys = Table(
    "api_keys",
    metadata,
    Column("id", _ci_string(32), primary_key=True),
    Column("name", String(80), nullable=False),
    Column("prefix", String(16), nullable=False),
    Column("digest", _ci_string(64), nullable=False),
    Column("namespaces", Text, nullable=False),
    Column("enabled", Boolean, nullable=False),
    Column("created_at", String(40), nullable=False),
    Column("updated_at", String(40), nullable=False),
    Column("protected_key", Text),
    UniqueConstraint("digest", name="uq_api_keys_digest"),
    **_MYSQL_TABLE,
)

knowledge_bases = Table(
    "knowledge_bases",
    metadata,
    Column("namespace", _ci_string(64), primary_key=True),
    Column("collection", _ci_string(128), nullable=False),
    Column("settings", _long_text(), nullable=False),
    Column("version", BigInteger, nullable=False, default=1),
    Column("created_at", String(40), nullable=False),
    Column("updated_at", String(40), nullable=False),
    Index("ix_knowledge_bases_collection", "collection"),
    **_MYSQL_TABLE,
)

documents = Table(
    "documents",
    metadata,
    Column("namespace", _ci_string(64), primary_key=True),
    Column("name", _ci_string(200), primary_key=True),
    Column("document_id", _ci_string(64), nullable=False),
    Column("size", BigInteger, nullable=False),
    Column("sha256", _ci_string(64), nullable=False),
    Column("chunking", Text, nullable=False),
    Column("collection", String(128), nullable=False),
    Column("chunk_count", Integer, nullable=False, default=0),
    Column("modified_at", String(40), nullable=False),
    Column("indexed_at", String(40), nullable=False),
    Column("diagnostics", _long_text(), nullable=False),
    Column("skipped_object_count", Integer, nullable=False, default=0),
    Column("del_flag", Boolean, nullable=False, default=False, server_default="0"),
    UniqueConstraint("namespace", "document_id", name="uq_documents_namespace_id"),
    Index("ix_documents_namespace_sha256", "namespace", "sha256"),
    **_MYSQL_TABLE,
)

blobs = Table(
    "blobs",
    metadata,
    Column("namespace", _ci_string(64), primary_key=True),
    Column("bucket", _ci_string(32), primary_key=True),
    Column("key", _ci_string(512), primary_key=True),
    Column("document_id", _ci_string(64), nullable=True),
    Column("content", _long_blob(), nullable=False),
    Column("size", BigInteger, nullable=False),
    Column("sha256", _ci_string(64), nullable=False),
    Column("created_at", String(40), nullable=False),
    Index("ix_blobs_document", "namespace", "bucket", "document_id"),
    **_MYSQL_TABLE,
)

mcp_catalogs = Table(
    "mcp_catalogs",
    metadata,
    Column("namespace", _ci_string(64), primary_key=True),
    Column("data", _long_text(), nullable=False),
    Column("version", BigInteger, nullable=False, default=1),
    Column("updated_at", String(40), nullable=False),
    **_MYSQL_TABLE,
)

upload_operations = Table(
    "upload_operations",
    metadata,
    Column("id", _ci_string(32), primary_key=True),
    Column("namespace", _ci_string(64), nullable=False),
    Column("name", _ci_string(200), nullable=False),
    Column("document_id", _ci_string(64), nullable=False),
    Column("collection", _ci_string(128), nullable=False),
    Column("idempotency_key", _ci_string(200)),
    Column("fingerprint", _ci_string(128)),
    Column("owner", _ci_string(32), nullable=False),
    Column("old_version", _ci_string(32)),
    Column("status", _ci_string(32), nullable=False),
    Column("result", _long_text()),
    Column("error", Text),
    Column("cleanup_pending", Boolean, nullable=False, default=False),
    Column("workspace", Text),
    Column("workspace_node", _ci_string(255)),
    Column("created_at", Double, nullable=False),
    Column("updated_at", Double, nullable=False),
    UniqueConstraint("namespace", "idempotency_key", name="uq_upload_idempotency"),
    Index("ix_upload_unfinished", "namespace", "status"),
    **_MYSQL_TABLE,
)

upload_steps = Table(
    "upload_steps",
    metadata,
    Column("operation_id", _ci_string(32), primary_key=True),
    Column("name", _ci_string(128), primary_key=True),
    Column("status", _ci_string(32), nullable=False),
    Column("details", _long_text(), nullable=False),
    Column("error", Text),
    Column("started_at", Double, nullable=False),
    Column("finished_at", Double),
    **_MYSQL_TABLE,
)

document_heads = Table(
    "document_heads",
    metadata,
    Column("namespace", _ci_string(64), primary_key=True),
    Column("document_id", _ci_string(64), primary_key=True),
    Column("version", _ci_string(32), nullable=False),
    Column("updated_at", Double, nullable=False),
    **_MYSQL_TABLE,
)

document_leases = Table(
    "document_leases",
    metadata,
    Column("namespace", _ci_string(64), primary_key=True),
    Column("resource", _ci_string(80), primary_key=True),
    Column("owner", _ci_string(32), nullable=False),
    Column("operation_id", _ci_string(32)),
    Column("expires_at", Double, nullable=False),
    **_MYSQL_TABLE,
)

upload_objects = Table(
    "upload_objects",
    metadata,
    Column("operation_id", _ci_string(32), primary_key=True),
    Column("bucket", _ci_string(32), primary_key=True),
    Column("key", _ci_string(512), primary_key=True),
    Column("content", _long_blob(), nullable=False),
    Column("metadata", _long_text()),
    Column("created_at", Double, nullable=False),
    **_MYSQL_TABLE,
)

cache_revisions = Table(
    "cache_revisions", metadata,
    Column("kind", _ci_string(32), primary_key=True),
    Column("scope", _ci_string(64), primary_key=True),
    Column("revision", BigInteger, nullable=False),
    **_MYSQL_TABLE,
)

cache_outbox = Table(
    "cache_outbox", metadata,
    Column("id", _ci_string(32), primary_key=True),
    Column("kind", _ci_string(32), nullable=False),
    Column("scope", _ci_string(64), nullable=False),
    Column("revision", BigInteger, nullable=False),
    Column("status", _ci_string(24), nullable=False),
    Column("attempts", Integer, nullable=False, default=0),
    Column("retry_at", Double, nullable=False),
    Column("created_at", Double, nullable=False),
    Column("completed_at", Double),
    Column("last_error", _ci_string(128)),
    Index("ix_cache_outbox_pending", "status", "retry_at"),
    **_MYSQL_TABLE,
)

search_result_snapshots = Table(
    "search_result_snapshots", metadata,
    Column("id", _ci_string(36), primary_key=True),
    Column("namespace", _ci_string(64), nullable=False),
    Column("content", _long_text(), nullable=False),
    Column("created_at", Double, nullable=False),
    Column("expires_at", Double, nullable=False),
    Index("ix_search_result_expiry", "expires_at"),
    Index("ix_search_result_namespace", "namespace"),
    **_MYSQL_TABLE,
)

artifact_snapshots = Table(
    "artifact_snapshots", metadata,
    Column("id", _ci_string(36), primary_key=True),
    Column("namespace", _ci_string(64), nullable=False),
    Column("identity", _ci_string(64), nullable=False),
    Column("document_id", _ci_string(64), nullable=False),
    Column("version", _ci_string(64)),
    Column("kind", _ci_string(32), nullable=False),
    Column("name", _ci_string(512), nullable=False),
    Column("mime_type", String(128), nullable=False),
    Column("content", _long_blob(), nullable=False),
    Column("created_at", Double, nullable=False),
    Column("expires_at", Double, nullable=False),
    UniqueConstraint("namespace", "identity", name="uq_artifact_identity"),
    Index("ix_artifact_expiry", "expires_at"),
    Index("ix_artifact_namespace", "namespace"),
    **_MYSQL_TABLE,
)

document_tasks = Table(
    "document_tasks", metadata,
    Column("artifact_id", _ci_string(36), primary_key=True),
    Column("namespace", _ci_string(64), nullable=False),
    Column("name", _ci_string(200), nullable=False),
    Column("name_key", _ci_string(64), nullable=False),
    Column("content", _long_blob(), nullable=False),
    Column("sha256", _ci_string(64), nullable=False),
    Column("idempotency_key", _ci_string(128)),
    Column("configuration_version", BigInteger, nullable=False),
    Column("status", _ci_string(24), nullable=False),
    Column("stage", _ci_string(64), nullable=False),
    Column("attempts", Integer, nullable=False),
    Column("owner", _ci_string(32)),
    Column("lease_until", Double, nullable=False),
    Column("operation_id", _ci_string(32)),
    Column("error", Text),
    Column("created_at", Double, nullable=False),
    Column("updated_at", Double, nullable=False),
    UniqueConstraint("namespace", "idempotency_key", name="uq_document_task_idempotency"),
    Index("ix_document_tasks_name", "namespace", "name_key", "status"),
    Index("ix_document_tasks_recovery", "status", "lease_until"),
    **_MYSQL_TABLE,
)

document_task_outbox = Table(
    "document_task_outbox", metadata,
    Column("artifact_id", _ci_string(36), primary_key=True),
    Column("pending", Boolean, nullable=False),
    Column("generation", BigInteger, nullable=False),
    Column("retry_at", Double, nullable=False),
    Index("ix_document_task_dispatch", "pending", "retry_at"),
    **_MYSQL_TABLE,
)

SOURCE_BUCKET = "source"
PDF_ASSET_BUCKET = "pdf-assets"
WORD_ASSET_BUCKET = "word-assets"
