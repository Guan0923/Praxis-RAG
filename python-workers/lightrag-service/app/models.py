"""Request models and validation for the LightRAG sidecar HTTP contract.

Pydantic enforces the JSON shape once, at the edge, so the service layer only
sees typed values. ``extra="forbid"`` keeps typos from being silently ignored,
which matters because this contract is called by the Go backend and any drift
should fail loudly during integration.

Two rules are easy to get wrong and are implemented here on purpose:

* chunk text is **not** stripped: the sidecar stores the exact text Praxis sent
  so evidence quotes stay byte-identical to the Go-side chunk;
* query text must weigh at least three English-equivalent characters (CJK
  characters count double), mirroring LightRAG's own RAG query minimum so a
  too-short query is rejected with 400 instead of failing inside the engine.
"""

from __future__ import annotations

from urllib.parse import urlsplit

from pydantic import BaseModel, ConfigDict, Field, field_validator

from .config import (
    DOCUMENT_ID_PATTERN,
    NAMESPACE_PATTERN,
    EmbeddingConfig,
    LLMConfig,
    WorkspaceConfig,
    validate_namespace_name,
    workspace_name,
)
from .engine import SUPPORTED_MODES
from .errors import InputError

#: Unicode blocks LightRAG counts double when enforcing its query minimum.
_WIDE_RANGES: tuple[tuple[int, int], ...] = (
    (0x1100, 0x11FF),
    (0x2E80, 0x33FF),
    (0x3400, 0x4DBF),
    (0x4E00, 0x9FFF),
    (0xA960, 0xA97F),
    (0xAC00, 0xD7FF),
    (0xF900, 0xFAFF),
    (0xFE30, 0xFE4F),
    (0xFF00, 0xFFEE),
    (0x1AFF0, 0x1B2FF),
    (0x20000, 0x3FFFD),
)

_MIN_QUERY_WEIGHT = 3

#: LightRAG's summary/entity-extraction prompt takes a human language name.
#: The Go client sends short codes ("zh"/"en"); map the common ones and pass
#: every other value through unchanged.
_LANGUAGE_ALIASES = {
    "zh": "Chinese",
    "zh-cn": "Chinese",
    "zh-hans": "Chinese",
    "chinese": "Chinese",
    "en": "English",
    "en-us": "English",
    "en-gb": "English",
    "english": "English",
}

_DEFAULT_TOP_K = 10


def _validate_base_url(value: str) -> str:
    """Validate an OpenAI-compatible API root URL.

    Args:
        value: Candidate base URL.

    Returns:
        The unchanged URL.

    Raises:
        ValueError: The URL is not an absolute ``http(s)`` URL.
    """

    candidate = value.strip()
    if len(candidate) > 2048 or any(character.isspace() for character in candidate):
        raise ValueError("base_url must be an absolute http(s) URL")
    parts = urlsplit(candidate)
    if parts.scheme not in {"http", "https"} or not parts.netloc:
        raise ValueError("base_url must be an absolute http(s) URL")
    return candidate


def _validate_api_key(value: str) -> str:
    """Reject credentials that cannot travel in an HTTP header safely.

    Args:
        value: Candidate API key; an empty string is allowed for local model
            servers that ignore auth.

    Returns:
        The unchanged key.

    Raises:
        ValueError: The key is too long or contains CR/LF.
    """

    candidate = value.strip()
    if len(candidate) > 4096:
        raise ValueError("api_key is too long")
    if "\r" in candidate or "\n" in candidate:
        raise ValueError("api_key must not contain line breaks")
    return candidate


def query_weight(text: str) -> int:
    """Score query text the way LightRAG's minimum-length check does.

    Args:
        text: Query text.

    Returns:
        Sum of character weights: East Asian wide characters count 2, other
        non-space characters count 1, whitespace counts 0.
    """

    total = 0
    for character in text:
        if character.isspace():
            continue
        code = ord(character)
        total += 2 if any(low <= code <= high for low, high in _WIDE_RANGES) else 1
    return total


class _RequestModel(BaseModel):
    """Base class: reject unknown fields so contract drift fails with 400."""

    model_config = ConfigDict(extra="forbid")


class EmbeddingSpec(_RequestModel):
    """Embedding endpoint of one workspace binding.

    Attributes:
        base_url: OpenAI-compatible API root.
        model: Embedding model name.
        api_key: Bearer credential; empty sends no Authorization header.
        dimension: Expected vector width (Praxis currently uses 1024).
    """

    base_url: str = Field(min_length=8, max_length=2048)
    model: str = Field(min_length=1, max_length=200)
    api_key: str = Field(default="", max_length=4096)
    dimension: int = Field(default=1024, ge=1, le=8192)

    @field_validator("base_url")
    @classmethod
    def _check_base_url(cls, value: str) -> str:
        """Validate the embedding URL; see :func:`_validate_base_url`."""

        return _validate_base_url(value)

    @field_validator("api_key")
    @classmethod
    def _check_api_key(cls, value: str) -> str:
        """Validate the embedding credential; see :func:`_validate_api_key`."""

        return _validate_api_key(value)


class ExtractLLMSpec(_RequestModel):
    """LLM used for entity/relation extraction while chunks are ingested.

    Attributes:
        base_url: OpenAI-compatible API root.
        model: Chat model name.
        api_key: Bearer credential; empty sends no Authorization header.
        max_output_tokens: Output cap forwarded as ``max_tokens``.
        timeout_seconds: Per-call HTTP deadline.
    """

    base_url: str = Field(min_length=8, max_length=2048)
    model: str = Field(min_length=1, max_length=200)
    api_key: str = Field(default="", max_length=4096)
    max_output_tokens: int = Field(default=9000, ge=1, le=200000)
    timeout_seconds: float = Field(default=300.0, gt=0, le=3600.0)

    @field_validator("base_url")
    @classmethod
    def _check_base_url(cls, value: str) -> str:
        """Validate the extraction URL; see :func:`_validate_base_url`."""

        return _validate_base_url(value)

    @field_validator("api_key")
    @classmethod
    def _check_api_key(cls, value: str) -> str:
        """Validate the extraction credential; see :func:`_validate_api_key`."""

        return _validate_api_key(value)


class KeywordLLMSpec(_RequestModel):
    """LLM used for query keyword extraction (retrieval only, no answers).

    Attributes:
        base_url: OpenAI-compatible API root.
        model: Chat model name.
        api_key: Bearer credential; empty sends no Authorization header.
        timeout_seconds: Per-call HTTP deadline.
    """

    base_url: str = Field(min_length=8, max_length=2048)
    model: str = Field(min_length=1, max_length=200)
    api_key: str = Field(default="", max_length=4096)
    timeout_seconds: float = Field(default=60.0, gt=0, le=3600.0)

    @field_validator("base_url")
    @classmethod
    def _check_base_url(cls, value: str) -> str:
        """Validate the keyword URL; see :func:`_validate_base_url`."""

        return _validate_base_url(value)

    @field_validator("api_key")
    @classmethod
    def _check_api_key(cls, value: str) -> str:
        """Validate the keyword credential; see :func:`_validate_api_key`."""

        return _validate_api_key(value)


class WorkspaceRequest(_RequestModel):
    """Body of ``POST /v1/workspaces``.

    Attributes:
        namespace: Praxis knowledge base id.
        config_fingerprint: Caller-computed hash of the binding; a different
            value rebuilds (wipes) the workspace. Treated as an opaque token
            because the Go side computes a plain SHA-256 hex digest.
        language: Extraction language forwarded to LightRAG; ``zh``/``en`` (and
            their variants) are normalized to ``Chinese``/``English``.
        embedding: Embedding endpoint binding.
        extract_llm: Extraction LLM binding.
        keyword_llm: Keyword-extraction LLM binding.
    """

    namespace: str = Field(pattern=NAMESPACE_PATTERN.pattern)
    config_fingerprint: str = Field(pattern=r"^[A-Za-z0-9._:-]{1,256}$")
    language: str = Field(default="Chinese", min_length=1, max_length=64)
    embedding: EmbeddingSpec
    extract_llm: ExtractLLMSpec
    keyword_llm: KeywordLLMSpec

    @field_validator("language")
    @classmethod
    def _normalize_language(cls, value: str) -> str:
        """Map short language codes to LightRAG's language names."""

        candidate = value.strip()
        if not candidate:
            raise ValueError("language must not be empty")
        return _LANGUAGE_ALIASES.get(candidate.lower(), candidate)

    @field_validator("namespace")
    @classmethod
    def _check_namespace(cls, value: str) -> str:
        """Apply the shared namespace rules, including reserved device names."""

        try:
            return validate_namespace_name(value)
        except InputError as error:
            raise ValueError(str(error)) from error

    def to_config(self) -> WorkspaceConfig:
        """Convert the validated request into the engine-facing binding.

        Returns:
            A :class:`~app.config.WorkspaceConfig` carrying the credential-bearing
            endpoint bindings; the caller must never persist or log it.
        """

        return WorkspaceConfig(
            namespace=self.namespace,
            workspace=workspace_name(self.namespace),
            language=self.language,
            embedding=EmbeddingConfig(
                base_url=self.embedding.base_url,
                model=self.embedding.model,
                api_key=self.embedding.api_key,
                dimension=self.embedding.dimension,
            ),
            extract_llm=LLMConfig(
                base_url=self.extract_llm.base_url,
                model=self.extract_llm.model,
                api_key=self.extract_llm.api_key,
                timeout_seconds=float(self.extract_llm.timeout_seconds),
                max_output_tokens=self.extract_llm.max_output_tokens,
            ),
            keyword_llm=LLMConfig(
                base_url=self.keyword_llm.base_url,
                model=self.keyword_llm.model,
                api_key=self.keyword_llm.api_key,
                timeout_seconds=float(self.keyword_llm.timeout_seconds),
                max_output_tokens=None,
            ),
        )


class ChunkInput(_RequestModel):
    """One already-split Praxis chunk. The sidecar never re-splits this text.

    Attributes:
        chunk_id: Praxis chunk identifier, echoed back in query contexts.
        text: Exact chunk text (not stripped).
        page: Source page number, ``None`` when the source has no pages.
        heading: Section heading path captured by the Go splitter.
        upload_version: Praxis upload version the chunk came from.
    """

    chunk_id: str = Field(min_length=1, max_length=200)
    text: str = Field(default="", max_length=200000)
    page: int | None = Field(default=None, ge=0, le=10_000_000)
    heading: str = Field(default="", max_length=500)
    upload_version: int | None = Field(default=None, ge=0, le=1_000_000_000)

    @field_validator("chunk_id")
    @classmethod
    def _check_chunk_id(cls, value: str) -> str:
        """Reject chunk ids with control characters or surrounding whitespace."""

        if value != value.strip() or any(ord(character) < 32 for character in value):
            raise ValueError("chunk_id must not contain control characters")
        return value


class ChunksRequest(_RequestModel):
    """Body of ``POST /v1/workspaces/{namespace}/chunks``.

    Attributes:
        document_id: Praxis document id; re-posting the same id replaces the
            document's chunks.
        document_name: Original document name, echoed in query contexts; an
            empty value is allowed and the Go side falls back to the id.
        chunks: Ordered pre-split chunks (up to 10000 per request); JSON ``null``
            is treated as an empty list, matching the Go client's zero value.
    """

    source_hash: str = Field(default="", pattern=r"^(?:[0-9a-f]{64})?$")
    publication_version: str = Field(default="", max_length=64)
    expected_revision: str | None = Field(default=None, max_length=128)
    document_id: str = Field(pattern=DOCUMENT_ID_PATTERN.pattern)
    document_name: str = Field(default="", max_length=500)
    chunks: list[ChunkInput] = Field(default_factory=list, max_length=10000)

    @field_validator("document_name")
    @classmethod
    def _check_document_name(cls, value: str) -> str:
        """Reject control characters in the document name."""

        if any(ord(character) < 32 for character in value):
            raise ValueError("document_name must not contain control characters")
        return value

    @field_validator("chunks", mode="before")
    @classmethod
    def _null_chunks_as_empty(cls, value):
        """Treat JSON ``null`` as an empty chunk list."""

        return [] if value is None else value

    @field_validator("chunks")
    @classmethod
    def _check_unique_chunk_ids(cls, value: list[ChunkInput]) -> list[ChunkInput]:
        """Reject duplicate chunk ids inside one request."""

        seen: set[str] = set()
        for chunk in value:
            if chunk.chunk_id in seen:
                raise ValueError("duplicate chunk_id in chunks")
            seen.add(chunk.chunk_id)
        return value


class QueryFilters(_RequestModel):
    """Optional retrieval filters.

    Attributes:
        document_ids: Restrict contexts to these documents; empty (or JSON
            ``null``, which the Go client sends for its nil slice) means all
            documents of the namespace.
    """

    document_ids: list[str] = Field(default_factory=list, max_length=100)

    @field_validator("document_ids", mode="before")
    @classmethod
    def _null_ids_as_empty(cls, value):
        """Treat JSON ``null`` as "no restriction"."""

        return [] if value is None else value

    @field_validator("document_ids")
    @classmethod
    def _check_document_ids(cls, value: list[str]) -> list[str]:
        """Validate every filter id with the document-id pattern."""

        for document_id in value:
            if not DOCUMENT_ID_PATTERN.fullmatch(document_id):
                raise ValueError("invalid document_id in filters")
        return value


class QueryRequest(_RequestModel):
    """Body of ``POST /v1/workspaces/{namespace}/query``.

    Attributes:
        query: Retrieval query text (never logged).
        mode: LightRAG retrieval mode; empty means ``mix`` (the Go client sends
            an empty string when it has no override).
        top_k: Maximum number of contexts and retrieval width; ``0``/``null``
            means the default of 10.
        filters: Optional document restriction; ``null`` means unrestricted.
        include_entities: Attach per-context entity/relation evidence.
    """

    query: str = Field(min_length=1, max_length=8000)
    mode: str = Field(default="mix", max_length=32)
    top_k: int = Field(default=_DEFAULT_TOP_K, ge=1, le=100)
    filters: QueryFilters = Field(default_factory=QueryFilters)
    include_entities: bool = True

    @field_validator("query")
    @classmethod
    def _check_query(cls, value: str) -> str:
        """Trim the query and enforce LightRAG's minimum retrieval weight."""

        candidate = value.strip()
        if query_weight(candidate) < _MIN_QUERY_WEIGHT:
            raise ValueError("query is too short for retrieval")
        return candidate

    @field_validator("mode", mode="before")
    @classmethod
    def _normalize_mode(cls, value):
        """Default an empty mode to ``mix`` and reject unknown modes."""

        candidate = "mix" if value is None else str(value).strip()
        if not candidate:
            candidate = "mix"
        if candidate not in SUPPORTED_MODES:
            raise ValueError("unsupported retrieval mode")
        return candidate

    @field_validator("top_k", mode="before")
    @classmethod
    def _default_top_k(cls, value):
        """Treat ``0``/``null`` as the default width, matching Go zero values."""

        if value is None or value == 0:
            return _DEFAULT_TOP_K
        return value

    @field_validator("filters", mode="before")
    @classmethod
    def _null_filters(cls, value):
        """Treat JSON ``null`` filters as an empty filter object."""

        return {} if value is None else value
