"""Token-budgeted splitting over original text spans."""

from __future__ import annotations

import re
from dataclasses import asdict, dataclass
from threading import RLock
from typing import Any

from .document_readers import DocumentContent

STRATEGIES = (
    ("character", "按字符", "characters", 700, 100),
    ("token", "按 token", "tokens", 512, 64),
    ("sentence", "按句子", "tokens", 512, 64),
    ("recursive", "递归切分", "tokens", 512, 64),
    ("structure", "按文档结构", "tokens", 512, 64),
)


@dataclass(frozen=True)
class ChunkingConfig:
    strategy: str = "recursive"
    chunk_size: int = 512
    chunk_overlap: int = 64

    def __post_init__(self) -> None:
        if self.strategy not in {row[0] for row in STRATEGIES}:
            raise ValueError("未知切分策略")
        if type(self.chunk_size) is not int or not 1 <= self.chunk_size <= 100_000:
            raise ValueError("块长度必须是 1 到 100000 的整数")
        if type(self.chunk_overlap) is not int or not 0 <= self.chunk_overlap < self.chunk_size:
            raise ValueError("重叠长度必须是非负整数且小于块长度")
        if self.strategy == "character" and self.chunk_size < 100:
            raise ValueError("字符切分的块长度至少为 100")

    def metadata(self, model: str) -> dict[str, object]:
        return {**asdict(self), "tokenizer": model}


LEGACY_CONFIG = ChunkingConfig("character", 700, 100)


def strategy_catalog() -> dict[str, object]:
    return {"default": "recursive", "strategies": [
        {"id": key, "label": label, "unit": unit, "chunk_size": size, "chunk_overlap": overlap}
        for key, label, unit, size, overlap in STRATEGIES
    ]}


_TOKENIZERS: dict[str, Any] = {}
_TOKENIZER_LOCK = RLock()


def get_tokenizer(model: str) -> Any:
    with _TOKENIZER_LOCK:
        if model not in _TOKENIZERS:
            try:
                from transformers import AutoTokenizer

                tokenizer = AutoTokenizer.from_pretrained(model, use_fast=True)
                if not tokenizer.is_fast:
                    raise ValueError("需要支持原文位置映射的 fast tokenizer")
                _TOKENIZERS[model] = tokenizer
            except Exception as exc:
                raise ValueError(f"Tokenizer 加载失败（{model}）：{type(exc).__name__}") from exc
        return _TOKENIZERS[model]


class TokenBudget:
    def __init__(self, tokenizer: Any, config: ChunkingConfig):
        self.tokenizer = tokenizer
        self.size = config.chunk_size
        self.overlap = config.chunk_overlap
        # BGE-M3 encode defaults to 8192; reserve its special tokens as well.
        maximum = min(int(tokenizer.model_max_length), 8192) - tokenizer.num_special_tokens_to_add(pair=False)
        if self.size > maximum:
            raise ValueError(f"块长度不能超过模型允许的 {maximum} token")

    def count(self, text: str) -> int:
        return len(self.tokenizer.encode(text, add_special_tokens=False, truncation=False, verbose=False))

    def offsets(self, text: str) -> list[tuple[int, int]]:
        return self.tokenizer(text, add_special_tokens=False, truncation=False,
                              return_offsets_mapping=True, verbose=False)["offset_mapping"]

    def suffix(self, text: str, start: int, end: int) -> int:
        if not self.overlap:
            return end
        offsets = self.offsets(text[start:end])
        candidate = start + offsets[max(0, len(offsets) - self.overlap)][0] if offsets else end
        candidate = max(start + 1, candidate)
        while candidate < end and self.count(text[candidate:end]) > self.overlap:
            candidate += 1
        return candidate

    def windows(self, text: str, start: int, end: int) -> list[tuple[int, int]]:
        result = []
        while start < end:
            # Bound work per window even for a multi-million-character sentence.
            probe = min(end, start + max(128, self.size * 8))
            offsets = self.offsets(text[start:probe])
            while len(offsets) <= self.size and probe < end:
                probe = min(end, start + (probe - start) * 2)
                offsets = self.offsets(text[start:probe])
            stop = probe if len(offsets) <= self.size else start + offsets[self.size][0]
            stop = max(start + 1, stop)
            while stop > start and self.count(text[start:stop].strip()) > self.size:
                stop -= 1
            if stop == start:
                raise ValueError("块长度过小，无法容纳一个完整字符")
            result.append((start, stop))
            if stop >= end:
                break
            start = self.suffix(text, start, stop)
        return result


_SENTENCE_END = re.compile(r'''[。！？!?]+[”’"'）)]*|\.(?:[”’"')]*)(?=\s|$)''')
_ABBREVIATION = re.compile(r"(?:\b(?:Mr|Mrs|Ms|Dr|Prof|Sr|Jr|vs|etc)|\b[A-Z]|\b(?:e\.g|i\.e))\.$", re.IGNORECASE)


def sentence_ranges(text: str, start: int, end: int) -> list[tuple[int, int]]:
    result = []
    cursor = start
    for match in _SENTENCE_END.finditer(text, start, end):
        stop = match.end()
        if match.group().startswith(".") and _ABBREVIATION.search(text[max(cursor, match.start() - 8):stop]):
            continue
        result.append((cursor, stop))
        cursor = stop
    if cursor < end:
        result.append((cursor, end))
    return [(a, b) for a, b in result if text[a:b].strip()]


def paragraph_ranges(text: str, start: int, end: int) -> list[tuple[int, int]]:
    ranges = []
    cursor = start
    for match in re.finditer(r"\n[ \t]*\n+", text[start:end]):
        stop = start + match.start()
        if text[cursor:stop].strip():
            ranges.append((cursor, stop))
        cursor = start + match.end()
    if text[cursor:end].strip():
        ranges.append((cursor, end))
    return ranges


def structure_ranges(document: DocumentContent, suffix: str) -> list[tuple[int, int]]:
    groups: list[tuple[int, int]] = []
    last_key = None
    section = 0
    for span in document.spans:
        block = span.block
        if block.kind == "heading":
            section += 1
        if suffix == ".pdf":
            key = ("page", block.page)
        elif suffix in {".md", ".docx", ".doc"} and block.headings:
            key = ("section", section, block.headings, block.table)
        elif block.table is not None:
            key = ("table", block.table)
        else:
            key = ("paragraph", span.paragraph)
        if groups and key == last_key:
            groups[-1] = (groups[-1][0], span.end)
        else:
            groups.append((span.start, span.end))
        last_key = key
    return groups


def split_content(document: DocumentContent, suffix: str, config: ChunkingConfig,
                  budget: TokenBudget) -> list[tuple[int, int]]:
    text = document.text
    if not text.strip():
        return []
    if config.strategy == "token":
        return budget.windows(text, 0, len(text))
    if config.strategy == "sentence":
        initial = sentence_ranges(text, 0, len(text))
    elif config.strategy == "structure":
        initial = structure_ranges(document, suffix)
    else:
        initial = paragraph_ranges(text, 0, len(text))

    units: list[tuple[int, int, bool]] = []
    for start, end in initial:
        if budget.count(text[start:end]) <= budget.size:
            units.append((start, end, False))
            continue
        for a, b in paragraph_ranges(text, start, end):
            if config.strategy != "sentence" and budget.count(text[a:b]) <= budget.size:
                units.append((a, b, False))
                continue
            for c, d in sentence_ranges(text, a, b):
                if budget.count(text[c:d]) <= budget.size:
                    units.append((c, d, False))
                else:
                    units.extend((x, y, True) for x, y in budget.windows(text, c, d))

    chunks: list[tuple[int, int]] = []
    current: tuple[int, int] | None = None
    for start, end, window in units:
        if current is not None and start >= current[1] and budget.count(text[current[0]:end].strip()) <= budget.size:
            current = (current[0], end)
            continue
        if current is not None:
            chunks.append(current)
            if not window and start >= current[1] and budget.overlap:
                # Complete trailing sentences only; never split a sentence just for overlap.
                for a, _ in sentence_ranges(text, current[0], current[1]):
                    if a <= current[0]:
                        continue
                    if budget.count(text[a:current[1]].strip()) <= budget.overlap and budget.count(text[a:end].strip()) <= budget.size:
                        start = a
                        break
        current = (start, end)
    if current is not None:
        chunks.append(current)
    return chunks


def source_metadata(document: DocumentContent, start: int, end: int) -> dict[str, object]:
    covered = [span for span in document.spans if span.start < end and span.end > start]
    locations = [{"start": max(start, span.start), "end": min(end, span.end),
                  "paragraph": span.paragraph, "page": span.block.page,
                  "heading_path": list(span.block.headings), "kind": span.block.kind,
                  "table": span.block.table} for span in covered]
    headings = list(dict.fromkeys(span.block.headings for span in covered if span.block.headings))
    return {"start": start, "end": end, "pages": sorted({span.block.page for span in covered if span.block.page}),
            "heading_paths": [list(path) for path in headings], "locations": locations}
