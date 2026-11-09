"""Connection settings for the shared MySQL state database."""

from __future__ import annotations

import os
from dataclasses import dataclass
from urllib.parse import quote_plus

from sqlalchemy.engine import make_url


class StateConfigurationError(ValueError):
    """Raised when the shared state database is missing or not MySQL."""


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

    @classmethod
    def from_env(cls) -> "StateSettings":
        explicit = os.getenv("RAG_STATE_URL", "").strip()
        url = explicit or _mysql_url_from_parts()
        if not url:
            raise StateConfigurationError(
                "未配置共享状态数据库：请设置 RAG_STATE_URL，"
                "或同时设置 RAG_MYSQL_HOST/RAG_MYSQL_USER/RAG_MYSQL_DATABASE/RAG_MYSQL_PASSWORD"
            )
        return cls(
            url=_require_mysql_url(url),
            encryption_key=os.getenv("RAG_STATE_ENCRYPTION_KEY", "").strip() or None,
            pool_size=max(1, _int_env("RAG_STATE_POOL_SIZE", 10)),
            max_overflow=max(0, _int_env("RAG_STATE_MAX_OVERFLOW", 20)),
            pool_recycle=max(60, _int_env("RAG_STATE_POOL_RECYCLE", 1800)),
            pool_timeout=max(1, _int_env("RAG_STATE_POOL_TIMEOUT", 30)),
            echo=os.getenv("RAG_STATE_ECHO", "").strip().lower() in {"1", "true", "yes", "on"},
            auto_schema=os.getenv("RAG_STATE_AUTO_SCHEMA", "1").strip().lower() in {"1", "true", "yes", "on"},
        )


def _require_mysql_url(url: str) -> str:
    """Reject every backend except MySQL/MariaDB with an actionable message."""

    try:
        backend = make_url(url).get_backend_name()
    except Exception as exc:  # noqa: BLE001 - surfaced as a configuration error
        raise StateConfigurationError("RAG_STATE_URL 不是有效的 SQLAlchemy 连接串") from exc
    if backend not in {"mysql", "mariadb"}:
        raise StateConfigurationError(
            f"共享状态数据库只支持 MySQL/MariaDB，当前 RAG_STATE_URL 使用的是 {backend!r}"
        )
    return url


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


def _int_env(name: str, default: int) -> int:
    raw = os.getenv(name, str(default)).strip()
    try:
        return int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer") from exc
