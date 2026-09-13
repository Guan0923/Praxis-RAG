"""Immutable creation settings for namespace-owned knowledge bases."""

import json
from pathlib import Path
import sqlite3


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
        db = sqlite3.connect(self.path)
        try:
            with db:
                db.execute("CREATE TABLE IF NOT EXISTS knowledge_bases (namespace TEXT PRIMARY KEY, settings TEXT NOT NULL)")
                db.execute("INSERT INTO knowledge_bases VALUES (?, ?)",
                           (settings["namespace"], json.dumps(settings, ensure_ascii=False)))
        finally:
            db.close()
