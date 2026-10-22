"""State database settings resolved from the environment."""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import quote_plus

DEFAULT_SQLITE_NAME = "state.sqlite3"


@dataclass(frozen=True)
class StateSettings:
    """Connection settings for the authoritative state database."""

    url: str
    encryption_key: str | None = None
    pool_size: int = 10
    max_overflow: int = 20
    pool_recycle: int = 1800
    pool_timeout: int = 30
    echo: bool = False
    auto_schema: bool = True

    @property
    def is_mysql(self) -> bool:
        return self.url.startswith("mysql")

    @classmethod
    def from_env(cls, data_root: Path | str | None = None) -> "StateSettings":
        explicit = os.getenv("RAG_STATE_URL", "").strip()
        url = explicit or _mysql_url_from_parts() or _sqlite_url(data_root)
        return cls(
            url=url,
            encryption_key=os.getenv("RAG_STATE_ENCRYPTION_KEY", "").strip() or None,
            pool_size=max(1, _int_env("RAG_STATE_POOL_SIZE", 10)),
            max_overflow=max(0, _int_env("RAG_STATE_MAX_OVERFLOW", 20)),
            pool_recycle=max(60, _int_env("RAG_STATE_POOL_RECYCLE", 1800)),
            pool_timeout=max(1, _int_env("RAG_STATE_POOL_TIMEOUT", 30)),
            echo=os.getenv("RAG_STATE_ECHO", "").strip().lower() in {"1", "true", "yes", "on"},
            auto_schema=os.getenv("RAG_STATE_AUTO_SCHEMA", "1").strip().lower() in {"1", "true", "yes", "on"},
        )


def _mysql_url_from_parts() -> str | None:
    host = os.getenv("RAG_MYSQL_HOST", "").strip()
    if not host:
        return None
    user = quote_plus(os.getenv("RAG_MYSQL_USER", "rag").strip() or "rag")
    password = quote_plus(os.getenv("RAG_MYSQL_PASSWORD", ""))
    database = quote_plus(os.getenv("RAG_MYSQL_DATABASE", "praxis_rag").strip() or "praxis_rag")
    port = _int_env("RAG_MYSQL_PORT", 3306)
    query = "charset=utf8mb4"
    ssl_ca = os.getenv("RAG_MYSQL_SSL_CA", "").strip()
    if ssl_ca:
        query += "&ssl_ca=" + quote_plus(ssl_ca)
    return f"mysql+pymysql://{user}:{password}@{host}:{port}/{database}?{query}"


def _sqlite_url(data_root: Path | str | None) -> str:
    root = Path(data_root or Path.home() / ".praxis-rag" / "data").expanduser().resolve()
    root.mkdir(parents=True, exist_ok=True)
    return "sqlite+pysqlite:///" + (root / DEFAULT_SQLITE_NAME).as_posix()


def _int_env(name: str, default: int) -> int:
    raw = os.getenv(name, str(default)).strip()
    try:
        return int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer") from exc
