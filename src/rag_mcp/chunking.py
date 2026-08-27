"""Deterministic text loading and overlapping chunk construction."""

from __future__ import annotations

import hashlib
import re
from dataclasses import dataclass
from pathlib import Path

from .document_readers import SUPPORTED_SUFFIXES, read_document_text

_WHITESPACE = re.compile(r"[ \t]+")
_BREAK_CHARS = frozenset("\n。！？!?；;")


@dataclass(frozen=True)
class DocumentChunk:
    """A stable, storage-neutral unit produced from one source document."""

    document_id: str
    chunk_id: str
    text: str
    title: str
    source: str
    metadata: dict[str, object]


def discover_documents(path: Path) -> list[Path]:
    """Return supported files in deterministic order from a file or directory."""

    resolved = path.expanduser().resolve()
    if resolved.is_file():
        if resolved.suffix.lower() not in SUPPORTED_SUFFIXES:
            raise ValueError(f"Unsupported document type: {resolved.suffix or '<none>'}")
        return [resolved]
    if not resolved.is_dir():
        raise FileNotFoundError(f"Document path does not exist: {resolved}")
    return sorted(
        candidate
        for candidate in resolved.rglob("*")
        if candidate.is_file() and candidate.suffix.lower() in SUPPORTED_SUFFIXES
    )


def chunk_document(
    path: Path,
    *,
    source_root: Path | None = None,
    chunk_size: int = 700,
    overlap: int = 100,
) -> list[DocumentChunk]:
    """Extract one supported document and split it into stable chunks."""

    if chunk_size < 100:
        raise ValueError("chunk_size must be at least 100 characters")
    if overlap < 0 or overlap >= chunk_size:
        raise ValueError("overlap must be non-negative and smaller than chunk_size")

    resolved = path.expanduser().resolve()
    text = _normalize_text(read_document_text(resolved))
    if not text:
        return []
    source = _source_name(resolved, source_root)
    document_id = _stable_id("doc", source.casefold())
    title = _title(text, resolved.stem)
    pieces = _split_text(text, chunk_size=chunk_size, overlap=overlap)
    chunks: list[DocumentChunk] = []
    for index, piece in enumerate(pieces):
        chunk_id = _stable_id("chunk", f"{document_id}\0{index}\0{piece}")
        chunks.append(
            DocumentChunk(
                document_id=document_id,
                chunk_id=chunk_id,
                text=piece,
                title=title,
                source=source,
                metadata={"chunk_index": index, "document_suffix": resolved.suffix.lower()},
            )
        )
    return chunks


def _split_text(text: str, *, chunk_size: int, overlap: int) -> list[str]:
    chunks: list[str] = []
    start = 0
    while start < len(text):
        target_end = min(len(text), start + chunk_size)
        end = target_end
        if target_end < len(text):
            minimum_end = start + max(1, chunk_size // 2)
            for index in range(target_end - 1, minimum_end - 1, -1):
                if text[index] in _BREAK_CHARS:
                    end = index + 1
                    break
        piece = text[start:end].strip()
        if piece:
            chunks.append(piece)
        if end >= len(text):
            break
        next_start = max(start + 1, end - overlap)
        start = next_start
    return chunks


def _normalize_text(text: str) -> str:
    lines = [_WHITESPACE.sub(" ", line).rstrip() for line in text.replace("\r\n", "\n").replace("\r", "\n").split("\n")]
    return "\n".join(lines).strip()


def _source_name(path: Path, source_root: Path | None) -> str:
    if source_root is not None:
        root = source_root.expanduser().resolve()
        try:
            return path.relative_to(root).as_posix()
        except ValueError:
            pass
    return path.name


def _title(text: str, fallback: str) -> str:
    for line in text.splitlines():
        candidate = line.strip().lstrip("#").strip()
        if candidate:
            return candidate[:200]
    return fallback[:200]


def _stable_id(prefix: str, value: str) -> str:
    digest = hashlib.sha256(value.encode("utf-8")).hexdigest()
    return f"{prefix}-{digest[:32]}"
