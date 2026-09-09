"""Word asset staging, indexing and saved content reads."""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

from .chunking import DocumentChunk, _source_name, _stable_id, _split_ranges
from .chunk_strategies import ChunkingConfig, LEGACY_CONFIG, TokenBudget, get_tokenizer, split_content
from .document_readers import DocumentContent, SourceBlock, SourceSpan
from .word_converter import word_source
from .word_reader import WORD_PARSER_VERSION, parse_docx, render_word


@dataclass(frozen=True)
class WordAsset:
    data: dict

    def payload(self, document_id, source):
        return {**self.data, "record_type": "asset", "document_id": document_id, "source": source,
                "asset_kind": self.data["kind"], "asset_storage": "word"}


@dataclass(frozen=True)
class ParsedWord:
    chunks: list[DocumentChunk]
    assets: list[WordAsset]
    staging_dir: Path
    diagnostics: list[dict]
    skipped_object_count: int
    assets_subdir: str = ".word-assets"


def word_chunks(manifest, document_id, source, title, config, tokenizer_model):
    tokenizer = get_tokenizer(tokenizer_model)
    maximum = min(int(tokenizer.model_max_length), 8192) - tokenizer.num_special_tokens_to_add(pair=False)
    budget = TokenBudget(tokenizer, config if config.strategy != "character" else ChunkingConfig("token", maximum, 0))
    assets = {a["asset_id"]: a for a in manifest["assets"]}
    chunks, pending = [], []

    def append(text, metadata):
        if not text.strip():
            return
        count = budget.count(text)
        if count > maximum:
            raise ValueError(f"Word item exceeds model input limit ({maximum} tokens): {metadata.get('refs')}")
        index = len(chunks)
        chunks.append(DocumentChunk(document_id, _stable_id("chunk", f"{document_id}\0{index}\0{text}"), text,
                                    title, source, {"document_suffix": Path(source).suffix.lower(),
                                    "chunk_index": index, "chunking": config.metadata(tokenizer_model),
                                    "word_parser_version": WORD_PARSER_VERSION, "token_count": count, **metadata}))

    def flush():
        if not pending:
            return
        text, spans = "", []
        for block in pending:
            if text:
                text += "\n\n"
            spans.append(SourceSpan(len(text), len(text)+len(block["text"]),
                         SourceBlock(block["text"], block["kind"], headings=tuple(block["location"]["headings"])), len(spans)))
            text += block["text"]
        document = DocumentContent(text, spans)
        ranges = _split_ranges(text, chunk_size=config.chunk_size, overlap=config.chunk_overlap) if config.strategy == "character" else split_content(document, ".docx", config, budget)
        equations = list(re.finditer(r"\$[^$]+\$", text))
        emitted = set()
        for start, end in ranges:
            # Never split an inline equation in the middle.
            for match in equations:
                if match.start() < start < match.end():
                    start = match.start()
                if match.start() < end < match.end():
                    end = match.end()
            if (start, end) in emitted:
                continue
            emitted.add((start, end))
            covered = [block for block, span in zip(pending, spans, strict=True) if span.start < end and span.end > start]
            append(text[start:end], {"refs": [b["ref"] for b in covered],
                                     "locations": [b["location"] for b in covered], "kind": "text"})
        pending.clear()

    def row_text(row):
        return " | ".join(str(cell).replace("|", r"\|").replace("\n", " ") for cell in row)

    for block in manifest["blocks"]:
        asset = assets.get(block.get("asset_id"))
        if not asset:
            if block.get("associated_assets") and block["kind"] in {"caption", "note"}:
                continue
            if block["text"] and not block.get("in_cell"):
                pending.append(block)
            continue
        flush()
        metadata = {"kind": asset["kind"], "asset_id": asset["asset_id"], "asset_kind": asset["kind"],
                    "refs": [block["ref"]], "location": block["location"]}
        caption = asset["caption"] or asset.get("generated_description", "")
        notes = "\n".join(note["text"] for note in asset["footnotes"])
        if asset["kind"] == "picture":
            append("\n".join(t for t in (caption, notes) if t), metadata)
        else:
            table = asset["table"]
            headers = "\n".join(row_text(row) for row in table["header_rows"])
            for row_number, row in enumerate(table["rows"]):
                append("\n".join(t for t in (caption, headers, row_text(row), notes) if t),
                       {**metadata, "table_row": row_number})
    flush()
    return chunks


def parse_word(path, *, source_root, config, tokenizer_model, converter=None, convert_timeout=120,
               parser_python=None, models_path=None, timeout_seconds=900):
    source = _source_name(path, source_root)
    document_id = _stable_id("doc", source.casefold())
    assets_root = source_root / ".word-assets"
    assets_root.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=f".{document_id}-", dir=assets_root))
    try:
        with word_source(path, converter, convert_timeout) as converted:
            raw = parse_docx(converted, staging)
        manifest = staging / "manifest.json"
        manifest.write_text(json.dumps(raw, ensure_ascii=False), encoding="utf-8")
        if any(asset["kind"] == "picture" and not asset["caption"] for asset in raw["assets"]):
            environment = os.environ.copy()
            environment["PYTHONPATH"] = str(Path(__file__).resolve().parents[1]) + os.pathsep + environment.get("PYTHONPATH", "")
            environment["PYTHONUTF8"] = "1"
            command = [str(parser_python or sys.executable), "-m", "rag_mcp.word_vision_worker", "--manifest", str(manifest)]
            if models_path:
                command.extend(["--models", str(models_path)])
            result = subprocess.run(command, capture_output=True, text=True, encoding="utf-8", errors="replace",
                                    timeout=timeout_seconds, env=environment,
                                    creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
            if result.returncode:
                detail = (result.stderr or result.stdout).strip().splitlines()[-1][-400:]
                raise ValueError(f"Word image description failed: {detail}")
            raw = json.loads(manifest.read_text(encoding="utf-8"))
        ids = {a["asset_id"]: f"{document_id}:{a['asset_id']}" for a in raw["assets"]}
        for asset in raw["assets"]:
            asset["asset_id"] = ids[asset["asset_id"]]
            if asset["image_file"]:
                asset["image_file"] = f"{document_id}/{asset['image_file']}"
            if asset["table"]:
                parent = asset["table"].get("parent_cell")
                if parent:
                    parent["asset_id"] = ids[parent["asset_id"]]
                for cell in asset["table"]["cells"]:
                    cell["nested_tables"] = [ids[a] for a in cell["nested_tables"] if a in ids]
        for block in raw["blocks"]:
            if block.get("asset_id"):
                block["asset_id"] = ids[block["asset_id"]]
            block["associated_assets"] = [ids[a] for a in block.get("associated_assets", []) if a in ids]
        for asset in raw["assets"]:
            if asset["table"]:
                asset["table"]["caption"] = asset["caption"]
                asset["table"]["nested_tables"] = {a["asset_id"]: a["table"] for a in raw["assets"]
                    if a["table"] and (a["table"].get("parent_cell") or {}).get("asset_id") == asset["asset_id"]}
        chunks = word_chunks(raw, document_id, source, path.stem, config or LEGACY_CONFIG, tokenizer_model)
        if not chunks:
            raise ValueError(f"Word document has no indexable content: {path.name}")
        manifest.write_text(json.dumps(raw, ensure_ascii=False, indent=2), encoding="utf-8")
        (staging / "document.md").write_text(render_word(raw), encoding="utf-8")
        return ParsedWord(chunks, [WordAsset(a) for a in raw["assets"]], staging, raw["diagnostics"], raw["skipped_object_count"])
    except (subprocess.TimeoutExpired, OSError) as exc:
        shutil.rmtree(staging, ignore_errors=True)
        raise ValueError(f"Word parsing failed: {path.name}: {exc}") from exc
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        raise


def read_word_content(path, source_root):
    document_id = _stable_id("doc", _source_name(path.resolve(), source_root.resolve()).casefold())
    folder = source_root / ".word-assets" / document_id
    try:
        manifest = json.loads((folder / "manifest.json").read_text(encoding="utf-8"))
        if manifest.get("version") != WORD_PARSER_VERSION:
            raise ValueError("Word parser changed; reimport this document")
        return (folder / "document.md").read_text(encoding="utf-8")
    except (OSError, json.JSONDecodeError) as exc:
        raise ValueError("Saved Word content unavailable; reimport this document") from exc
