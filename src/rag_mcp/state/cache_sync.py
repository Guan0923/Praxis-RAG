"""Read-through state caching and durable refresh delivery.

MySQL owns revisions/outbox. Redis failures never roll back a committed write.
Only event IDs cross the queue; consumers load current redacted read models.
"""

from __future__ import annotations

import logging
import threading
import time

from sqlalchemy import select, update, func, tuple_
from sqlalchemy.dialects.mysql import insert as mysql_insert
from sqlalchemy.dialects.sqlite import insert as sqlite_insert

from .cache import CacheSettings, CacheUnavailable, RedisStateCache
from .cache_queue import RocketMQSettings, RocketMQCacheQueue
from .schema import cache_outbox, cache_revisions

logger = logging.getLogger(__name__)


class CacheSynchronizer:
    def __init__(self, state, *, backend=None, transport=None):
        self.state = state
        self.engine = state.engine
        self.backend = backend or RedisStateCache(CacheSettings.from_env())
        queue_settings = RocketMQSettings.from_env()
        self.transport = transport or (RocketMQCacheQueue(queue_settings) if queue_settings.endpoint else None)
        self.loaders = {}
        self._stop = threading.Event()
        self._worker = None
        self._mutex = threading.Lock()
        self._queue_retry_at = 0

    def register(self, kind, loader):
        self.loaders[kind] = loader

    def _snapshot(self, kind, scope, loader):
        # Lock the revision before the first consistent read. Writers acquire
        # this same latch before changing the underlying read model.
        with self.engine.begin() as conn:
            values = dict(kind=kind, scope=scope, revision=0)
            if conn.dialect.name == "mysql":
                stmt = mysql_insert(cache_revisions).values(**values)
                stmt = stmt.on_duplicate_key_update(revision=cache_revisions.c.revision)
            else:
                stmt = sqlite_insert(cache_revisions).values(**values).on_conflict_do_nothing()
            conn.execute(stmt)
            rev = int(conn.scalar(select(cache_revisions.c.revision).where(
                cache_revisions.c.kind == kind, cache_revisions.c.scope == scope).with_for_update()))
            value = loader(conn)
        return rev, value

    def get_or_load(self, kind, scope, loader):
        if not self.backend.enabled:
            with self.engine.connect() as conn:
                return loader(conn)
        try:
            hit, value = self.backend.get(kind, scope)
            if hit:
                return value
        except CacheUnavailable:
            with self.engine.connect() as conn:
                return loader(conn)
        rev, value = self._snapshot(kind, scope, loader)
        try:
            self.backend.put(kind, scope, rev, value, deleted=value is None)
        except CacheUnavailable:
            pass
        return value

    def refresh_event(self, event_id):
        with self.engine.connect() as conn:
            row = conn.execute(select(cache_outbox).where(cache_outbox.c.id == event_id)).mappings().first()
        if row is None or row["status"] == "DONE":
            return True
        loader = self.loaders.get(row["kind"])
        if loader is None or not self.backend.enabled:
            return False
        try:
            rev, value = self._snapshot(row["kind"], row["scope"], lambda c: loader(c, row["scope"]))
            self.backend.put(row["kind"], row["scope"], rev, value, deleted=value is None)
        except CacheUnavailable as exc:
            with self.engine.begin() as conn:
                conn.execute(update(cache_outbox).where(
                    cache_outbox.c.id == event_id, cache_outbox.c.status != "DONE"
                ).values(attempts=cache_outbox.c.attempts + 1, last_error=type(exc).__name__))
            return False
        with self.engine.begin() as conn:
            conn.execute(update(cache_outbox).where(
                cache_outbox.c.kind == row["kind"], cache_outbox.c.scope == row["scope"],
                cache_outbox.c.revision <= rev
            ).values(status="DONE", completed_at=time.time(), last_error=None))
        return True

    def flush_pending(self, limit=50, *, keys=None):
        """Best-effort synchronous refresh; failure persists for the relay worker."""
        if not self.backend.enabled or not self.loaders:
            return
        owns_mutex = False
        if keys is None:
            if not self._mutex.acquire(blocking=False):
                return
            owns_mutex = True
        try:
            query = select(cache_outbox.c.kind, cache_outbox.c.scope,
                           func.max(cache_outbox.c.revision).label("revision")).where(
                cache_outbox.c.status == "PENDING", cache_outbox.c.retry_at <= time.time())
            if keys:
                query = query.where(tuple_(cache_outbox.c.kind, cache_outbox.c.scope).in_(keys))
            with self.engine.connect() as conn:
                pending = conn.execute(query.group_by(cache_outbox.c.kind, cache_outbox.c.scope).limit(limit)).all()
                ids = []
                for kind, scope, rev in pending:
                    value = conn.scalar(select(cache_outbox.c.id).where(
                        cache_outbox.c.kind == kind, cache_outbox.c.scope == scope,
                        cache_outbox.c.revision == rev, cache_outbox.c.status == "PENDING").limit(1))
                    if value:
                        ids.append(value)
            for event_id in ids:
                try:
                    if not self.refresh_event(event_id):
                        self._enqueue(event_id)
                except Exception as exc:
                    logger.warning("Cache refresh pending event_id=%s error_type=%s", event_id, type(exc).__name__)
        except Exception as exc:
            logger.warning("Cache dispatch unavailable error_type=%s", type(exc).__name__)
        finally:
            if owns_mutex:
                self._mutex.release()

    def _enqueue(self, event_id):
        if self.transport is None or self._queue_retry_at > time.monotonic():
            return
        try:
            self.transport.send(event_id)
            with self.engine.begin() as conn:
                conn.execute(update(cache_outbox).where(
                    cache_outbox.c.id == event_id, cache_outbox.c.status != "DONE"
                ).values(status="QUEUED", retry_at=time.time() + 60))
        except Exception as exc:
            self._queue_retry_at = time.monotonic() + 5
            with self.engine.begin() as conn:
                conn.execute(update(cache_outbox).where(
                    cache_outbox.c.id == event_id, cache_outbox.c.status != "DONE"
                ).values(retry_at=time.time() + 5, last_error=type(exc).__name__))
            logger.warning("Cache queue publish pending event_id=%s error_type=%s", event_id, type(exc).__name__)

    def relay_once(self):
        """Re-publish missing/expired deliveries, including tasks stranded in DLQ."""
        self.flush_pending()
        if self.transport is None or not self.backend.enabled:
            return
        with self.engine.connect() as conn:
            ids = list(conn.scalars(select(cache_outbox.c.id).where(
                cache_outbox.c.status != "DONE", cache_outbox.c.retry_at <= time.time()
            ).order_by(cache_outbox.c.created_at).limit(50)))
        for event_id in ids:
            self._enqueue(event_id)

    def start(self):
        if not self.backend.enabled or self._worker is not None:
            return
        self._stop.clear()
        def run():
            while not self._stop.wait(2):
                try:
                    self.relay_once()
                    if self.transport is not None:
                        self.transport.consume_once(self.refresh_event)
                except Exception as exc:
                    logger.warning("Cache worker will retry error_type=%s", type(exc).__name__)
        self._worker = threading.Thread(target=run, name="state-cache-sync", daemon=True)
        self._worker.start()

    def stop(self):
        self._stop.set()
        if self._worker is not None:
            self._worker.join(timeout=30)
            if not self._worker.is_alive():
                self._worker = None
        if self._worker is None and self.transport is not None:
            self.transport.close()

    def close(self):
        self.stop()
        if self.transport is not None:
            self.transport.close()
        self.backend.close()
