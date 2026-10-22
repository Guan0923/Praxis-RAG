"""Knowledge-base configuration and protected per-base provider credentials."""

from __future__ import annotations

from pathlib import Path

from .model_api import ModelConnection
from .model_settings import KINDS, _protect, _unprotect
from .state.store import StateStores
from .state.repositories import StateConflict


def stored_connection(connection: ModelConnection) -> dict:
    return {"base_url": connection.base_url, "protected_key": _protect(connection.api_key)}


def model_connection(settings: dict, kind: str) -> ModelConnection:
    provider = settings.get(kind, {})
    return ModelConnection(base_url=provider.get("base_url", ""), model=settings[f"{kind}_model"],
                           api_key=_unprotect(provider.get("protected_key", "")),
                           vector_size=settings["vector_size"] if kind == "embedding" else None)


def public_settings(settings: dict) -> dict:
    return {**{key: value for key, value in settings.items() if key not in KINDS},
            **{kind: {"base_url": settings.get(kind, {}).get("base_url", ""),
                      "api_key_set": bool(settings.get(kind, {}).get("protected_key"))}
               for kind in KINDS}}


class KnowledgeBaseStore:
    """Compatibility facade over the shared knowledge-base repository."""

    def __init__(self, state: StateStores | Path):
        self._owned = state if isinstance(state, StateStores) else StateStores.create(state)
        self.state = self._owned
        self.repository = self._owned.knowledge_bases

    def list(self) -> list[dict]:
        return self.repository.list()

    def get(self, namespace: str) -> dict | None:
        return self.repository.get(namespace)

    def create(self, settings: dict) -> None:
        try:
            self.repository.create(settings)
        except StateConflict as exc:
            raise LookupError("该知识库已存在") from exc

    def update(self, settings: dict) -> None:
        self.repository.update(settings)

    def delete(self, namespace: str) -> bool:
        return self.repository.delete(namespace)
