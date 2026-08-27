"""Bounded text extraction for supported knowledge document formats."""

from __future__ import annotations

from pathlib import Path
from zipfile import BadZipFile, ZipFile

SUPPORTED_SUFFIXES = frozenset({".docx", ".md", ".pdf", ".txt"})
MAX_PDF_PAGES = 1_000
MAX_DOCX_MEMBERS = 10_000
MAX_DOCX_UNCOMPRESSED_BYTES = 50 * 1024 * 1024
MAX_EXTRACTED_CHARACTERS = 5_000_000


class DocumentReadError(ValueError):
    """Raised when an uploaded document cannot be safely converted to text."""


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
        pages: list[str] = []
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
                pages.append(text)
    except DocumentReadError:
        raise
    except Exception as exc:
        raise DocumentReadError(f"PDF 文件损坏或格式无效：{path.name}") from exc
    if not pages:
        raise DocumentReadError(f"PDF 未提取到文本，扫描件需要先进行 OCR：{path.name}")
    return "\n\n".join(pages)


def _read_docx(path: Path) -> str:
    _validate_docx_archive(path)
    from docx import Document
    from docx.table import Table

    try:
        document = Document(path)
        blocks: list[str] = []
        extracted_characters = 0
        for block in document.iter_inner_content():
            if isinstance(block, Table):
                values = [
                    " | ".join(cell.text.strip() for cell in row.cells if cell.text.strip()) for row in block.rows
                ]
                text = "\n".join(value for value in values if value)
            else:
                text = block.text.strip()
            if not text:
                continue
            extracted_characters += len(text)
            if extracted_characters > MAX_EXTRACTED_CHARACTERS:
                raise DocumentReadError(f"文档提取文本过大：{path.name}")
            blocks.append(text)
    except DocumentReadError:
        raise
    except Exception as exc:
        raise DocumentReadError(f"Word 文件损坏或格式无效：{path.name}") from exc
    return "\n\n".join(blocks)


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
