"""Persistent, namespace-scoped service credentials."""

from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import secrets
import sqlite3


class ApiKeyError(ValueError):
    pass

from .model_settings import _protect, _unprotect


class ApiKeyStore:
    def __init__(self, data_root: Path):
        self.path = data_root / "api-keys.sqlite3"

    @contextmanager
    def connection(self):
        self.path.parent.mkdir(parents=True, exist_ok=True)
        descriptor = os.open(self.path, os.O_CREAT | os.O_RDWR, 0o600)
        os.close(descriptor)
        if os.name != "nt":
            self.path.chmod(0o600)
        db = sqlite3.connect(self.path, timeout=10)
        db.row_factory = sqlite3.Row
        try:
            with db:
                db.execute("BEGIN IMMEDIATE")
                db.execute("""CREATE TABLE IF NOT EXISTS api_keys (
                    id TEXT PRIMARY KEY, name TEXT NOT NULL, prefix TEXT NOT NULL,
                    digest TEXT UNIQUE NOT NULL, namespaces TEXT NOT NULL,
                    enabled INTEGER NOT NULL, created_at TEXT NOT NULL, protected_key TEXT
                )""")
                if "protected_key" not in {row["name"] for row in db.execute("PRAGMA table_info(api_keys)")}:
                    db.execute("ALTER TABLE api_keys ADD COLUMN protected_key TEXT")
                yield db
        finally:
            db.close()

    @staticmethod
    def public(row):
        return {"id": row["id"], "name": row["name"], "prefix": row["prefix"],
                "namespaces": json.loads(row["namespaces"]), "enabled": bool(row["enabled"]),
                "created_at": row["created_at"], "exportable": bool(row["protected_key"])}

    def list(self):
        with self.connection() as db:
            return [self.public(row) for row in db.execute("SELECT * FROM api_keys ORDER BY created_at DESC")]

    def create(self, name, namespaces):
        secret = "prk_" + secrets.token_urlsafe(32)
        key_id = secrets.token_hex(12)
        with self.connection() as db:
            db.execute("INSERT INTO api_keys VALUES (?, ?, ?, ?, ?, ?, ?, ?)", (
                key_id, name, secret[:12], hashlib.sha256(secret.encode()).hexdigest(),
                json.dumps(namespaces), 1, datetime.now(timezone.utc).isoformat(), _protect(secret),
            ))
            item = self.public(db.execute("SELECT * FROM api_keys WHERE id = ?", (key_id,)).fetchone())
        return {**item, "key": secret}

    def update(self, key_id, name, namespaces, enabled):
        with self.connection() as db:
            changed = db.execute("UPDATE api_keys SET name = ?, namespaces = ?, enabled = ? WHERE id = ?",
                                 (name, json.dumps(namespaces), int(enabled), key_id)).rowcount
            if not changed:
                return None
            return self.public(db.execute("SELECT * FROM api_keys WHERE id = ?", (key_id,)).fetchone())

    def delete(self, key_id):
        with self.connection() as db:
            return bool(db.execute("DELETE FROM api_keys WHERE id = ?", (key_id,)).rowcount)

    def namespaces_for_key(self, secret):
        if not secret.startswith("prk_") or len(secret) > 128:
            raise ApiKeyError("访问 API Key 格式错误，需要 prk_ 开头的服务 API Key")
        with self.connection() as db:
            row = db.execute("SELECT namespaces, enabled FROM api_keys WHERE digest = ?",
                             (hashlib.sha256(secret.encode()).hexdigest(),)).fetchone()
            if row is None:
                raise ApiKeyError("访问 API Key 在当前服务中不存在或已删除")
            if not row["enabled"]:
                raise ApiKeyError("访问 API Key 已停用")
            return json.loads(row["namespaces"])

    def secret_for_namespace(self, key_id, namespace):
        with self.connection() as db:
            row = db.execute("SELECT * FROM api_keys WHERE id = ?", (key_id,)).fetchone()
            if row is None:
                raise LookupError("API Key 不存在")
            if not row["enabled"] or namespace not in json.loads(row["namespaces"]):
                raise PermissionError("API Key 已停用或未获授权访问此 Namespace")
            if not row["protected_key"]:
                raise ValueError("该密钥仅保存摘要，无法显示原值；请在 API Key 页面重新创建")
            return _unprotect(row["protected_key"])
