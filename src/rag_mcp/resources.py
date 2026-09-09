"""Stable document resources and lossless, bounded paragraph pages."""

from __future__ import annotations

import re
from pathlib import Path
from urllib.parse import parse_qs, quote, unquote, urlencode, urlsplit

from .pdf_content import FORMULA_BLOCK

PAGE_CHARACTERS = 8000


def document_uri(namespace: str, name: str) -> str:
    return f"rag://knowledge/{quote(namespace, safe='')}/{quote(name, safe='')}"


def parse_uri(namespace: str, uri: str) -> tuple[str, int, str | None]:
    parsed = urlsplit(uri)
    parts = parsed.path.split("/")
    if parsed.scheme != "rag" or parsed.netloc != "knowledge" or parsed.fragment or len(parts) != 3:
        raise ValueError("Invalid document URI")
    uri_namespace, name = (unquote(value, errors="strict") for value in parts[1:])
    if uri_namespace != namespace or not name or name in {".", ".."} or any(c in name for c in "/\\\x00"):
        raise ValueError("Document URI is outside this namespace")
    # Require the canonical spelling so encoded separators cannot be decoded twice.
    if f"{parsed.scheme}://{parsed.netloc}{parsed.path}" != document_uri(namespace, name):
        raise ValueError("Non-canonical document URI")
    query = parse_qs(parsed.query, keep_blank_values=True, strict_parsing=True)
    if set(query) - {"page", "version"} or any(len(values) != 1 for values in query.values()):
        raise ValueError("Invalid page parameters")
    raw_page = query.get("page", ["1"])[0]
    if not re.fullmatch(r"[1-9][0-9]{0,8}", raw_page):
        raise ValueError("Invalid page number")
    page = int(raw_page)
    version = query.get("version", [None])[0]
    if "version" in query and not version:
        raise ValueError("Empty document version")
    if page > 1 and not version:
        raise ValueError("Read the document URI first to obtain a versioned next-page URI")
    return name, page, version


def paragraph_pages(text: str, *, protect_inline_math: bool = False) -> list[str]:
    pages: list[str] = []
    formulas = [(match.start(), match.end()) for match in FORMULA_BLOCK.finditer(text)]
    if protect_inline_math:
        formulas.extend((match.start(), match.end()) for match in re.finditer(r"(?<!\$)\$(?!\$)[^$]+\$(?!\$)", text))
        formulas.sort()
    start = 0
    while start < len(text):
        if len(text) - start <= PAGE_CHARACTERS:
            pages.append(text[start:])
            break
        end = text.rfind("\n\n", start, start + PAGE_CHARACTERS)
        end = end + 2 if end >= start else start + PAGE_CHARACTERS
        for formula_start, formula_end in formulas:
            if formula_start < end < formula_end:
                end = formula_start if formula_start > start else formula_end
                break
        pages.append(text[start:end])
        start = end
    return pages or [""]


def resource_page(namespace: str, name: str, text: str, version: str, page: int, expected: str | None) -> dict:
    if expected is not None and expected != version:
        raise ValueError("Document changed; read its document URI again")
    pages = paragraph_pages(text, protect_inline_math=Path(name).suffix.lower() in {".doc", ".docx"})
    if page > len(pages):
        raise ValueError("Page does not exist")
    uri = document_uri(namespace, name)
    has_more = page < len(pages)
    return {
        "uri": uri,
        "text": pages[page - 1],
        "page": page,
        "total_pages": len(pages),
        "version": version,
        "truncated": has_more,
        "has_more": has_more,
        "next_uri": f"{uri}?{urlencode({'page': page + 1, 'version': version})}" if has_more else None,
    }
