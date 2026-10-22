"""PDF-only asset parsing and public evidence projection."""

from __future__ import annotations

import base64
import json
import os
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .chunking import DocumentChunk, _stable_id
from .chunk_strategies import ChunkingConfig, LEGACY_CONFIG
from .pdf_content import PDF_PARSER_VERSION, content_chunks, render_content
from .state.schema import PDF_ASSET_BUCKET, WORD_ASSET_BUCKET
from .state.store import StateStores
from .workspace import cleanup_workspace, create_workspace, run_bounded


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
    captions: list[dict] = field(default_factory=list)
    footnotes: list[dict] = field(default_factory=list)

    def payload(self, document_id: str, source: str) -> dict[str, object]:
        return {
            "record_type": "asset", "document_id": document_id, "asset_id": self.asset_id,
            "asset_kind": self.kind, "source": source, "page": self.page, "bbox": self.bbox,
            "caption": self.caption, "caption_source": self.caption_source,
            "generated_description": self.generated_description, "image_file": self.image_file,
            "table": self.table,
            "captions": self.captions, "footnotes": self.footnotes,
        }


@dataclass(frozen=True)
class ParsedPdf:
    chunks: list[DocumentChunk]
    assets: list[PdfAsset]
    staging_dir: Path


def parse_pdf(path: Path, *, source_name: str, config: ChunkingConfig | None,
              tokenizer_model: str, parser_python: Path | None, models_path: Path | None,
              timeout_seconds: int, workspace: Path | None = None, trace=None) -> ParsedPdf:
    """Run Docling out of process and turn its manifest into index chunks."""

    document_id = _stable_id("doc", source_name.casefold())
    managed = workspace is not None
    staging = workspace or create_workspace("rag-pdf-")
    trace = trace or (lambda _name, action: action())
    staging.mkdir(parents=True, exist_ok=True)
    manifest = staging / "manifest.json"
    executable = parser_python or Path(sys.executable)
    command = [str(executable), "-m", "rag_mcp.pdf_worker", "--input", str(path),
               "--output", str(staging), "--manifest", str(manifest)]
    if models_path is not None:
        command.extend(["--models", str(models_path)])
    environment = os.environ.copy()
    environment["PYTHONPATH"] = str(Path(__file__).resolve().parents[1]) + os.pathsep + environment.get("PYTHONPATH", "")
    environment["PYTHONUTF8"] = "1"
    def convert():
        result = run_bounded(command, timeout=timeout_seconds, env=environment)
        if result.returncode != 0 or not manifest.is_file():
            raise ValueError(f"PDF 解析失败：{source_name}")
        raw = json.loads(manifest.read_text(encoding="utf-8"))
        if raw.get("version") != PDF_PARSER_VERSION:
            raise ValueError("PDF parser version mismatch; reimport this document")
        return raw

    try:
        raw = trace("parse", convert)
    except subprocess.TimeoutExpired as exc:
        if not managed:
            cleanup_workspace(staging)
        raise ValueError(f"PDF 解析超过 {timeout_seconds} 秒：{path.name}") from exc
    except OSError as exc:
        if not managed:
            cleanup_workspace(staging)
        raise ValueError(f"PDF parser could not start: {type(exc).__name__}") from exc
    except Exception:
        if not managed:
            cleanup_workspace(staging)
        raise
    try:
        raw_assets = raw["assets"]
        def render():
            (staging / "document.md").write_text(render_content(raw), encoding="utf-8")
        trace("render_artifacts", render)
        for item in raw_assets:
            item["asset_id"] = f"{document_id}:{item['asset_id']}"
            if item.get("image_file"):
                item["image_file"] = f"{document_id}/{item['image_file']}"
        for block in raw["blocks"]:
            if block.get("asset_id"):
                block["asset_id"] = f"{document_id}:{block['asset_id']}"
        assets = [_asset_from_manifest(item) for item in raw_assets]
        chunks = trace("chunking_tokenizer", lambda: content_chunks(
            raw, document_id, source_name, Path(source_name).stem, config or LEGACY_CONFIG, tokenizer_model))
        if not chunks:
            raise ValueError(f"PDF 没有可索引的内容：{path.name}")
        return ParsedPdf(chunks, assets, staging)
    except Exception:
        if not managed:
            cleanup_workspace(staging)
        raise


def project_results(candidates, store, state: "StateStores", namespace: str) -> list[dict[str, object]]:
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
        if candidate.metadata.get("upload_version") != asset.get("upload_version"):
            continue
        common: dict[str, object] = {
            "rank": rank, "document_id": candidate.document_id, "chunk_id": candidate.chunk_id,
            "title": candidate.title, "source": candidate.source, "score": round(candidate.score, 6),
            "metadata": {**candidate.metadata, "asset_id": str(asset_id), "asset_kind": asset.get("asset_kind"),
                         "page": asset.get("page"), "bbox": asset.get("bbox")},
        }
        caption = str(asset.get("caption") or "")
        common["metadata"].update(captions=asset.get("captions", []), footnotes=asset.get("footnotes", []),
                                  caption_source=asset.get("caption_source", "none"))
        if asset.get("asset_storage") == "word":
            common["metadata"].pop("page", None)
            common["metadata"].pop("bbox", None)
            common["metadata"]["location"] = asset.get("location")
        if asset.get("asset_kind") == "picture":
            relative = asset.get("image_file")
            if not isinstance(relative, str):
                continue
            bucket = WORD_ASSET_BUCKET if asset.get("asset_storage") == "word" else PDF_ASSET_BUCKET
            version = candidate.metadata.get("upload_version")
            content = (state.uploads.get_object(version, bucket, relative) if version else
                       state.blobs.get(namespace, bucket, relative))
            if content is None:
                continue
            common["asset"] = {"type": "image", "mime_type": asset.get("mime_type") or "image/png",
                               "data": base64.b64encode(content).decode("ascii"),
                               "caption": caption, "page": asset.get("page"), "bbox": asset.get("bbox")}
        else:
            table = asset.get("table")
            if not isinstance(table, dict):
                continue
            common["asset"] = {"type": "table", "caption": caption, "page": asset.get("page"),
                               "bbox": asset.get("bbox"), "rows": table.get("rows", []),
                               "headers": table.get("headers", []),
                               "header_rows": table.get("header_rows", []),
                               "cells": table.get("cells", []), "num_rows": table.get("num_rows", 0),
                               "num_cols": table.get("num_cols", 0), "footnotes": table.get("footnotes", [])}
            if asset.get("asset_storage") == "word":
                common["asset"].update(nested_tables=table.get("nested_tables", {}), parent_cell=table.get("parent_cell"))
        if asset.get("asset_storage") == "word":
            common["asset"].pop("page", None)
            common["asset"].pop("bbox", None)
            common["asset"]["location"] = asset.get("location")
        results.append(common)
    return results


def _asset_from_manifest(item: dict[str, Any]) -> PdfAsset:
    return PdfAsset(str(item["asset_id"]), str(item["kind"]), int(item["page"]),
                    dict(item.get("bbox") or {}), str(item.get("caption") or ""),
                    str(item.get("caption_source") or "none"),
                    str(item.get("generated_description") or ""),
                    str(item["image_file"]) if item.get("image_file") else None,
                    dict(item["table"]) if isinstance(item.get("table"), dict) else None,
                    list(item.get("captions", [])), list(item.get("footnotes", [])))


def read_pdf_content(state: "StateStores", namespace: str, source_name: str,
                     chunking: dict[str, object]) -> str:
    if chunking.get("pdf_parser_version") != PDF_PARSER_VERSION:
        raise ValueError("PDF parser version changed; explicitly reimport this PDF")
    document_id = _stable_id("doc", source_name.casefold())
    try:
        text = state.blobs.get_text(namespace, PDF_ASSET_BUCKET, f"{document_id}/document.md")
    except (OSError, UnicodeDecodeError) as exc:
        raise ValueError("Saved PDF content is unavailable; explicitly reimport this PDF") from exc
    if text is None:
        raise ValueError("Saved PDF content is unavailable; explicitly reimport this PDF")
    return text
