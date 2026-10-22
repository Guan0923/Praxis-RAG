"""One-directional, repeatable import of legacy file/SQLite state into the database."""

from __future__ import annotations

import argparse
import hashlib
import json
import sqlite3
from pathlib import Path

from ..chunking import _stable_id
from ..model_api import ModelAPIError
from . import crypto
from .schema import PDF_ASSET_BUCKET, SOURCE_BUCKET, WORD_ASSET_BUCKET
from .settings import StateSettings
from .store import StateStores


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Import legacy Praxis-RAG SQLite/JSON/file state into the shared database."
    )
    parser.add_argument("--legacy-root", type=Path, default=Path.home() / ".praxis-rag" / "data")
    parser.add_argument("--apply", action="store_true", help="Write changes; default is a read-only dry run.")
    parser.add_argument("--force", action="store_true", help="Overwrite documents already present in the target.")
    parser.add_argument("--skip-assets", action="store_true", help="Do not copy parsed PDF/Word artifacts.")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    root = args.legacy_root.expanduser().resolve()
    settings = StateSettings.from_env(root)
    state = StateStores.create(settings=settings, create_schema=args.apply)
    summary = {"api_keys": 0, "knowledge_bases": 0, "documents": 0, "blobs": 0, "catalogs": 0}
    errors: list[str] = []
    try:
        summary["api_keys"] = _migrate_api_keys(root, state, settings, args, errors)
        summary["knowledge_bases"] = _migrate_knowledge_bases(root, state, args, errors)
        documents, blobs = _migrate_documents(root, state, args, errors)
        summary["documents"], summary["blobs"] = documents, blobs
        summary["catalogs"] = _migrate_catalogs(root, state, args, errors)
    finally:
        state.close()
    print(json.dumps({"mode": "apply" if args.apply else "dry-run", "root": str(root), **summary},
                     ensure_ascii=False, indent=2))
    for error in errors[:20]:
        print(f"WARN {error}")
    return 1 if errors else 0


def _migrate_api_keys(root: Path, state: StateStores, settings: StateSettings,
                      args: argparse.Namespace, errors: list[str]) -> int:
    path = root / "api-keys.sqlite3"
    if not path.is_file():
        return 0
    imported = 0
    try:
        with sqlite3.connect(path) as db:
            db.row_factory = sqlite3.Row
            columns = {row["name"] for row in db.execute("PRAGMA table_info(api_keys)")}
            rows = list(db.execute("SELECT * FROM api_keys"))
    except (OSError, sqlite3.DatabaseError) as exc:
        errors.append(f"api-keys: cannot read {path.name}: {type(exc).__name__}")
        return 0
    for row in rows:
        try:
            protected = row["protected_key"] if "protected_key" in columns else None
            if args.apply:
                state.api_keys.import_row(
                    key_id=row["id"], name=row["name"], prefix=row["prefix"], digest=row["digest"],
                    namespaces=json.loads(row["namespaces"]), enabled=bool(row["enabled"]),
                    created_at=row["created_at"], protected_key=protected,
                )
            imported += 1
        except (ValueError, ModelAPIError) as exc:
            errors.append(f"api-keys: {row['id']} needs the original OS account to decrypt: {exc}")
    return imported


def _migrate_knowledge_bases(root: Path, state: StateStores, args: argparse.Namespace,
                             errors: list[str]) -> int:
    path = root / "knowledge-bases.sqlite3"
    if not path.is_file():
        return 0
    imported = 0
    try:
        with sqlite3.connect(path) as db:
            rows = list(db.execute("SELECT settings FROM knowledge_bases"))
    except (OSError, sqlite3.DatabaseError) as exc:
        errors.append(f"knowledge-bases: cannot read {path.name}: {type(exc).__name__}")
        return 0
    for (raw,) in rows:
        try:
            settings = json.loads(raw)
            if not isinstance(settings, dict):
                raise ValueError("knowledge-base settings must be an object")
            settings = _reprotect_settings(settings)
            if args.apply:
                existing = state.knowledge_bases.get(settings["namespace"])
                if existing is None:
                    state.knowledge_bases.create(settings)
                else:
                    state.knowledge_bases.update(settings)
            imported += 1
        except (KeyError, ValueError, ModelAPIError) as exc:
            errors.append(f"knowledge-bases: {settings.get('namespace', '?')}: {exc}")
    return imported


def _reprotect_settings(settings: dict) -> dict:
    result = dict(settings)
    for kind in ("embedding", "reranker"):
        provider = dict(result.get(kind) or {})
        protected = provider.get("protected_key")
        if protected:
            provider["protected_key"] = crypto.protect(crypto.unprotect(protected))
        result[kind] = provider
    return result


def _migrate_documents(root: Path, state: StateStores, args: argparse.Namespace,
                       errors: list[str]) -> tuple[int, int]:
    documents = blobs = 0
    for manifest_path in sorted(root.glob("*/manifest.json")):
        namespace = manifest_path.parent.name
        knowledge = manifest_path.parent / "knowledge"
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            errors.append(f"{namespace}: manifest unreadable: {type(exc).__name__}")
            continue
        if not isinstance(manifest, dict):
            errors.append(f"{namespace}: manifest is not an object")
            continue
        for name, record in manifest.items():
            if not isinstance(record, dict):
                continue
            source = knowledge / name
            if not source.is_file():
                errors.append(f"{namespace}/{name}: source file missing")
                continue
            content = source.read_bytes()
            document_id = str(record.get("document_id") or _stable_id("doc", name.casefold()))
            if args.apply:
                existing = state.documents.get(namespace, name)
                if (not args.force and existing is not None
                        and state.blobs.exists(namespace, SOURCE_BUCKET, name)):
                    continue
                migrated = {
                    **record,
                    "name": name,
                    "size": len(content),
                    "sha256": record.get("sha256") or hashlib.sha256(content).hexdigest(),
                    "document_id": document_id,
                }
                with state.engine.begin() as conn:
                    state.blobs.put(namespace, SOURCE_BUCKET, name, content,
                                    document_id=document_id, conn=conn)
                    state.documents.upsert(namespace, migrated, conn=conn)
                    if not args.skip_assets:
                        blobs += _copy_assets(state, namespace, knowledge, document_id, conn)
            documents += 1
            blobs += 1
    return documents, blobs


def _copy_assets(state: StateStores, namespace: str, knowledge: Path, document_id: str, conn=None) -> int:
    copied = 0
    for directory, bucket in ((".pdf-assets", PDF_ASSET_BUCKET), (".word-assets", WORD_ASSET_BUCKET)):
        folder = knowledge / directory / document_id
        if folder.is_dir():
            copied += state.blobs.put_tree(namespace, bucket, document_id, folder, conn=conn)
    return copied


def _migrate_catalogs(root: Path, state: StateStores, args: argparse.Namespace, errors: list[str]) -> int:
    imported = 0
    for path in sorted(root.glob("*/mcp_catalog.json")):
        namespace = path.parent.name
        try:
            raw = path.read_text(encoding="utf-8")
            json.loads(raw)
        except (OSError, json.JSONDecodeError) as exc:
            errors.append(f"{namespace}: MCP catalog unreadable: {type(exc).__name__}")
            continue
        if args.apply:
            state.catalogs.mutate(namespace, lambda _current: (raw, None, True))
        imported += 1
    return imported


if __name__ == "__main__":
    raise SystemExit(main())
