"""Docling conversion and manifest mapping for the Praxis-RAG Docling service.

The service accepts static HTML and DOCX (the Go backend converts legacy .doc to
DOCX first). Docling converts the file into a :class:`DoclingDocument`; this
module maps that document into the versioned manifest the Go backend ingests.

Contract (validated by ``tests/test_parser_contract.py``):

``manifest.json``
    ``version`` — ``docling-word-v1`` for DOCX, ``docling-html-v1`` for HTML;
    ``blocks`` — ordered content blocks (heading/paragraph/code/caption/note/
    picture/table) with reading-order provenance;
    ``assets`` — picture and table records with captions and table cell spans;
    ``diagnostics`` — safe parse/association diagnostics;
    ``parser`` — service contract, Docling version and input format metadata.

``document.json``
    The serialized ``DoclingDocument`` (images replaced by placeholders) kept
    for audit/debugging; the Go side reads ``manifest.json`` only.

``images/picture-N.<ext>``
    Every picture referenced by ``assets[].image_file``.

Structure preservation rules:

* reading order comes from the ``DoclingDocument`` body tree, not the flat
  ``texts`` array;
* tables keep every cell's row/column offsets and row/column spans, header
  flags and the caption reference Docling reports;
* pictures and tables only take captions Docling explicitly references
  (``<figcaption>``/``<caption>`` or ``alt``); anything else stays a caption
  block so the Go side can decide conservatively (and never force a caption
  onto a neighbouring object);
* external/relative HTML images are never fetched: the picture asset keeps its
  caption text but no image file, and one safe diagnostic records why.
"""

from __future__ import annotations

import base64
import io
import json
import re
import zipfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from . import __version__
from .errors import (
    ParserConfigError,
    ParserConversionError,
    ParserInputError,
    ParserUnsupportedError,
)

#: Evidence-format contract version written by this service. Independent of
#: the Docling release and of the per-format manifest versions below.
SERVICE_CONTRACT_VERSION = "docling-service-v1"

#: Manifest version per accepted input suffix. The Go reader accepts exactly
#: this value per format and records it as the document's parser version.
MANIFEST_VERSION_BY_SUFFIX = {
    ".docx": "docling-word-v1",
    ".html": "docling-html-v1",
    ".htm": "docling-html-v1",
}

SUPPORTED_SUFFIXES = tuple(MANIFEST_VERSION_BY_SUFFIX)

#: Hard bounds mirroring the Go reader limits; a lying upload cannot make the
#: service materialize an unbounded document.
MAX_DOCX_MEMBERS = 10_000
MAX_DOCX_UNCOMPRESSED_BYTES = 50 * 1024 * 1024
MAX_EXTRACTED_CHARACTERS = 5_000_000

#: Docling labels mapped to Praxis-RAG block kinds. Unknown labels stay
#: paragraphs so no body text is dropped.
LABEL_KINDS = {
    "title": "heading",
    "section_header": "heading",
    "code": "code",
    "caption": "caption",
    "footnote": "note",
    "formula": "paragraph",
    "list_item": "paragraph",
    "text": "paragraph",
    "paragraph": "paragraph",
    "reference": "paragraph",
    "page_header": "paragraph",
    "page_footer": "paragraph",
}

_IMAGE_MIME_EXTENSIONS = {
    "image/png": "png",
    "image/jpeg": "jpg",
    "image/jpg": "jpg",
    "image/gif": "gif",
    "image/webp": "webp",
    "image/bmp": "bmp",
    "image/tiff": "tiff",
    "image/svg+xml": "svg",
}

_DATA_URI = re.compile(r"^data:(?P<mime>[^;,]+)?(?P<base64>;base64)?,(?P<payload>.*)$", re.DOTALL)


@dataclass
class ParseOutput:
    """Everything one successful parse returns to the HTTP layer."""

    manifest: dict[str, Any]
    images: dict[str, bytes] = field(default_factory=dict)
    document_json: dict[str, Any] = field(default_factory=dict)


class DoclingConverter:
    """Small wrapper around Docling's ``DocumentConverter``.

    Conversion itself is serialized because one converter instance owns
    format-specific state; mapping a converted document is cheap and runs
    inside the request's concurrency slot.
    """

    def __init__(self) -> None:
        self._converter: Any | None = None
        self._lock = __import__("threading").Lock()
        self._docling_version: str | None = None

    def version(self) -> str:
        if self._docling_version is None:
            try:
                from importlib import metadata

                self._docling_version = metadata.version("docling-slim")
            except Exception:
                try:
                    import docling

                    self._docling_version = getattr(docling, "__version__", "unknown")
                except Exception:
                    self._docling_version = "unavailable"
        return self._docling_version

    def convert(self, source: Path) -> Any:
        with self._lock:
            if self._converter is None:
                try:
                    from docling.document_converter import DocumentConverter
                except Exception as error:  # pragma: no cover - deployment problem
                    raise ParserConfigError(
                        "Docling 解析服务未正确安装：无法导入 docling"
                    ) from error
                self._converter = DocumentConverter()
            try:
                result = self._converter.convert(source)
            except Exception as error:  # Docling raises many converter-specific types
                raise ParserConversionError(
                    "Docling 无法解析该文档（格式无效或包含不支持的内容）"
                ) from error
        return result.document


def validate_upload(data: bytes, filename: str) -> str:
    """Validate the raw upload and return its normalized suffix (``.docx``/``.html``)."""
    suffix = Path(filename or "").suffix.lower()
    if suffix not in MANIFEST_VERSION_BY_SUFFIX:
        raise ParserUnsupportedError("仅支持 .docx、.html 和 .htm 文档")
    if not data:
        raise ParserInputError("上传的文件为空")
    if suffix == ".docx":
        _validate_docx_archive(data)
        return ".docx"
    _validate_html(data)
    return ".html"


def _validate_docx_archive(data: bytes) -> None:
    try:
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            members = archive.infolist()
            if len(members) > MAX_DOCX_MEMBERS:
                raise ParserInputError("Word 文件内部条目过多")
            uncompressed = 0
            names = set()
            for member in members:
                if member.flag_bits & 0x1:
                    raise ParserInputError("Word 文件已加密，暂不支持需要密码的文件")
                uncompressed += member.file_size
                if uncompressed > MAX_DOCX_UNCOMPRESSED_BYTES:
                    raise ParserInputError("Word 解压后超过大小限制")
                names.add(member.filename)
            if "[Content_Types].xml" not in names or "word/document.xml" not in names:
                raise ParserInputError("上传的文件不是有效的 Word (.docx) 文档")
    except zipfile.BadZipFile as error:
        raise ParserInputError("上传的文件不是有效的 Word (.docx) 文档") from error


def _validate_html(data: bytes) -> None:
    try:
        text = data.decode("utf-8-sig")
    except UnicodeDecodeError as error:
        raise ParserInputError("HTML 文件不是有效的 UTF-8 文本") from error
    if not text.strip():
        raise ParserInputError("HTML 文件内容为空")


def parse_document(converter: DoclingConverter, source: Path, suffix: str) -> ParseOutput:
    """Convert one validated file and map it to the manifest contract."""
    manifest_version = MANIFEST_VERSION_BY_SUFFIX[suffix]
    doc = converter.convert(source)
    input_format = "docx" if suffix == ".docx" else "html"
    html_images = extract_html_images(source.read_text(encoding="utf-8-sig")) if input_format == "html" else []
    manifest, images = build_manifest(
        doc,
        input_format=input_format,
        manifest_version=manifest_version,
        html_images=html_images,
        docling_version=converter.version(),
    )
    document_json = serialize_document(doc)
    return ParseOutput(manifest=manifest, images=images, document_json=document_json)


def serialize_document(doc: Any) -> dict[str, Any]:
    """Serialize the DoclingDocument with placeholders instead of image bytes.

    Image bytes travel once, through ``images/``, so the audit copy of the
    document stays small and references images by identity only.
    """
    import tempfile

    from docling_core.types.doc import ImageRefMode

    with tempfile.TemporaryDirectory(prefix="docling-document-") as directory:
        target = Path(directory) / "document.json"
        doc.save_as_json(target, image_mode=ImageRefMode.PLACEHOLDER)
        return json.loads(target.read_text(encoding="utf-8"))


def build_manifest(
    doc: Any,
    *,
    input_format: str,
    manifest_version: str,
    html_images: list["HtmlImage"],
    docling_version: str,
) -> tuple[dict[str, Any], dict[str, bytes]]:
    """Map a DoclingDocument to the versioned manifest and its image bytes."""
    lookup = _document_lookup(doc)
    asset_order = _asset_order(doc, lookup)
    caption_owner = _caption_owners(doc, asset_order)

    blocks: list[dict[str, Any]] = []
    assets: list[dict[str, Any]] = []
    images: dict[str, bytes] = {}
    diagnostics: list[dict[str, Any]] = []
    state = _WalkState(headings=[], section=0, paragraph=0)

    def location() -> dict[str, Any]:
        return {
            "part": "body",
            "paragraph": state.paragraph,
            "headings": list(state.headings),
            "scope": "body",
            "section": state.section,
        }

    def emit_text(item: Any) -> None:
        text = (getattr(item, "text", "") or "").strip()
        if not text:
            return
        label = str(getattr(item, "label", "text") or "text")
        ref = _self_ref(item)
        if ref in caption_owner:
            # Explicit Docling caption (HTML figcaption/caption/alt). Keep it
            # as a caption block associated with its object so the Go chunk
            # builder skips a duplicate text chunk and search hits the asset.
            owner = caption_owner[ref]
            blocks.append(
                _block(
                    ref=ref,
                    order=len(blocks),
                    kind="caption" if owner["kind"] == "caption" else "note",
                    text=text,
                    location=location(),
                    associated_assets=[owner["asset_id"]],
                )
            )
            return
        kind = LABEL_KINDS.get(label, "paragraph")
        if kind == "heading":
            level = _heading_level(item, input_format)
            del state.headings[max(level - 1, 0):]
            state.headings.append(text)
            state.section += 1
        _append_paragraph(state)
        blocks.append(
            _block(
                ref=ref,
                order=len(blocks),
                kind=kind,
                text=text,
                location=location(),
            )
        )
        # Docling nests section content under its heading item, so the reading
        # order continues in the text item's children.
        walk(list(getattr(item, "children", []) or []))

    def emit_table(item: Any) -> None:
        info = asset_order.get(_self_ref(item))
        if info is None:
            return
        asset_id = info["asset_id"]
        captions, footnotes, note_texts = _annotation_texts(doc, item)
        asset_location = location()
        _append_paragraph(state)
        payload = _table_payload(item, captions, note_texts)
        assets.append(
            _asset(
                asset_id=asset_id,
                kind="table",
                location=asset_location,
                caption=_join_annotations(captions),
                caption_source="document" if captions else "none",
                captions=captions,
                footnotes=footnotes,
                table=payload,
            )
        )
        blocks.append(
            _block(
                ref=_self_ref(item),
                order=len(blocks),
                kind="table",
                text="",
                location=asset_location,
                asset_id=asset_id,
            )
        )

    def emit_picture(item: Any) -> None:
        info = asset_order.get(_self_ref(item))
        if info is None:
            return
        asset_id = info["asset_id"]
        picture_index = int(info.get("picture_index") or 0)
        data, mime, missing_kind = _picture_bytes(item, doc, picture_index, html_images, input_format)
        captions, footnotes, _note_texts = _annotation_texts(doc, item)
        asset_location = location()
        _append_paragraph(state)
        image_file = None
        if data is not None and mime is not None:
            extension = _IMAGE_MIME_EXTENSIONS.get(mime.lower(), "png")
            image_file = f"images/{asset_id}.{extension}"
            images[asset_id] = data
        else:
            diagnostics.append(
                {
                    "kind": missing_kind or "missing_picture",
                    "message": _missing_picture_message(missing_kind),
                    "location": asset_location,
                    "text": "",
                    "asset_ids": [asset_id],
                }
            )
        assets.append(
            _asset(
                asset_id=asset_id,
                kind="picture",
                location=asset_location,
                caption=_join_annotations(captions),
                caption_source="document" if captions else "none",
                captions=captions,
                footnotes=footnotes,
                table=None,
                image_file=image_file,
                mime_type=mime if image_file else None,
            )
        )
        blocks.append(
            _block(
                ref=_self_ref(item),
                order=len(blocks),
                kind="picture",
                text="",
                location=asset_location,
                asset_id=asset_id,
            )
        )

    def walk(children: list[Any]) -> None:
        for reference in children:
            kind, item = _resolve(lookup, reference)
            if item is None or _content_layer(item) != "body":
                continue
            if kind == "group":
                walk(list(getattr(item, "children", []) or []))
            elif kind == "text":
                emit_text(item)
            elif kind == "table":
                emit_table(item)
            elif kind == "picture":
                emit_picture(item)

    walk(list(getattr(doc.body, "children", []) or []))

    text_characters = sum(len(block["text"]) for block in blocks) + sum(
        len(asset["caption"]) for asset in assets
    )
    if text_characters > MAX_EXTRACTED_CHARACTERS:
        raise ParserConversionError("文档文本超过大小限制")

    manifest: dict[str, Any] = {
        "version": manifest_version,
        "blocks": blocks,
        "assets": assets,
        "diagnostics": diagnostics,
        "skipped_object_count": 0,
        "parser": {
            "contract": SERVICE_CONTRACT_VERSION,
            "service_version": __version__,
            "docling_version": docling_version,
            "docling_schema": str(getattr(doc, "version", "") or ""),
            "input_format": input_format,
        },
    }
    return manifest, images


def _asset_order(doc: Any, lookup: dict[str, tuple[str, Any]]) -> dict[str, dict[str, Any]]:
    """Assign picture-N/table-N ids in body reading order before block emission.

    Captions can precede their object in the body tree (HTML puts the caption
    text before the table item), so ids must be known before the first block is
    written.
    """
    picture_indices = {id(picture): index for index, picture in enumerate(getattr(doc, "pictures", []) or [])}
    order: dict[str, dict[str, Any]] = {}
    counters = {"picture": 0, "table": 0}

    def walk(children: list[Any]) -> None:
        for reference in children:
            kind, item = _resolve(lookup, reference)
            if item is None or _content_layer(item) != "body":
                continue
            if kind == "group":
                walk(list(getattr(item, "children", []) or []))
            elif kind == "text":
                walk(list(getattr(item, "children", []) or []))
            elif kind == "table":
                counters["table"] += 1
                order[_self_ref(item)] = {
                    "kind": "table",
                    "asset_id": f"table-{counters['table']}",
                    "picture_index": None,
                }
            elif kind == "picture":
                counters["picture"] += 1
                order[_self_ref(item)] = {
                    "kind": "picture",
                    "asset_id": f"picture-{counters['picture']}",
                    "picture_index": picture_indices.get(id(item), counters["picture"] - 1),
                }

    walk(list(getattr(doc.body, "children", []) or []))
    return order


class HtmlImage:
    """One ``<img>`` of the uploaded HTML and the bytes this service may use."""

    __slots__ = ("data", "mime", "alt", "missing")

    def __init__(self, data: bytes | None, mime: str | None, alt: str, missing: str | None) -> None:
        self.data = data
        self.mime = mime
        self.alt = alt
        self.missing = missing


def extract_html_images(html_text: str) -> list[HtmlImage]:
    """Collect HTML images in document order without fetching anything.

    Inline ``data:`` images are decoded. External, protocol-relative and
    relative sources are recorded as missing: the service never performs
    network requests or reads host files referenced by the upload.
    """
    try:
        from bs4 import BeautifulSoup
    except Exception:  # pragma: no cover - deployment problem
        raise ParserConfigError("Docling 解析服务缺少 HTML 解析依赖") from None
    soup = BeautifulSoup(html_text, "html.parser")
    images: list[HtmlImage] = []
    for element in soup.find_all("img"):
        source = (element.get("src") or "").strip()
        alt = (element.get("alt") or "").strip()
        if source.lower().startswith("data:"):
            match = _DATA_URI.match(source)
            if not match:
                images.append(HtmlImage(None, None, alt, "invalid_embedded_picture"))
                continue
            mime = (match.group("mime") or "image/png").strip().lower()
            payload = match.group("payload") or ""
            try:
                if match.group("base64"):
                    data = base64.b64decode(payload, validate=True)
                else:
                    from urllib.parse import unquote_to_bytes

                    data = unquote_to_bytes(payload)
            except Exception:
                images.append(HtmlImage(None, None, alt, "invalid_embedded_picture"))
                continue
            if not data:
                images.append(HtmlImage(None, None, alt, "invalid_embedded_picture"))
                continue
            images.append(HtmlImage(data, mime, alt, None))
            continue
        lowered = source.lower()
        if lowered.startswith("file:") or lowered.startswith("\\"):
            images.append(HtmlImage(None, None, alt, "local_file_picture"))
        elif re.match(r"^[a-zA-Z][a-zA-Z0-9+.-]*:", source) or source.startswith("//"):
            images.append(HtmlImage(None, None, alt, "external_picture"))
        else:
            images.append(HtmlImage(None, None, alt, "relative_picture"))
    return images


def _missing_picture_message(kind: str | None) -> str:
    if kind == "external_picture":
        return "HTML 图片引用了外部资源，服务不会抓取，已保留文字说明"
    if kind == "relative_picture":
        return "HTML 图片引用了未上传的本地相对资源，已保留文字说明"
    if kind == "local_file_picture":
        return "HTML 图片引用了本地文件路径，服务不会读取宿主机文件"
    if kind == "invalid_embedded_picture":
        return "HTML 内嵌图片数据无效，已保留文字说明"
    return "图片数据缺失，已保留文字说明"


@dataclass
class _WalkState:
    headings: list[str]
    section: int
    paragraph: int


def _append_paragraph(state: _WalkState) -> None:
    state.paragraph += 1


def _document_lookup(doc: Any) -> dict[str, tuple[str, Any]]:
    lookup: dict[str, tuple[str, Any]] = {}
    for group in getattr(doc, "groups", []) or []:
        lookup[_self_ref(group)] = ("group", group)
    for text in getattr(doc, "texts", []) or []:
        lookup[_self_ref(text)] = ("text", text)
    for table in getattr(doc, "tables", []) or []:
        lookup[_self_ref(table)] = ("table", table)
    for picture in getattr(doc, "pictures", []) or []:
        lookup[_self_ref(picture)] = ("picture", picture)
    return lookup


def _resolve(lookup: dict[str, tuple[str, Any]], reference: Any) -> tuple[str, Any]:
    ref = getattr(reference, "cref", None) or str(reference)
    found = lookup.get(ref)
    if found is None:
        return "unknown", None
    return found


def _self_ref(item: Any) -> str:
    return str(getattr(item, "self_ref", "") or "")


def _content_layer(item: Any) -> str:
    value = getattr(item, "content_layer", "body")
    value = getattr(value, "value", value)
    return str(value or "body").lower()


def _heading_level(item: Any, input_format: str) -> int:
    label = str(getattr(item, "label", "") or "")
    if label == "title":
        return 1
    level = int(getattr(item, "level", 1) or 1)
    # HTML reports the h1 title and h2+ as section_header level 1.., so shift
    # section headers below the document title; DOCX word styles already map
    # heading styles to level 1.. directly.
    return max(level + (1 if input_format == "html" else 0), 1)


def _headings_for(doc: Any, input_format: str) -> bool:
    # Kept as a named hook: the walk derives heading state from the items, so
    # this only documents that both formats participate in heading splitting.
    return input_format in {"docx", "html"}


def _caption_owners(doc: Any, asset_order: dict[str, dict[str, Any]]) -> dict[str, dict[str, str]]:
    """Map explicit caption/note text refs to their owning asset id.

    Only references Docling itself reports (HTML ``<figcaption>``/``<caption>``
    and ``alt`` text, or DOCX explicit caption relationships) are associated
    here; everything else stays a caption block for the conservative Go-side
    association.
    """
    owners: dict[str, dict[str, str]] = {}
    for item in list(getattr(doc, "tables", []) or []) + list(getattr(doc, "pictures", []) or []):
        info = asset_order.get(_self_ref(item))
        if info is None:
            continue
        for caption in getattr(item, "captions", []) or []:
            owners[_ref_string(caption)] = {"kind": "caption", "asset_id": info["asset_id"]}
        for footnote in getattr(item, "footnotes", []) or []:
            owners[_ref_string(footnote)] = {"kind": "note", "asset_id": info["asset_id"]}
    return owners


def _ref_string(reference: Any) -> str:
    return str(getattr(reference, "cref", None) or reference)


def _resolve_note(doc: Any, reference: Any) -> str:
    try:
        item = reference.resolve(doc) if hasattr(reference, "resolve") else None
    except Exception:
        item = None
    return (getattr(item, "text", "") or "").strip()


def _annotation_texts(
    doc: Any, item: Any
) -> tuple[list[dict[str, Any]], list[dict[str, Any]], list[str]]:
    """Return (caption annotations, footnote annotations, footnote texts)."""
    captions: list[dict[str, Any]] = []
    for caption in getattr(item, "captions", []) or []:
        text = _resolve_note(doc, caption)
        if text:
            captions.append({"text": text, "ref": _ref_string(caption), "location": {}})
    footnotes: list[dict[str, Any]] = []
    note_texts: list[str] = []
    for note in getattr(item, "footnotes", []) or []:
        text = _resolve_note(doc, note)
        if text:
            footnotes.append({"text": text, "ref": _ref_string(note), "location": {}})
            note_texts.append(text)
    return captions, footnotes, note_texts


def _join_annotations(captions: list[dict[str, Any]]) -> str:
    return "\n".join(caption["text"] for caption in captions)


def _block(**values: Any) -> dict[str, Any]:
    block = {
        "ref": values.get("ref", ""),
        "order": values.get("order", 0),
        "kind": values.get("kind", "paragraph"),
        "text": values.get("text", ""),
        "location": values.get("location", {}),
        "asset_id": values.get("asset_id", ""),
        "in_cell": False,
    }
    if values.get("associated_assets"):
        block["associated_assets"] = list(values["associated_assets"])
    return block


def _asset(**values: Any) -> dict[str, Any]:
    return {
        "asset_id": values["asset_id"],
        "kind": values["kind"],
        "image_file": values.get("image_file"),
        "mime_type": values.get("mime_type"),
        "caption": values.get("caption", ""),
        "caption_source": values.get("caption_source", "none"),
        "generated_description": "",
        "captions": values.get("captions", []),
        "footnotes": values.get("footnotes", []),
        "table": values.get("table"),
        "location": values.get("location", {}),
        "labels": [],
        "bookmarks": [],
    }


def _table_payload(
    item: Any, captions: list[dict[str, Any]], footnote_texts: list[str]
) -> dict[str, Any]:
    data = getattr(item, "data")
    num_rows = int(getattr(data, "num_rows", 0) or 0)
    num_cols = int(getattr(data, "num_cols", 0) or 0)
    cells = []
    header_flags: dict[tuple[int, int], bool] = {}
    for cell in getattr(data, "table_cells", []) or []:
        row = int(getattr(cell, "start_row_offset_idx", 0) or 0)
        column = int(getattr(cell, "start_col_offset_idx", 0) or 0)
        header_flags[(row, column)] = bool(getattr(cell, "column_header", False))
        cells.append(
            {
                "row": row,
                "col": column,
                "row_span": max(int(getattr(cell, "row_span", 1) or 1), 1),
                "col_span": max(int(getattr(cell, "col_span", 1) or 1), 1),
                "text": str(getattr(cell, "text", "") or ""),
                "nested_tables": [],
            }
        )
    rows = [["" for _ in range(num_cols)] for _ in range(num_rows)]
    for cell in cells:
        row, column = cell["row"], cell["col"]
        if 0 <= row < num_rows and 0 <= column < num_cols:
            rows[row][column] = cell["text"]
    header_rows = 0
    for row_number in range(num_rows):
        anchors = [(row_number, column) for column in range(num_cols) if rows[row_number][column]]
        if not anchors or not all(header_flags.get(position, False) for position in anchors):
            break
        header_rows += 1
    header_rows_data = [list(row) for row in rows[:header_rows]]
    headers = list(rows[header_rows - 1]) if header_rows else []
    return {
        "rows": rows,
        "cells": cells,
        "headers": headers,
        "header_rows": header_rows_data,
        "num_rows": num_rows,
        "num_cols": num_cols,
        "footnotes": list(footnote_texts),
        "parent_cell": None,
        "caption": _join_annotations(captions),
    }


def _picture_bytes(
    item: Any,
    doc: Any,
    picture_index: int,
    html_images: list[HtmlImage],
    input_format: str,
) -> tuple[bytes | None, str | None, str | None]:
    """Return (bytes, mime, missing_kind) for one picture.

    DOCX pictures carry their bytes in the DoclingDocument image reference;
    HTML pictures do not (Docling does not decode embedded data URIs), so the
    service uses its own document-order image extraction.
    """
    if input_format == "html":
        if picture_index < len(html_images):
            image = html_images[picture_index]
            return image.data, image.mime, image.missing
        return None, None, "missing_picture"
    image = getattr(item, "image", None)
    if image is not None:
        uri = str(getattr(image, "uri", "") or "")
        match = _DATA_URI.match(uri)
        if match:
            mime = (match.group("mime") or "image/png").strip().lower()
            payload = match.group("payload") or ""
            try:
                data = base64.b64decode(payload, validate=True) if match.group("base64") else payload.encode("latin-1")
            except Exception:
                data = None
            if data:
                return data, mime, None
        try:
            pil_image = item.get_image(doc)
        except Exception:
            pil_image = None
        if pil_image is not None:
            buffer = io.BytesIO()
            pil_image.save(buffer, format="PNG")
            return buffer.getvalue(), "image/png", None
    return None, None, "missing_picture"
