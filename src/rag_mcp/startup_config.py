"""Persistent settings for the standalone service entry point."""

from __future__ import annotations

import os
from pathlib import Path
import tomllib

from .config import RagConfigError

DEFAULT_CONFIG = '''# Praxis-RAG service settings. Restart the service after editing.
# Environment variables override these values.
[environment]
RAG_BACKEND = "qdrant"
RAG_SERVICE_HOST = "127.0.0.1"
RAG_SERVICE_PORT = "8011"
RAG_SERVICE_DATA_ROOT = "~/.praxis-rag/data"
QDRANT_URL = "http://127.0.0.1:6333"
RAG_COLLECTION = "mini_agent_knowledge_pdf_assets_v1"
RAG_WORD_CONVERT_TIMEOUT = "120"

# Shared state store. Set RAG_MYSQL_HOST (plus RAG_MYSQL_USER/DATABASE) for a
# MySQL deployment, or RAG_STATE_URL for an explicit SQLAlchemy URL.
# Credentials belong in the environment, not in this file:
#   RAG_MYSQL_PASSWORD, RAG_STATE_ENCRYPTION_KEY
# RAG_MYSQL_HOST = "127.0.0.1"
# RAG_MYSQL_PORT = "3306"
# RAG_MYSQL_USER = "rag"
# RAG_MYSQL_DATABASE = "praxis_rag"

# Set actual local paths to enable DOC conversion and PDF/Word vision.
# RAG_WORD_CONVERTER_PATH = 'C:/Program Files/LibreOffice/program/soffice.exe'
# RAG_PDF_PARSER_PYTHON = 'C:/path/to/parser/python.exe'
# RAG_PDF_MODELS_PATH = 'C:/path/to/docling/models'
'''

_KEYS = frozenset('''RAG_BACKEND RAG_SERVICE_HOST RAG_SERVICE_PORT
RAG_SERVICE_DATA_ROOT RAG_SERVICE_TOKEN RAG_MCP_ALLOWED_HOSTS
RAG_MCP_ALLOWED_ORIGINS QDRANT_URL QDRANT_PATH RAG_COLLECTION
RAG_STATE_URL RAG_MYSQL_HOST RAG_MYSQL_PORT RAG_MYSQL_USER
RAG_MYSQL_DATABASE RAG_MYSQL_SSL_CA RAG_STATE_POOL_SIZE
RAG_STATE_MAX_OVERFLOW RAG_STATE_POOL_RECYCLE RAG_STATE_POOL_TIMEOUT
RAG_RESULT_TTL_SECONDS RAG_RESULT_MAX_BYTES RAG_ARTIFACT_MAX_BYTES RAG_RESULT_MAX_ARTIFACTS
RAG_REDIS_ENABLED RAG_REDIS_URL RAG_REDIS_PREFIX RAG_REDIS_TTL_SECONDS
RAG_REDIS_TIMEOUT_SECONDS RAG_ROCKETMQ_ENDPOINT RAG_ROCKETMQ_TOPIC
RAG_ROCKETMQ_GROUP RAG_ROCKETMQ_NAMESPACE RAG_ROCKETMQ_TLS
RAG_STATE_ECHO RAG_STATE_AUTO_SCHEMA RAG_UPLOAD_LEASE_SECONDS
RAG_UPLOAD_CLEANUP_DELAY_SECONDS
RAG_DOCUMENT_TOPIC RAG_DOCUMENT_GROUP RAG_DOCUMENT_DLQ_GROUP RAG_DOCUMENT_MAX_ATTEMPTS
RAG_DOCUMENT_LEASE_SECONDS RAG_DOCUMENT_RETRY_SECONDS
RAG_DOCUMENT_DISPATCH_LEASE_SECONDS RAG_DOCUMENT_DISPATCH_RETRY_SECONDS
RAG_DOCUMENT_DISPATCH_MAX_RETRY_SECONDS RAG_DOCUMENT_DISPATCH_ALERT_SECONDS
RAG_SEARCH_TOPIC RAG_SEARCH_GROUP RAG_SEARCH_DLQ_GROUP RAG_SEARCH_TIMEOUT_SECONDS
RAG_SEARCH_MAX_ATTEMPTS RAG_SEARCH_LEASE_SECONDS RAG_SEARCH_RETRY_SECONDS
RAG_EMBEDDING_MODEL RAG_EMBEDDING_DEVICE RAG_CHUNK_TOKENIZER_MODEL
RAG_DENSE_VECTOR_SIZE RAG_PRELOAD_MODEL RAG_NAMESPACE RAG_RERANKER_MODEL
RAG_RERANKER_DEVICE RAG_RERANKER_BATCH_SIZE RAG_RERANKER_MAX_CANDIDATES
RAG_RERANKER_MAX_LENGTH RAG_RERANKER_MAX_TEXT_CHARS RAG_PRELOAD_RERANKER
RAG_PDF_PARSER_PYTHON RAG_PDF_MODELS_PATH RAG_PDF_PARSE_TIMEOUT
RAG_WORD_CONVERTER_PATH RAG_WORD_CONVERT_TIMEOUT'''.split())


def load_service_config() -> Path:
    """Create the default file once, then apply validated settings to this process."""
    override = os.getenv("RAG_CONFIG_FILE")
    path = Path(override).expanduser() if override else Path.home() / ".praxis-rag" / "config.toml"
    try:
        if not override:
            path.parent.mkdir(parents=True, exist_ok=True)
            try:
                descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            except FileExistsError:
                pass
            else:
                with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
                    stream.write(DEFAULT_CONFIG)
        with path.open("rb") as stream:
            document = tomllib.load(stream)
    except (OSError, ValueError) as exc:
        raise RagConfigError(f"Cannot read Praxis-RAG config: {path}; check file access and TOML syntax") from exc

    values = document.get("environment")
    if set(document) != {"environment"} or not isinstance(values, dict):
        raise RagConfigError(f"{path}: expected one [environment] section")
    for key, value in values.items():
        if key not in _KEYS or not isinstance(value, str):
            raise RagConfigError(f"{path}: unknown setting or non-string value: {key}")

    # A caller selecting a different database must not inherit the other selector.
    database_override = "QDRANT_URL" in os.environ or "QDRANT_PATH" in os.environ
    for key, value in values.items():
        if database_override and key in {"QDRANT_URL", "QDRANT_PATH"}:
            continue
        os.environ.setdefault(key, value)
    return path
