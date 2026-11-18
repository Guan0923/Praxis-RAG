"""Process-level request limits: a bounded concurrency gate.

LightRAG ingestion fans out many LLM calls and keeps file-backed stores; letting
an unbounded number of HTTP requests in would multiply that fan-out without any
backpressure. :class:`ConcurrencyGate` is the sidecar's single admission point:
at most ``RAG_LIGHTRAG_SERVICE_CONCURRENCY`` engine operations run at once, and
the rest wait their turn inside the ASGI worker.

The gate is bound per event loop because ``asyncio.Semaphore`` is a loop-local
primitive. That matters in tests, where Starlette's ``TestClient`` starts a new
loop per request; production has one loop for the process lifetime.
"""

from __future__ import annotations

import asyncio
import contextlib
import threading
import weakref
from collections.abc import AsyncIterator


class ConcurrencyGate:
    """Bound the number of simultaneous engine operations.

    Attributes:
        limit: Maximum number of concurrent slots (always >= 1).
    """

    def __init__(self, limit: int) -> None:
        """Create a gate with a fixed slot count.

        Args:
            limit: Maximum concurrent operations; must be >= 1.

        Raises:
            ValueError: ``limit`` is smaller than one.
        """

        if limit < 1:
            raise ValueError("concurrency limit must be >= 1")
        self.limit = limit
        self._semaphores: weakref.WeakKeyDictionary = weakref.WeakKeyDictionary()
        self._lock = threading.Lock()

    @contextlib.asynccontextmanager
    async def slot(self) -> AsyncIterator[None]:
        """Hold one concurrency slot for the duration of the block.

        Yields:
            ``None`` once a slot is available. The slot is released even when
            the guarded operation raises or is cancelled.
        """

        semaphore = self._semaphore_for_current_loop()
        async with semaphore:
            yield

    def _semaphore_for_current_loop(self) -> asyncio.Semaphore:
        """Return the semaphore belonging to the running event loop.

        Returns:
            A per-loop semaphore with ``limit`` slots, created on first use.

        Raises:
            RuntimeError: Called outside a running event loop.
        """

        loop = asyncio.get_running_loop()
        with self._lock:
            semaphore = self._semaphores.get(loop)
            if semaphore is None:
                semaphore = asyncio.Semaphore(self.limit)
                self._semaphores[loop] = semaphore
            return semaphore
