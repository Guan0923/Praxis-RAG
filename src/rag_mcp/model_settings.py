"""OS-protected credentials and provider connection validation."""

from __future__ import annotations

from .model_api import ModelAPIError, ModelConnection, normalize_base_url
from .state.crypto import protect as _portable_protect
from .state.crypto import unprotect as _portable_unprotect

KINDS = ("embedding", "reranker")


def _protect(value: str) -> str:
    return _portable_protect(value)


def _unprotect(value: str) -> str:
    return _portable_unprotect(value)


def draft_connection(current: ModelConnection, values: dict[str, object]) -> ModelConnection:
    base_url = normalize_base_url(str(values.get("base_url", current.base_url)))
    key = values.get("apikey", current.api_key if base_url == current.base_url else "")
    if not isinstance(key, str) or any(char in key for char in "\r\n"):
        raise ModelAPIError("apikey 格式无效")
    return ModelConnection(base_url=base_url, api_key=key)
