"""Explicit, non-destructive migration from legacy Backend-owned RAG sources."""

from __future__ import annotations

import argparse
import hashlib
from pathlib import Path

from .config import RagConfig
from .http_service import RagHttpSettings
from .service_knowledge import MAX_FILES_PER_BATCH, SUPPORTED_EXTENSIONS, RagServiceRuntime


def namespace_for_legacy_user(user_id: str) -> str:
    digest = hashlib.sha256(user_id.encode("utf-8")).hexdigest()[:16]
    return f"user-{digest}"


def discover_legacy_sources(root: Path, selected_users: set[str]) -> list[tuple[str, list[Path]]]:
    """Find preserved supported sources without changing either store."""

    result: list[tuple[str, list[Path]]] = []
    if not root.is_dir():
        return result
    for user_root in sorted(
        (path for path in root.iterdir() if path.is_dir() and not path.is_symlink()),
        key=lambda path: path.name,
    ):
        if selected_users and user_root.name not in selected_users:
            continue
        knowledge = user_root / "rag" / "knowledge"
        if not knowledge.is_dir() or knowledge.is_symlink():
            continue
        files = sorted(
            (
                path
                for path in knowledge.iterdir()
                if path.is_file() and not path.is_symlink() and path.suffix.casefold() in SUPPORTED_EXTENSIONS
            ),
            key=lambda path: path.name.casefold(),
        )
        if files:
            result.append((user_root.name, files))
    return result


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Copy legacy Mini-Agent knowledge into RAG Service storage.")
    parser.add_argument("--legacy-root", type=Path, default=Path.home() / ".mini_agent")
    parser.add_argument("--user-id", action="append", default=[], help="Restrict migration to one user; repeatable.")
    parser.add_argument(
        "--apply",
        action="store_true",
        help="Perform copy + reindex. Without this flag the command is read-only preview mode.",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    sources = discover_legacy_sources(args.legacy_root.expanduser().resolve(), set(args.user_id))
    total = sum(len(files) for _user_id, files in sources)
    print(f"legacy users={len(sources)} files={total} mode={'apply' if args.apply else 'preview'}")
    for user_id, files in sources:
        print(f"{user_id}: {len(files)} file(s) -> {namespace_for_legacy_user(user_id)}")
    if not args.apply or not sources:
        return 0

    settings = RagHttpSettings.from_env()
    runtime = RagServiceRuntime(RagConfig.from_env(), settings.data_root)
    failed = 0
    for user_id, files in sources:
        namespace = namespace_for_legacy_user(user_id)
        for offset in range(0, len(files), MAX_FILES_PER_BATCH):
            batch = files[offset : offset + MAX_FILES_PER_BATCH]
            handles = [path.open("rb") for path in batch]
            try:
                result = runtime.upload(
                    namespace,
                    [(path.name, handle) for path, handle in zip(batch, handles, strict=True)],
                )
            finally:
                for handle in handles:
                    handle.close()
            failed += len(result["failed"])
            print(f"{user_id}: imported={len(result['imported'])} failed={len(result['failed'])}")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
