"""Persistent, namespace-scoped service credentials."""

from __future__ import annotations

from pathlib import Path

from .state.store import StateStores
from .state.repositories import StateConflict


class ApiKeyError(ValueError):
    pass


class ApiKeyStore:
    """Compatibility facade over the shared API-key repository."""

    def __init__(self, state: StateStores | Path):
        self._owned = state if isinstance(state, StateStores) else StateStores.create(state)
        self.state = self._owned
        self.repository = self._owned.api_keys

    def list(self):
        """Return display-only cached metadata, never authentication authority."""
        return self.state.views.api_keys()

    def create(self, name, namespaces):
        try:
            return self.repository.create(name, namespaces)
        except StateConflict as exc:
            raise ApiKeyError("API Key 已存在") from exc

    def update(self, key_id, name, namespaces, enabled):
        return self.repository.update(key_id, name, namespaces, enabled)

    def delete(self, key_id):
        return self.repository.delete(key_id)

    def namespaces_for_key(self, secret):
        if not isinstance(secret, str) or not secret.startswith("prk_") or len(secret) > 128:
            raise ApiKeyError("访问 API Key 格式错误，需要 prk 开头的服务 API Key")
        try:
            return self.repository.namespaces_for_key(secret)
        except LookupError as exc:
            raise ApiKeyError(str(exc)) from exc
        except PermissionError as exc:
            raise ApiKeyError(str(exc)) from exc

    def secret_for_namespace(self, key_id, namespace):
        return self.repository.secret_for_namespace(key_id, namespace)
