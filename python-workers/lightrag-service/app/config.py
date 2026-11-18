"""Configuration for the LightRAG sidecar service.

Three concerns live here:

* :class:`Settings` reads the ``RAG_LIGHTRAG_SERVICE_*`` environment once at
  startup and never raises on a bad value: the problem is stored in
  ``config_error`` so ``GET /healthz`` can keep answering 200 (the same
  convention as ``docling-service``). :meth:`Settings.require_startup_security`
  turns that text into a refused startup when the process really cannot run.
* :class:`EmbeddingConfig`, :class:`LLMConfig` and :class:`WorkspaceConfig` are
  the secret-bearing bindings copied from ``POST /v1/workspaces``. They live in
  memory only and are never persisted or logged.
* Small path/identifier validators keep namespace and document ids safe to use
  as directory names and as LightRAG document keys.
"""

from __future__ import annotations

import ipaddress
import os
import re
from dataclasses import dataclass
from pathlib import Path

from . import WORKSPACE_PREFIX
from .errors import ConfigError, InputError

DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 8093
DEFAULT_WORKING_DIR = "rag_storage"
DEFAULT_CONCURRENCY = 2
DEFAULT_MAX_BODY_BYTES = 32 * 1024 * 1024
DEFAULT_OPERATION_TIMEOUT = 900.0

LOG_LEVEL_ENV = "RAG_LIGHTRAG_SERVICE_LOG_LEVEL"
LOG_FORMAT_ENV = "RAG_LIGHTRAG_SERVICE_LOG_FORMAT"

#: Praxis namespace: letters/digits plus ``_ . -``, 1-64 chars, starting and
#: ending with an alphanumeric. This deliberately matches the Go-side KB
#: namespace format but rejects trailing dots/hyphens, which Windows strips from
#: directory names (the namespace is also an on-disk directory component).
NAMESPACE_PATTERN = re.compile(r"^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,62}[A-Za-z0-9])?$")
#: Windows devices names cannot be used as directory names, so a namespace that
#: happens to match one is rejected instead of failing at runtime.
_WINDOWS_RESERVED_NAMES = frozenset(
    {"con", "prn", "aux", "nul"}
    | {f"com{index}" for index in range(1, 10)}
    | {f"lpt{index}" for index in range(1, 10)}
)
#: Praxis document/chunk ids are opaque; only path-hostile characters are banned.
DOCUMENT_ID_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$")

_LOG_LEVELS = {
    "debug": "debug",
    "info": "info",
    "warn": "warning",
    "warning": "warning",
    "error": "error",
}
_LOG_FORMATS = {"text", "json"}
_TRUTHY = {"1", "true", "yes", "on"}


@dataclass(frozen=True)
class EmbeddingConfig:
    """OpenAI-compatible embedding endpoint binding.

    Args:
        base_url: Root URL of the embeddings API (for example
            ``https://api.openai.com/v1``); ``/embeddings`` is appended.
        model: Embedding model name sent in the request body.
        api_key: Bearer credential; empty means "send no Authorization header".
        dimension: Expected vector width; LightRAG validates every batch.
    """

    base_url: str
    model: str
    api_key: str
    dimension: int


@dataclass(frozen=True)
class LLMConfig:
    """OpenAI-compatible chat-completion endpoint binding.

    Args:
        base_url: Root URL of the chat API; ``/chat/completions`` is appended.
        model: Model name sent in the request body.
        api_key: Bearer credential; empty means "send no Authorization header".
        timeout_seconds: Per-call HTTP deadline.
        max_output_tokens: Optional output cap (``max_tokens``); ``None`` leaves
            the parameter out so the upstream default applies.
    """

    base_url: str
    model: str
    api_key: str
    timeout_seconds: float
    max_output_tokens: int | None = None


@dataclass(frozen=True)
class WorkspaceConfig:
    """Everything needed to bind one Praxis namespace to a LightRAG workspace.

    Args:
        namespace: Praxis knowledge base id (``legal``).
        workspace: LightRAG workspace name (``praxis_legal``).
        language: Summary/entity-extraction language forwarded to LightRAG's
            ``addon_params``.
        embedding: Embedding endpoint binding.
        extract_llm: LLM used for entity/relation extraction during ingestion.
        keyword_llm: LLM used for query keyword extraction.
    """

    namespace: str
    workspace: str
    language: str
    embedding: EmbeddingConfig
    extract_llm: LLMConfig
    keyword_llm: LLMConfig


def workspace_name(namespace: str) -> str:
    """Return the LightRAG workspace name for a Praxis namespace.

    Args:
        namespace: Validated Praxis namespace.

    Returns:
        ``praxis_<namespace>``, the value reported in every workspace response.
    """

    return f"{WORKSPACE_PREFIX}{namespace}"


def is_loopback_host(host: str) -> bool:
    """Report whether ``host`` only accepts local connections.

    Args:
        host: Bind address as configured.

    Returns:
        ``True`` for ``localhost``, IPv4/IPv6 loopback literals and the empty
        string (an empty host means "all interfaces", which is not loopback and
        therefore deliberately returns ``False``).
    """

    candidate = (host or "").strip().strip("[]").lower()
    if candidate == "localhost":
        return True
    try:
        return ipaddress.ip_address(candidate).is_loopback
    except ValueError:
        return False


def validate_namespace_name(namespace: str) -> str:
    """Validate a namespace before it becomes a path or LightRAG workspace.

    Args:
        namespace: Candidate namespace from a request body or URL path.

    Returns:
        The unchanged namespace when it matches ``NAMESPACE_PATTERN``.

    Raises:
        InputError: The namespace is empty, too long, contains a path
            separator, or is a Windows device name.
    """

    if not NAMESPACE_PATTERN.fullmatch(namespace or ""):
        raise InputError(
            "namespace 只允许字母、数字、下划线、点和连字符（1-64 位，"
            "首尾为字母或数字）"
        )
    if namespace.lower() in _WINDOWS_RESERVED_NAMES:
        raise InputError("namespace 不能使用 Windows 保留设备名")
    return namespace


def validate_document_id(document_id: str) -> str:
    """Validate a Praxis document id used in URLs, LightRAG keys and metadata.

    Args:
        document_id: Candidate document id.

    Returns:
        The unchanged document id when it matches ``DOCUMENT_ID_PATTERN``.

    Raises:
        InputError: The id is empty, too long or contains unsafe characters.
    """

    if not DOCUMENT_ID_PATTERN.fullmatch(document_id or ""):
        raise InputError("document_id 不可用：只允许字母、数字、点、下划线、冒号和连字符（1-200 位）")
    return document_id


@dataclass(frozen=True)
class Settings:
    """Process configuration read from the ``RAG_LIGHTRAG_SERVICE_*`` variables.

    A misconfigured deployment does not crash the module: the message is kept in
    ``config_error`` for ``/healthz`` and only :meth:`require_startup_security`
    rejects starting uvicorn.

    Args:
        host: Bind address.
        port: Bind port.
        token: Bearer token required on ``/v1/*``; empty disables auth.
        working_dir: Root directory for workspace state, chunk metadata and the
            LightRAG storage of every workspace.
        concurrency: Maximum number of simultaneous engine operations.
        max_body_bytes: Request body limit.
        operation_timeout: Deadline for one HTTP request's engine work.
        log_level: One of debug/info/warning/error.
        log_format: ``text`` or ``json``.
        config_error: Human-readable, secret-free configuration problem.
    """

    host: str = DEFAULT_HOST
    port: int = DEFAULT_PORT
    token: str = ""
    working_dir: str = DEFAULT_WORKING_DIR
    concurrency: int = DEFAULT_CONCURRENCY
    max_body_bytes: int = DEFAULT_MAX_BODY_BYTES
    operation_timeout: float = DEFAULT_OPERATION_TIMEOUT
    log_level: str = "info"
    log_format: str = "text"
    config_error: str | None = None

    def __post_init__(self) -> None:
        """Record the non-loopback-without-token problem for every constructor.

        ``from_env`` already appends the same message; this keeps explicitly
        constructed settings (tests, embedding the service in another process)
        reporting the same ``config_error`` through ``/healthz``.
        """

        if self.config_error is None and not is_loopback_host(self.host) and not self.token:
            object.__setattr__(
                self,
                "config_error",
                "RAG_LIGHTRAG_SERVICE_TOKEN is required when binding a non-loopback address",
            )

    @property
    def workdir(self) -> Path:
        """Return the working directory as an absolute :class:`Path`."""

        return Path(self.working_dir).expanduser().resolve()

    @property
    def auth_required(self) -> bool:
        """Return ``True`` when ``/v1/*`` requests must carry a bearer token."""

        return bool(self.token)

    @classmethod
    def from_env(cls) -> "Settings":
        """Build settings from the environment without raising on bad values.

        Returns:
            A :class:`Settings` instance whose ``config_error`` lists every
            problem found (empty values fall back to the documented defaults).
        """

        errors: list[str] = []

        host = os.environ.get("RAG_LIGHTRAG_SERVICE_HOST", "").strip() or DEFAULT_HOST
        port = _int_env("RAG_LIGHTRAG_SERVICE_PORT", DEFAULT_PORT, minimum=1, maximum=65535, errors=errors)
        token = _token_env(errors=errors)
        working_dir = (
            os.environ.get("RAG_LIGHTRAG_SERVICE_WORKING_DIR", "").strip() or DEFAULT_WORKING_DIR
        )
        concurrency = _int_env(
            "RAG_LIGHTRAG_SERVICE_CONCURRENCY", DEFAULT_CONCURRENCY, minimum=1, maximum=64, errors=errors
        )
        max_body_bytes = _int_env(
            "RAG_LIGHTRAG_SERVICE_MAX_BYTES",
            DEFAULT_MAX_BODY_BYTES,
            minimum=1024,
            maximum=1024 * 1024 * 1024,
            errors=errors,
        )
        operation_timeout = _float_env(
            "RAG_LIGHTRAG_SERVICE_TIMEOUT",
            DEFAULT_OPERATION_TIMEOUT,
            minimum=1.0,
            maximum=86400.0,
            errors=errors,
        )
        log_level = (os.environ.get(LOG_LEVEL_ENV, "").strip().lower() or "info")
        if log_level not in _LOG_LEVELS:
            errors.append(f"{LOG_LEVEL_ENV} must be one of debug, info, warning, error")
            log_level = "info"
        log_format = (os.environ.get(LOG_FORMAT_ENV, "").strip().lower() or "text")
        if log_format not in _LOG_FORMATS:
            errors.append(f"{LOG_FORMAT_ENV} must be one of text, json")
            log_format = "text"

        if not is_loopback_host(host) and not token:
            errors.append(
                "RAG_LIGHTRAG_SERVICE_TOKEN is required when binding a non-loopback address"
            )

        return cls(
            host=host,
            port=port,
            token=token,
            working_dir=working_dir,
            concurrency=concurrency,
            max_body_bytes=max_body_bytes,
            operation_timeout=operation_timeout,
            log_level=log_level,
            log_format=log_format,
            config_error="; ".join(errors) or None,
        )

    def require_startup_security(self) -> None:
        """Refuse to start when the deployment is misconfigured.

        Raises:
            ConfigError: A configuration problem was recorded, or the process
                would bind a non-loopback address without a bearer token.
        """

        if self.config_error:
            raise ConfigError(self.config_error)
        if not is_loopback_host(self.host) and not self.token:
            raise ConfigError(
                "RAG_LIGHTRAG_SERVICE_TOKEN is required when binding a non-loopback address"
            )


def _token_env(*, errors: list[str]) -> str:
    """Read and validate the bearer token, keeping it out of error messages.

    Args:
        errors: Mutable list collecting safe configuration problems.

    Returns:
        The token, or an empty string when auth is disabled.
    """

    token = os.environ.get("RAG_LIGHTRAG_SERVICE_TOKEN", "").strip()
    if not token:
        return ""
    if len(token) > 512 or any(character.isspace() for character in token):
        errors.append("RAG_LIGHTRAG_SERVICE_TOKEN must be a single token without whitespace")
        return ""
    return token


def _int_env(name: str, default: int, *, minimum: int, maximum: int, errors: list[str]) -> int:
    """Parse a bounded integer environment variable.

    Args:
        name: Variable name.
        default: Value used when the variable is unset.
        minimum: Smallest accepted value.
        maximum: Largest accepted value.
        errors: Mutable list collecting safe configuration problems.

    Returns:
        The parsed value or ``default`` when unset/invalid.
    """

    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError:
        errors.append(f"{name} must be an integer")
        return default
    if value < minimum or value > maximum:
        errors.append(f"{name} must be between {minimum} and {maximum}")
        return default
    return value


def _float_env(
    name: str, default: float, *, minimum: float, maximum: float, errors: list[str]
) -> float:
    """Parse a bounded float environment variable.

    Args:
        name: Variable name.
        default: Value used when the variable is unset.
        minimum: Smallest accepted value.
        maximum: Largest accepted value.
        errors: Mutable list collecting safe configuration problems.

    Returns:
        The parsed value or ``default`` when unset/invalid.
    """

    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError:
        errors.append(f"{name} must be a number")
        return default
    if value < minimum or value > maximum:
        errors.append(f"{name} must be between {minimum} and {maximum}")
        return default
    return value


def truthy(value: str | None) -> bool:
    """Interpret the truthy strings used across the repository's services.

    Args:
        value: Environment value to interpret.

    Returns:
        ``True`` for ``1``/``true``/``yes``/``on`` (case-insensitive).
    """

    return (value or "").strip().lower() in _TRUTHY
