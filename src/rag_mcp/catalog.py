"""Namespace-local tool settings and inert prompt templates."""

from __future__ import annotations

import re
from pathlib import Path
from threading import RLock
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, StrictBool, StrictStr, model_validator

from .service_knowledge import KnowledgeError
from .state.repositories import StateError
from .state.store import StateStores

NAME_PATTERN = r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$"
_PLACEHOLDER = re.compile(r"\{\{([A-Za-z0-9_.-]+)\}\}")


class ToolPatch(BaseModel):
    model_config = ConfigDict(extra="forbid")
    enabled: StrictBool | None = None
    description: StrictStr | None = Field(default=None, max_length=8000)

    @model_validator(mode="after")
    def non_null_changes(self):
        if not self.model_fields_set or any(getattr(self, key) is None for key in self.model_fields_set):
            raise ValueError("Provide enabled and/or description; null is not accepted")
        return self


class PromptArgument(BaseModel):
    model_config = ConfigDict(extra="forbid")
    name: str = Field(pattern=NAME_PATTERN)
    description: str = Field(default="", max_length=2000)
    required: bool = False


class PromptMessage(BaseModel):
    model_config = ConfigDict(extra="forbid")
    role: Literal["user", "assistant"]
    text: str = Field(max_length=32000)


class PromptDefinition(BaseModel):
    model_config = ConfigDict(extra="forbid")
    description: str = Field(default="", max_length=8000)
    arguments: list[PromptArgument] = Field(default_factory=list, max_length=32)
    messages: list[PromptMessage] = Field(min_length=1, max_length=32)

    @model_validator(mode="after")
    def check_placeholders(self):
        names = [argument.name for argument in self.arguments]
        if len(set(names)) != len(names):
            raise ValueError("Prompt arguments must have unique names")
        for message in self.messages:
            if set(_PLACEHOLDER.findall(message.text)) - set(names):
                raise ValueError("Prompt placeholders must refer to declared arguments")
            remaining = _PLACEHOLDER.sub("", message.text)
            if "{{" in remaining or "}}" in remaining:
                raise ValueError("Use {{argument}} for prompt placeholders")
        return self

    def render(self, arguments: dict[str, str]) -> list[dict[str, str]]:
        names = {argument.name for argument in self.arguments}
        if set(arguments) - names or any(not isinstance(value, str) for value in arguments.values()):
            raise ValueError("Unknown or non-string prompt argument")
        missing = [argument.name for argument in self.arguments if argument.required and argument.name not in arguments]
        if missing:
            raise ValueError("Missing prompt arguments: " + ", ".join(missing))
        return [
            {"role": message.role, "text": _PLACEHOLDER.sub(lambda match: arguments.get(match[1], ""), message.text)}
            for message in self.messages
        ]


class CatalogData(BaseModel):
    model_config = ConfigDict(extra="forbid")
    tools: dict[str, dict[str, StrictBool | StrictStr]] = Field(default_factory=dict)
    prompts: dict[str, PromptDefinition] = Field(default_factory=dict)

    @model_validator(mode="after")
    def validate_saved_settings(self):
        for name, settings in self.tools.items():
            if not re.fullmatch(NAME_PATTERN, name):
                raise ValueError("Invalid registered tool name")
            ToolPatch.model_validate(settings)
        for name in self.prompts:
            if not re.fullmatch(NAME_PATTERN, name):
                raise ValueError("Invalid prompt name")
        return self


class NamespaceCatalog:
    """One namespace's tool/prompt settings stored in the shared database."""

    def __init__(self, state: StateStores, namespace: str) -> None:
        self.state = state
        self.namespace = namespace
        self.lock = RLock()

    def snapshot(self, *, authoritative: bool = False) -> CatalogData:
        """Read the public catalog, or bypass cache for execution-time permission checks."""
        with self.lock:
            try:
                raw = (self.state.catalogs.load(self.namespace) if authoritative
                       else self.state.views.catalog(self.namespace))
            except StateError as exc:
                raise KnowledgeError("MCP configuration cannot be read") from exc
            if raw is None:
                return CatalogData()
            try:
                return CatalogData.model_validate_json(raw)
            except ValueError as exc:
                raise KnowledgeError("MCP configuration cannot be read") from exc

    def _mutate(self, transform):
        with self.lock:
            def apply(raw: str | None):
                try:
                    data = CatalogData() if raw is None else CatalogData.model_validate_json(raw)
                except ValueError as exc:
                    raise KnowledgeError("MCP configuration cannot be read") from exc
                changed, result = transform(data)
                return data.model_dump_json(indent=2), result, changed

            try:
                return self.state.catalogs.mutate(self.namespace, apply)
            except StateError as exc:
                raise KnowledgeError("MCP configuration cannot be saved") from exc

    def patch_tool(self, name: str, patch: ToolPatch, defaults: dict[str, object]) -> bool:
        def apply(data: CatalogData):
            previous = {**defaults, **data.tools.get(name, {})}
            updated = {**previous, **patch.model_dump(exclude_unset=True)}
            if previous == updated:
                return False, False
            data.tools[name] = updated
            return True, True

        return bool(self._mutate(apply))

    def put_prompt(self, name: str, prompt: PromptDefinition) -> bool:
        self._validate_name(name)

        def apply(data: CatalogData):
            if data.prompts.get(name) == prompt:
                return False, False
            data.prompts[name] = prompt
            return True, True

        return bool(self._mutate(apply))

    def delete_prompt(self, name: str) -> bool:
        self._validate_name(name)

        def apply(data: CatalogData):
            if name not in data.prompts:
                raise KeyError(name)
            del data.prompts[name]
            return True, True

        return bool(self._mutate(apply))

    @staticmethod
    def _validate_name(name: str) -> None:
        if not re.fullmatch(NAME_PATTERN, name):
            raise ValueError("Invalid prompt name")


class CatalogStore:
    def __init__(self, state: StateStores | Path) -> None:
        self._owned = state if isinstance(state, StateStores) else StateStores.create(state)
        self.state = self._owned
        self._catalogs: dict[str, NamespaceCatalog] = {}
        self._lock = RLock()

    def for_namespace(self, namespace: str) -> NamespaceCatalog:
        if not re.fullmatch(NAME_PATTERN, namespace):
            raise ValueError("Invalid namespace")
        with self._lock:
            if namespace not in self._catalogs:
                self._catalogs[namespace] = NamespaceCatalog(self.state, namespace)
            return self._catalogs[namespace]
