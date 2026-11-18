"""Pytest fixtures for the LightRAG sidecar suite.

The environment is pinned *before* importing :mod:`app.main` so that the
module-level default application (needed by ``uvicorn app.main:app``) writes its
working directory into a temporary folder instead of the repository. Every test
then builds its own application with an injected :class:`FakeEngine`, which is
why the suite passes without ``lightrag-hku`` installed.
"""

from __future__ import annotations

import atexit
import contextlib
import os
import shutil
import sys
import tempfile
from pathlib import Path

SERVICE_ROOT = Path(__file__).resolve().parents[1]
TESTS_ROOT = Path(__file__).resolve().parent
for candidate in (SERVICE_ROOT, TESTS_ROOT):
    if str(candidate) not in sys.path:
        sys.path.insert(0, str(candidate))

_BOOT_WORKDIR = tempfile.mkdtemp(prefix="lightrag-service-boot-")
atexit.register(shutil.rmtree, _BOOT_WORKDIR, True)
os.environ.setdefault("RAG_LIGHTRAG_SERVICE_HOST", "127.0.0.1")
os.environ.setdefault("RAG_LIGHTRAG_SERVICE_WORKING_DIR", _BOOT_WORKDIR)

import pytest  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402

from app.config import Settings  # noqa: E402
from app.main import create_app  # noqa: E402

from helpers import TOKEN, FakeEngine  # noqa: E402


@pytest.fixture()
def fake_engine() -> FakeEngine:
    """Return a fresh in-memory engine double.

    Returns:
        Empty :class:`FakeEngine` instance.
    """

    return FakeEngine()


@pytest.fixture()
def client_factory(tmp_path: Path):
    """Return a factory building test clients over one temporary storage root.

    Args:
        tmp_path: Pytest temporary directory.

    Yields:
        ``build(engine=None, **settings_overrides) -> TestClient``.
    """

    stack = contextlib.ExitStack()

    def build(engine: FakeEngine | None = None, **overrides):
        """Create an application and its test client.

        Args:
            engine: Engine double; a new one is created when omitted.
            **overrides: Settings field overrides (host, token, ...).

        Returns:
            Open :class:`TestClient` bound to the configured application.
        """

        options = {"working_dir": str(tmp_path / "storage")}
        options.update(overrides)
        settings = Settings(**options)
        selected = engine if engine is not None else FakeEngine()
        application = create_app(settings, engine=selected)
        return stack.enter_context(TestClient(application))

    yield build
    stack.close()


@pytest.fixture()
def client(client_factory, fake_engine) -> TestClient:
    """Return a client without authentication and the shared fake engine.

    Returns:
        Unauthenticated test client.
    """

    return client_factory(engine=fake_engine)


@pytest.fixture()
def auth_client(client_factory) -> TestClient:
    """Return a client whose deployment requires a bearer token.

    Returns:
        Test client configured with :data:`helpers.TOKEN`.
    """

    return client_factory(token=TOKEN)
