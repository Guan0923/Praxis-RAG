"""PDF-only asset parsing and public evidence projection."""

from __future__ import annotations

import base64
import json
import os
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .chunking import DocumentChunk, _source_name, _stable_id, _title
from .chunk_strategies import ChunkingConfig, LEGACY_CONFIG


@dataclass(frozen=True)
class PdfAsset:
    asset_id: str
    kind: str
    page: int
    bbox: dict[str, object]
    caption: str
    caption_source: str
    generated_description: str
    image_file: str | None
    table: dict[str, object] | None

    def payload(self, document_id: str, source: str) -> dict[str, object]:
        return {
            "record_type": "asset", "document_id": document_id, "asset_id": self.asset_id,
            "asset_kind": self.kind, "source": source, "page": self.page, "bbox": self.bbox,
            "caption": self.caption, "caption_source": self.caption_source,
            "generated_description": self.generated_description, "image_file": self.image_file,
            "table": self.table,
        }


@dataclass(frozen=True)
class ParsedPdf:
    chunks: list[DocumentChunk]
    assets: list[PdfAsset]
    staging_dir: Path


def parse_pdf(path: Path, *, source_root: Path, config: ChunkingConfig | None,
              tokenizer_model: str, parser_python: Path | None, models_path: Path | None,
              timeout_seconds: int) -> ParsedPdf:
    """Run Docling out of process and turn its manifest into index chunks."""

    source = _source_name(path, source_root)
    document_id = _stable_id("doc", source.casefold())
    assets_root = source_root / ".pdf-assets"
    assets_root.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=f".{document_id}-", dir=assets_root))
    manifest = staging / "manifest.json"
    executable = parser_python or Path(sys.executable)
    command = [str(executable), "-m", "rag_mcp.pdf_worker", "--input", str(path),
               "--output", str(staging), "--manifest", str(manifest)]
    if models_path is not None:
        command.extend(["--models", str(models_path)])
    environment = os.environ.copy()
    environment["PYTHONPATH"] = str(Path(__file__).resolve().parents[1]) + os.pathsep + environment.get("PYTHONPATH", "")
    environment["PYTHONUTF8"] = "1"
    try:
        result = subprocess.run(command, capture_output=True, text=True, encoding="utf-8", errors="replace",
                                timeout=timeout_seconds,
                                env=environment, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    except subprocess.TimeoutExpired as exc:
        shutil.rmtree(staging, ignore_errors=True)
        raise ValueError(f"PDF 解析超过 {timeout_seconds} 秒：{path.name}") from exc
    if result.returncode != 0 or not manifest.is_file():
        shutil.rmtree(staging, ignore_errors=True)
        detail = (result.stderr or result.stdout or "PDF parser exited without a manifest").strip()[-800:]
        raise ValueError(f"PDF 解析失败：{path.name}；{detail}")
    try:
        raw = json.loads(manifest.read_text(encoding="utf-8"))
        blocks, raw_assets = raw["blocks"], raw["assets"]
    except (OSError, json.JSONDecodeError, KeyError, TypeError) as exc:
        shutil.rmtree(staging, ignore_errors=True)
        raise ValueError(f"PDF 解析结果无效：{path.name}") from exc
    for item in raw_assets:
        item["asset_id"] = f"{document_id}:{item['asset_id']}"
        if item.get("image_file"):
            item["image_file"] = f"{document_id}/{item['image_file']}"
    assets = [_asset_from_manifest(item) for item in raw_assets]
    chunks = _manifest_chunks(blocks, assets, document_id, source, path.stem,
                              config or LEGACY_CONFIG, tokenizer_model)
    if not chunks:
        shutil.rmtree(staging, ignore_errors=True)
        raise ValueError(f"PDF 没有可索引的正文、图片说明或表格内容：{path.name}")
    return ParsedPdf(chunks, assets, staging)


def install_assets(parsed: ParsedPdf, source_root: Path, document_id: str) -> tuple[Path, Path | None]:
    final = source_root / ".pdf-assets" / document_id
    backup = None
    if final.exists():
        backup = final.with_name(f".{final.name}.backup")
        shutil.rmtree(backup, ignore_errors=True)
        os.replace(final, backup)
    os.replace(parsed.staging_dir, final)
    return final, backup


def rollback_assets(final: Path, backup: Path | None) -> None:
    shutil.rmtree(final, ignore_errors=True)
    if backup is not None and backup.exists():
        os.replace(backup, final)


def finish_assets(backup: Path | None) -> None:
    if backup is not None:
        shutil.rmtree(backup, ignore_errors=True)


def project_results(candidates, store, assets_root: Path) -> list[dict[str, object]]:
    """Hydrate ranked asset hits while keeping generated descriptions private."""

    asset_ids = [str(c.metadata["asset_id"]) for c in candidates if c.metadata.get("asset_id")]
    assets = store.get_assets(asset_ids)
    results: list[dict[str, object]] = []
    for rank, candidate in enumerate(candidates, start=1):
        asset_id = candidate.metadata.get("asset_id")
        if not asset_id:
            results.append(candidate.to_result(rank))
            continue
        asset = assets.get(str(asset_id))
        if asset is None:
            continue
        common: dict[str, object] = {
            "rank": rank, "document_id": candidate.document_id, "chunk_id": candidate.chunk_id,
            "title": candidate.title, "source": candidate.source, "score": round(candidate.score, 6),
            "metadata": {"asset_id": str(asset_id), "asset_kind": asset.get("asset_kind"),
                         "page": asset.get("page"), "bbox": asset.get("bbox")},
        }
        caption = str(asset.get("caption") or "")
        if asset.get("asset_kind") == "picture":
            relative = asset.get("image_file")
            if not isinstance(relative, str):
                continue
            image = (assets_root / relative).resolve()
            if assets_root.resolve() not in image.parents or not image.is_file():
                continue
            common["asset"] = {"type": "image", "mime_type": "image/png",
                               "data": base64.b64encode(image.read_bytes()).decode("ascii"),
                               "caption": caption, "page": asset.get("page"), "bbox": asset.get("bbox")}
        else:
            table = asset.get("table")
            if not isinstance(table, dict):
                continue
            common["asset"] = {"type": "table", "caption": caption, "page": asset.get("page"),
                               "bbox": asset.get("bbox"), "rows": table.get("rows", []),
                               "cells": table.get("cells", []), "num_rows": table.get("num_rows", 0),
                               "num_cols": table.get("num_cols", 0), "footnotes": table.get("footnotes", [])}
        results.append(common)
    return results


def _asset_from_manifest(item: dict[str, Any]) -> PdfAsset:
    return PdfAsset(str(item["asset_id"]), str(item["kind"]), int(item["page"]),
                    dict(item.get("bbox") or {}), str(item.get("caption") or ""),
                    str(item.get("caption_source") or "none"),
                    str(item.get("generated_description") or ""),
                    str(item["image_file"]) if item.get("image_file") else None,
                    dict(item["table"]) if isinstance(item.get("table"), dict) else None)


def _manifest_chunks(blocks, assets, document_id, source, fallback_title, config, tokenizer_model):
    texts = [str(block.get("text") or "").strip() for block in blocks]
    title = _title("\n".join(texts), fallback_title)
    base = {"document_suffix": ".pdf", "chunking": config.metadata(tokenizer_model)}
    entries: list[tuple[str, dict[str, object]]] = []
    for block in blocks:
        text = str(block.get("text") or "").strip()
        if text:
            entries.append((text, {"page": block.get("page"), "kind": block.get("kind", "paragraph")}))
    for asset in assets:
        if asset.kind == "picture":
            text = asset.caption or asset.generated_description
            if text:
                entries.append((text, {"asset_id": asset.asset_id, "asset_kind": "picture", "page": asset.page}))
        elif asset.table:
            caption = asset.caption or asset.generated_description
            headers = [str(value) for value in asset.table.get("headers", []) if str(value).strip()]
            for row_number, row in enumerate(asset.table.get("rows", [])):
                values = [str(value) for value in row]
                text = "\n".join(part for part in [caption, " | ".join(headers), " | ".join(values)] if part)
                if text.strip():
                    entries.append((text, {"asset_id": asset.asset_id, "asset_kind": "table",
                                           "page": asset.page, "table_row": row_number}))
    chunks = []
    for index, (text, extra) in enumerate(entries):
        chunks.append(DocumentChunk(document_id, _stable_id("chunk", f"{document_id}\0{index}\0{text}"),
                                    text, title, source, {"chunk_index": index, **base, **extra}))
    return chunks
