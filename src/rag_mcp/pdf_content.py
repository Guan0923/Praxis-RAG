"""Ordered PDF content shared by indexing and resource reads."""

from __future__ import annotations

import re

from .chunking import DocumentChunk, _split_ranges, _stable_id, _title
from .chunk_strategies import ChunkingConfig, TokenBudget, get_tokenizer, source_metadata, split_content
from .document_readers import DocumentContent, SourceBlock, SourceSpan, MAX_EXTRACTED_CHARACTERS

PDF_PARSER_VERSION = "docling-formula-ordered-v2"
FORMULA_BLOCK = re.compile(r"(?m)^\$\$[ \t]*\n[\s\S]*?^\$\$[ \t]*(?:\n|$)")


def table_row_text(row) -> str:
    return " | ".join(str(cell).replace("\n", " ").replace("|", r"\|") for cell in row)


def render_content(manifest: dict) -> str:
    assets = {item["asset_id"]: item for item in manifest["assets"]}
    parts = []
    for block in manifest["blocks"]:
        if block.get("asset_id"):
            asset = assets[block["asset_id"]]
            if asset["kind"] == "table":
                parts.append("\n".join(table_row_text(row) for row in asset["table"]["rows"]))
            else:
                parts.append(f"[Image: {block['ref']}]")
        elif block.get("text"):
            parts.append(block["text"])
    text = "\n\n".join(parts)
    if len(text) > MAX_EXTRACTED_CHARACTERS:
        raise ValueError("Parsed PDF exceeds the extracted text limit")
    return text


def content_chunks(manifest: dict, document_id: str, source: str, fallback_title: str,
                   config: ChunkingConfig, tokenizer_model: str) -> list[DocumentChunk]:
    tokenizer = get_tokenizer(tokenizer_model)
    maximum = min(int(tokenizer.model_max_length), 8192) - tokenizer.num_special_tokens_to_add(pair=False)
    budget = TokenBudget(tokenizer, config if config.strategy != "character" else ChunkingConfig("token", maximum, 0))
    assets = {item["asset_id"]: item for item in manifest["assets"]}
    title = _title("\n".join(block.get("text", "") for block in manifest["blocks"]), fallback_title)
    base = {"document_suffix": ".pdf", "chunking": config.metadata(tokenizer_model),
            "pdf_parser_version": PDF_PARSER_VERSION}
    result = []
    pending = []

    def append(text, metadata):
        if not text.strip():
            return
        count = budget.count(text)
        if count > maximum:
            raise ValueError(f"PDF item exceeds model input limit ({maximum} tokens): {metadata.get('refs')}")
        index = len(result)
        pages = metadata.get("pages", [metadata.get("page")])
        warnings = [warning for warning in manifest.get("warnings", []) if warning["page"] in pages]
        if warnings:
            metadata = {**metadata, "source_warnings": warnings}
        result.append(DocumentChunk(
            document_id, _stable_id("chunk", f"{document_id}\0{index}\0{text}"), text, title, source,
            {"chunk_index": index, **base, **metadata, "token_count": count},
        ))

    def flush():
        if not pending:
            return
        pieces, spans = [], []
        offset = 0
        for block in pending:
            text = block["text"]
            if pieces:
                offset += 2
            kind = "heading" if block["kind"] in {"title", "section_header"} else block["kind"]
            span = SourceSpan(offset, offset + len(text), SourceBlock(text, kind, block["page"]), len(spans))
            spans.append(span)
            pieces.append(text)
            offset += len(text)
        document = DocumentContent("\n\n".join(pieces), spans)
        ranges = (_split_ranges(document.text, chunk_size=config.chunk_size, overlap=config.chunk_overlap)
                  if config.strategy == "character" else split_content(document, ".pdf", config, budget))
        for start, end in ranges:
            covered = [block for block, span in zip(pending, spans, strict=True)
                       if span.start < end and span.end > start]
            metadata = source_metadata(document, start, end)
            metadata.update(refs=[block["ref"] for block in covered],
                            page=covered[0]["page"], order=covered[0]["order"],
                            pages=sorted({page for block in covered for page in _pages(block)}),
                            provenance=[_provenance(block) for block in covered])
            append(document.text[start:end], metadata)
        pending.clear()

    for block in manifest["blocks"]:
        kind = block["kind"]
        if not block.get("asset_id") and kind != "formula":
            if block.get("text"):
                pending.append(block)
            continue
        flush()
        metadata = {"kind": kind, "page": block["page"], "order": block["order"],
                    "pages": _pages(block), "refs": [block["ref"]], "provenance": [_provenance(block)]}
        if kind == "formula":
            append(block["text"], {**metadata, "latex": block["latex"],
                                   "original_text": block.get("original_text", ""),
                                   "equation_number": block.get("equation_number")})
            continue
        asset = assets[block["asset_id"]]
        metadata.update(asset_id=asset["asset_id"], asset_kind=asset["kind"])
        caption = asset["caption"] or asset.get("generated_description", "")
        if asset["kind"] == "picture":
            append(caption, metadata)
        else:
            table = asset["table"]
            headers = "\n".join(table_row_text(row) for row in table.get("header_rows", [table["headers"]]))
            for row_number, row in enumerate(table["rows"]):
                text = "\n".join(part for part in (caption, headers, table_row_text(row)) if part)
                append(text, {**metadata, "table_row": row_number})
    flush()
    return result


def _provenance(block: dict) -> dict:
    keys = ("ref", "page", "bbox", "locations", "order", "kind", "association", "associated_assets")
    return {key: block[key] for key in keys if key in block}


def _pages(block: dict) -> list[int]:
    return sorted({part["page"] for part in block.get("locations", [{"page": block["page"]}])})
