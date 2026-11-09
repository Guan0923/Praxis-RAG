"""Engine and repository assembly for the authoritative state database."""

from __future__ import annotations

import logging
from pathlib import Path

from sqlalchemy import Engine, create_engine
from sqlalchemy.engine import make_url

from .blobs import BlobStore
from .repositories import (
    ApiKeyRepository,
    CatalogRepository,
    DocumentRepository,
    KnowledgeBaseRepository,
    StateError,
)
from .schema import metadata
from .schema_migrations import migrate_document_outbox, migrate_soft_delete
from .settings import StateSettings
from .uploads import UploadJournal
from .cache_sync import CacheSynchronizer
from .read_views import CachedReadViews
from .artifacts import ArtifactRegistry
from .result_snapshots import ResultSnapshots

logger = logging.getLogger(__name__)


class StateStores:
    """One engine shared by every repository in a service process."""

    def __init__(self, settings: StateSettings, *, create_schema: bool = True) -> None:
        self.settings = settings
        self.engine = _create_engine(settings)
        if create_schema:
            try:
                metadata.create_all(self.engine)
                migrate_soft_delete(self.engine)
                migrate_document_outbox(self.engine)
            except Exception as exc:  # noqa: BLE001 - surfaced as a configuration error
                raise StateError(f"状态数据库初始化失败：{type(exc).__name__}") from exc
        self.cache_sync = CacheSynchronizer(self)
        self.blobs = BlobStore(self.engine)
        self.api_keys = ApiKeyRepository(self.engine, settings.encryption_key, self.cache_sync)
        self.knowledge_bases = KnowledgeBaseRepository(self.engine, self.cache_sync)
        self.documents = DocumentRepository(self.engine, self.cache_sync)
        self.catalogs = CatalogRepository(self.engine, self.cache_sync)
        self.uploads = UploadJournal(self)
        from .document_jobs import DocumentJobs

        self.document_jobs = DocumentJobs(self)
        from .document_queue import DocumentJobDispatcher
        self.document_dispatcher = DocumentJobDispatcher(self)
        self.views = CachedReadViews(self)
        self.results = ResultSnapshots(self)
        self.artifacts = ArtifactRegistry(self, ttl_seconds=self.results.settings.ttl_seconds,
                                          max_bytes=self.results.settings.max_artifact_bytes)
        from .search_tasks import SearchTasks
        from .search_queue import SearchTaskDispatcher
        self.search_tasks = SearchTasks(self)
        self.search_dispatcher = SearchTaskDispatcher(self)
        _warn_if_unencrypted(settings)

    @classmethod
    def create(
        cls,
        data_root: Path | str | None = None,
        *,
        settings: StateSettings | None = None,
        create_schema: bool = True,
    ) -> "StateStores":
        # data_root is still accepted by callers that pass their service data
        # directory, but it never selects the database: the connection always
        # comes from StateSettings/RAG_STATE_URL (MySQL only).
        resolved = settings or StateSettings.from_env()
        return cls(resolved, create_schema=create_schema and resolved.auto_schema)

    def close(self) -> None:
        self.search_dispatcher.close()
        self.document_dispatcher.close()
        self.cache_sync.close()
        self.uploads.close()
        self.engine.dispose()

    def ping(self) -> None:
        with self.engine.connect() as conn:
            conn.exec_driver_sql("SELECT 1")

    def describe(self) -> dict[str, object]:
        url = make_url(self.settings.url)
        return {
            "backend": "mysql",
            "database": url.database,
            "host": url.host,
            "encrypted": bool(self.settings.encryption_key),
        }


def _create_engine(settings: StateSettings) -> Engine:
    return create_engine(
        settings.url,
        pool_size=settings.pool_size,
        max_overflow=settings.max_overflow,
        pool_recycle=settings.pool_recycle,
        pool_timeout=settings.pool_timeout,
        pool_pre_ping=True,
        echo=settings.echo,
        future=True,
    )


def _warn_if_unencrypted(settings: StateSettings) -> None:
    if not settings.encryption_key:
        logger.warning(
            "RAG_STATE_ENCRYPTION_KEY is not set: provider and API keys may not be readable on other instances"
        )
