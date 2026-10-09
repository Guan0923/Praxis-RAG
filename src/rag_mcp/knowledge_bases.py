"""Knowledge-base configuration and protected per-base provider credentials."""

import json
import os
from pathlib import Path
import sqlite3

from .model_api import ModelConnection
from .model_settings import KINDS, _protect, _unprotect


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
    def __init__(self, root: Path):
        self.path = root / "knowledge-bases.sqlite3"

    def list(self) -> list[dict]:
        if not self.path.exists():
            return []
        db = sqlite3.connect(self.path)
        try:
            return [json.loads(row[0]) for row in db.execute("SELECT settings FROM knowledge_bases ORDER BY namespace")]
        finally:
            db.close()

    def get(self, namespace: str) -> dict | None:
        if not self.path.exists():
            return None
        db = sqlite3.connect(self.path)
        try:
            row = db.execute("SELECT settings FROM knowledge_bases WHERE namespace = ?", (namespace,)).fetchone()
            return json.loads(row[0]) if row else None
        finally:
            db.close()

    def create(self, settings: dict) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.path.touch(mode=0o600, exist_ok=True)
        if os.name != "nt":
            self.path.chmod(0o600)
        db = sqlite3.connect(self.path)
        try:
            with db:
                db.execute("CREATE TABLE IF NOT EXISTS knowledge_bases (namespace TEXT PRIMARY KEY, settings TEXT NOT NULL)")
                db.execute("INSERT INTO knowledge_bases VALUES (?, ?)",
                           (settings["namespace"], json.dumps(settings, ensure_ascii=False)))
        finally:
            db.close()

    def update(self, settings: dict) -> None:
        if os.name != "nt":
            self.path.chmod(0o600)
        db = sqlite3.connect(self.path)
        try:
            with db:
                changed = db.execute("UPDATE knowledge_bases SET settings = ? WHERE namespace = ?",
                                     (json.dumps(settings, ensure_ascii=False), settings["namespace"])).rowcount
                if not changed:
                    raise LookupError("知识库不存在")
        finally:
            db.close()
