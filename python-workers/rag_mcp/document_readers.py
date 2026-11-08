"""Bounded text extraction for supported knowledge document formats."""

from __future__ import annotations

from pathlib import Path
from dataclasses import dataclass, field
import re
from zipfile import BadZipFile, ZipFile

SUPPORTED_SUFFIXES = frozenset({".doc", ".docx", ".md", ".pdf", ".txt"})
MAX_PDF_PAGES = 1_000
MAX_DOCX_MEMBERS = 10_000
MAX_DOCX_UNCOMPRESSED_BYTES = 50 * 1024 * 1024
MAX_EXTRACTED_CHARACTERS = 5_000_000


class DocumentReadError(ValueError):
    """Raised when an uploaded document cannot be safely converted to text."""


@dataclass(frozen=True)
class SourceBlock:
    text: str
    kind: str = "paragraph"
    page: int | None = None
    headings: tuple[str, ...] = ()
    table: int | None = None


@dataclass(frozen=True)
class SourceSpan:
    start: int
    end: int
    block: SourceBlock
    paragraph: int


@dataclass(frozen=True)
class DocumentContent:
    text: str
    spans: list[SourceSpan] = field(default_factory=list)
    original_text: str = ""


def _paragraphs(text: str) -> list[str]:
    return [part.strip() for part in re.split(r"\n[ \t]*\n+", text.replace("\r\n", "\n").replace("\r", "\n")) if part.strip()]


def read_document(path: Path) -> DocumentContent:
    resolved = path.expanduser().resolve()
    suffix = resolved.suffix.casefold()
    if suffix not in SUPPORTED_SUFFIXES:
        raise DocumentReadError(f"暂不支持该文件类型：{suffix or '<none>'}")
    if suffix == ".pdf":
        pages = _pdf_pages(resolved)
        original = "\n\n".join(page.text for page in pages)
        blocks = [SourceBlock(part, page=page.page) for page in pages for part in _paragraphs(page.text)]
    elif suffix in {".doc", ".docx"}:
        blocks = _docx_blocks(resolved)
        original = _join_docx(blocks)
    elif suffix == ".md":
        original = _read_utf8(resolved)
        blocks = _markdown_blocks(original)
    else:
        original = _read_utf8(resolved)
        blocks = [SourceBlock(part) for part in _paragraphs(original)]
    spans: list[SourceSpan] = []
    parts: list[str] = []
    offset = 0
    for block in blocks:
        if not block.text.strip():
            continue
        if parts:
            offset += 2
        spans.append(SourceSpan(offset, offset + len(block.text), block, len(spans) + 1))
        parts.append(block.text)
        offset += len(block.text)
    return DocumentContent(_bounded_text("\n\n".join(parts), resolved.name), spans, original)


def _markdown_blocks(text: str) -> list[SourceBlock]:
    from markdown_it import MarkdownIt

    lines = text.splitlines(keepends=True)
    tokens = MarkdownIt("commonmark").parse(text)
    headings: list[tuple[int, str]] = []
    blocks: list[SourceBlock] = []
    cursor = 0
    for index, token in enumerate(tokens):
        if token.level != 0 or token.map is None or token.nesting == -1:
            continue
        start, end = token.map
        if start < cursor:
            continue
        if start > cursor and "".join(lines[cursor:start]).strip():
            blocks.append(SourceBlock("".join(lines[cursor:start]).strip(), headings=tuple(h[1] for h in headings)))
        if token.type == "heading_open":
            level = int(token.tag[1:])
            title = tokens[index + 1].content
            headings = [item for item in headings if item[0] < level] + [(level, title)]
        kind = "heading" if token.type == "heading_open" else "code" if token.type in {"fence", "code_block"} else "paragraph"
        blocks.append(SourceBlock("".join(lines[start:end]).rstrip("\r\n"), kind, headings=tuple(h[1] for h in headings)))
        cursor = end
    if "".join(lines[cursor:]).strip():
        blocks.append(SourceBlock("".join(lines[cursor:]).rstrip("\r\n"), headings=tuple(h[1] for h in headings)))
    return blocks


def read_document_text(path: Path) -> str:
    """Extract bounded plain text from one supported document."""

    resolved = path.expanduser().resolve()
    suffix = resolved.suffix.casefold()
    if suffix not in SUPPORTED_SUFFIXES:
        raise DocumentReadError(f"暂不支持该文件类型：{suffix or '<none>'}")
    if suffix in {".md", ".txt"}:
        return _read_utf8(resolved)
    if suffix == ".pdf":
        return _read_pdf(resolved)
    return _read_docx(resolved)


def _read_utf8(path: Path) -> str:
    try:
        return _bounded_text(path.read_text(encoding="utf-8-sig"), path.name)
    except UnicodeDecodeError as exc:
        raise DocumentReadError(f"文本文件必须使用 UTF-8 编码：{path.name}") from exc
    except OSError as exc:
        raise DocumentReadError(f"知识文件无法读取：{path.name}") from exc


def _read_pdf(path: Path) -> str:
    return "\n\n".join(block.text for block in _pdf_pages(path))


def _pdf_blocks(path: Path) -> list[SourceBlock]:
    return [SourceBlock(part, page=page.page) for page in _pdf_pages(path) for part in _paragraphs(page.text)]


def _pdf_pages(path: Path) -> list[SourceBlock]:
    from pypdf import PdfReader

    try:
        reader = PdfReader(path, strict=False)
        if reader.is_encrypted:
            try:
                decrypted = reader.decrypt("")
            except Exception as exc:
                raise DocumentReadError(f"PDF 已加密，暂不支持需要密码的文件：{path.name}") from exc
            if not decrypted:
                raise DocumentReadError(f"PDF 已加密，暂不支持需要密码的文件：{path.name}")
        page_count = len(reader.pages)
        if page_count > MAX_PDF_PAGES:
            raise DocumentReadError(f"PDF 页数不能超过 {MAX_PDF_PAGES} 页：{path.name}")
        pages: list[SourceBlock] = []
        extracted_characters = 0
        for page_number, page in enumerate(reader.pages, start=1):
            try:
                text = page.extract_text() or ""
            except Exception as exc:
                raise DocumentReadError(f"PDF 第 {page_number} 页文本提取失败：{path.name}") from exc
            extracted_characters += len(text)
            if extracted_characters > MAX_EXTRACTED_CHARACTERS:
                raise DocumentReadError(f"文档提取文本过大：{path.name}")
            if text.strip():
                pages.append(SourceBlock(text, page=page_number))
    except DocumentReadError:
        raise
    except Exception as exc:
        raise DocumentReadError(f"PDF 文件损坏或格式无效：{path.name}") from exc
    if not pages:
        raise DocumentReadError(f"PDF 未提取到文本，扫描件需要先进行 OCR：{path.name}")
    return pages


def _read_docx(path: Path) -> str:
    return _join_docx(_docx_blocks(path))


def _join_docx(blocks: list[SourceBlock]) -> str:
    parts: list[str] = []
    previous_table = None
    for block in blocks:
        if parts:
            parts.append("\n" if block.table is not None and block.table == previous_table else "\n\n")
        parts.append(block.text)
        previous_table = block.table
    return "".join(parts)


def _docx_blocks(path: Path) -> list[SourceBlock]:
    import os
    from .word_converter import word_source
    from .word_reader import parse_docx
    try:
        with word_source(path, timeout=int(os.getenv("RAG_WORD_CONVERT_TIMEOUT", "120"))) as source:
            manifest = parse_docx(source)
        assets = {asset["asset_id"]: asset for asset in manifest["assets"]}
        blocks = []
        for item in manifest["blocks"]:
            headings = tuple(item["location"]["headings"])
            asset = assets.get(item.get("asset_id"))
            if asset and asset["kind"] == "table":
                index = list(assets).index(asset["asset_id"]) + 1
                for row in asset["table"]["rows"]:
                    value = " | ".join(str(cell).replace("\n", " ").replace("|", r"\|") for cell in row)
                    blocks.append(SourceBlock(value, "table_row", headings=headings, table=index))
            elif asset:
                blocks.append(SourceBlock(f"[Image: {asset['asset_id']}]", "picture", headings=headings))
            elif item["text"]:
                blocks.append(SourceBlock(item["text"], item["kind"], headings=headings))
        return blocks
    except DocumentReadError:
        raise
    except Exception as exc:
        raise DocumentReadError(f"Word 解析失败：{path.name}；{exc}") from exc


def _validate_docx_archive(path: Path) -> None:
    try:
        with ZipFile(path) as archive:
            members = archive.infolist()
            if len(members) > MAX_DOCX_MEMBERS:
                raise DocumentReadError(f"Word 文件内部条目过多：{path.name}")
            if any(member.flag_bits & 0x1 for member in members):
                raise DocumentReadError(f"Word 文件已加密，暂不支持需要密码的文件：{path.name}")
            uncompressed_bytes = sum(member.file_size for member in members)
            if uncompressed_bytes > MAX_DOCX_UNCOMPRESSED_BYTES:
                limit_mib = MAX_DOCX_UNCOMPRESSED_BYTES // (1024 * 1024)
                raise DocumentReadError(f"Word 解压后不能超过 {limit_mib} MiB：{path.name}")
    except DocumentReadError:
        raise
    except (BadZipFile, OSError) as exc:
        raise DocumentReadError(f"Word 文件损坏或格式无效：{path.name}") from exc


def _bounded_text(text: str, name: str) -> str:
    if len(text) > MAX_EXTRACTED_CHARACTERS:
        raise DocumentReadError(f"文档提取文本过大：{name}")
    return text
