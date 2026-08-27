"""Deterministic BM25-style sparse encoding for multilingual knowledge chunks."""

from __future__ import annotations

import hashlib
import math
import re
import unicodedata
from collections import Counter
from collections.abc import Iterable
from dataclasses import dataclass

_SEGMENT_PATTERN = re.compile(r"[a-z0-9]+|[\u3400-\u4dbf\u4e00-\u9fff]+", re.IGNORECASE)
_CJK_PATTERN = re.compile(r"^[\u3400-\u4dbf\u4e00-\u9fff]+$")
_HASH_PERSON = b"rag-bm25"


@dataclass(frozen=True)
class SparseEmbedding:
    """Sorted sparse indices and weights independent of a vector database SDK."""

    indices: tuple[int, ...]
    values: tuple[float, ...]

    def __post_init__(self) -> None:
        if len(self.indices) != len(self.values):
            raise ValueError("Sparse indices and values must have equal lengths")
        if tuple(sorted(set(self.indices))) != self.indices:
            raise ValueError("Sparse indices must be unique and sorted")
        if any(index < 0 or index > 0xFFFFFFFF for index in self.indices):
            raise ValueError("Sparse indices must be unsigned 32-bit integers")
        if any(not math.isfinite(value) or value <= 0 for value in self.values):
            raise ValueError("Sparse values must be finite positive numbers")


class Bm25SparseEncoder:
    """Hash lexical tokens and apply BM25 term-frequency saturation.

    Qdrant's ``Modifier.IDF`` supplies collection-level inverse document
    frequency at query time.  This encoder owns deterministic tokenization and
    the document-side BM25 term-frequency component, so it requires no local
    corpus state and remains safe to use from independent upload processes.
    """

    def __init__(self, *, k1: float = 1.2, b: float = 0.75, length_pivot: float = 100.0) -> None:
        if k1 <= 0 or not 0 <= b <= 1 or length_pivot <= 0:
            raise ValueError("BM25 k1, b, and length_pivot are out of range")
        self.k1 = k1
        self.b = b
        self.length_pivot = length_pivot

    def encode_documents(self, texts: list[str]) -> list[SparseEmbedding]:
        """Encode every document without sharing mutable corpus statistics."""

        return [self.encode_document(text) for text in texts]

    def encode_document(self, text: str) -> SparseEmbedding:
        """Return BM25-saturated lexical weights for one chunk."""

        tokens = tokenize(text)
        if not tokens:
            return SparseEmbedding((), ())
        counts = Counter(tokens)
        length_normalization = 1.0 - self.b + self.b * len(tokens) / self.length_pivot
        weighted = (
            (_token_index(token), (count * (self.k1 + 1.0)) / (count + self.k1 * length_normalization))
            for token, count in counts.items()
        )
        return _embedding(weighted)

    def encode_query(self, text: str) -> SparseEmbedding:
        """Use bounded query term frequency; Qdrant adds collection IDF."""

        counts = Counter(tokenize(text))
        if not counts:
            return SparseEmbedding((), ())
        return _embedding((_token_index(token), 1.0 + math.log(count)) for token, count in counts.items())


def tokenize(text: str) -> list[str]:
    """Normalize Latin tokens and emit CJK unigrams plus adjacent bigrams."""

    normalized = unicodedata.normalize("NFKC", str(text or "")).casefold()
    tokens: list[str] = []
    for match in _SEGMENT_PATTERN.finditer(normalized):
        segment = match.group(0)
        if not _CJK_PATTERN.fullmatch(segment):
            tokens.append(segment)
            continue
        characters = list(segment)
        tokens.extend(characters)
        tokens.extend("".join(pair) for pair in zip(characters, characters[1:], strict=False))
    return tokens


def _token_index(token: str) -> int:
    digest = hashlib.blake2s(token.encode("utf-8"), digest_size=4, person=_HASH_PERSON).digest()
    return int.from_bytes(digest, "big", signed=False)


def _embedding(weighted: dict[int, float] | Iterable[tuple[int, float]]) -> SparseEmbedding:
    # Hash collisions are intentionally merged before sorting.  Four-byte
    # indices keep Qdrant payloads compact while making collisions negligible
    # for the bounded per-chunk vocabulary used here.
    merged: dict[int, float] = {}
    items = weighted.items() if isinstance(weighted, dict) else weighted
    for index, value in items:
        merged[index] = merged.get(index, 0.0) + value
    ordered = sorted(merged.items())
    return SparseEmbedding(
        tuple(index for index, _value in ordered),
        tuple(value for _index, value in ordered),
    )
