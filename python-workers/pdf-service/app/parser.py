"""Vision-backed PDF conversion used by the Praxis-RAG PDF microservice.

The service is deliberately split in two halves:

* **local** — PyMuPDF reads the PDF, renders pages and crops picture/table
  regions (:mod:`app.pdfreader`); no model weights are loaded in this process.
* **remote** — an OpenAI-compatible vision endpoint does layout analysis, OCR,
  table structure, formula LaTeX and picture descriptions
  (:mod:`app.vision`). The Go backend sends the endpoint/model/key per knowledge
  base through ``options.vision``; ``RAG_VLM_*`` is only the service fallback.

This module assembles the local and remote halves into the manifest dictionary
that the Go backend unmarshals into ``document.PDFManifest``
(``internal/document/pdf.go``). The manifest contract is frozen: version
``docling-service-v1`` (the string is the **evidence format** contract and is
independent of the implementation that writes it), block/asset/warning key sets
identical to the former Docling converter, picture assets stored as
``pictures/picture-N.png`` and table assets keeping ``image_file`` null.
:func:`validate_manifest` stays the last gate before the zip is returned.

Security rules enforced here:

* the per-request API key never reaches the manifest (:func:`_parser_metadata`),
  a log line or an error message;
* vision/model failures are rewritten into page-specific Chinese errors that
  never quote the upstream response body;
* text-block ``text`` never carries document-wide content, only the block text.
"""

from __future__ import annotations

import json
import math
import re
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol

from . import SERVICE_NAME, __version__
from .errors import (  # re-exported: app.main and the tests import them from here
    ParserConfigError,
    ParserConversionError,
    ParserError,
    ParserOptionsError,
    VisionError,
)
from .pdfreader import (
    DEFAULT_PAGE_DPI,
    PdfRenderer,
    PointBBox,
    RenderedPage,
    points_to_bbox,
)
from .vision import (
    BACKEND,
    DEFAULT_CONCURRENCY,
    DEFAULT_MAX_TOKENS,
    DEFAULT_PROMPT_VERSION,
    DEFAULT_TIMEOUT,
    DESCRIPTION_PROMPT,
    PAGE_PROMPT,
    TABLE_PROMPT,
    VisionClient,
    VisionSettings,
)

#: Manifest contract version consumed by the Go backend.
SERVICE_PARSER_VERSION = "docling-service-v1"

#: Bounds that mirror the HTTP layer / former worker.
MAX_FILE_BYTES = 100 * 1024 * 1024

#: Per-request defaults for ``options.vision`` when timeout/concurrency are absent.
VISION_DEFAULT_TIMEOUT = DEFAULT_TIMEOUT
VISION_DEFAULT_CONCURRENCY = DEFAULT_CONCURRENCY

#: Documented failure texts (Chinese, page-numbered, no response body).
NOT_CONFIGURED_MESSAGE = (
    "PDF 解析服务未配置视觉模型：请设置 RAG_VLM_BASE_URL 与 RAG_VLM_MODEL，"
    "或在请求 options.vision 中提供"
)
OCR_REMOVED_MESSAGE = "options.ocr 已废除：OCR 已并入视觉模型，请使用 options.vision"
VISION_FAILED_MESSAGE = (
    "PDF 解析失败：第 {page} 页视觉模型调用失败，请检查服务地址、模型和 API Key"
)
TABLE_FAILED_MESSAGE = "PDF 解析失败：第 {page} 页表格结构识别失败"
FORMULA_EMPTY_MESSAGE = "PDF 解析失败：第 {page} 页公式识别结果为空"

#: Blocks whose ``kind`` is passed through by ``pdfcontent``; Docling's labels
#: (title/section_header -> heading) and these values line up.
BLOCK_KINDS = frozenset(
    {
        "text",
        "title",
        "section_header",
        "list_item",
        "caption",
        "footnote",
        "formula",
        "table",
        "picture",
        "code",
    }
)
_TABLE_ORIENTATIONS = frozenset({"horizontal", "rotated_90", "rotated_180", "rotated_270"})

#: Manifest key sets, asserted by :func:`validate_manifest`. The Go structs have
#: no ``omitempty``, so every key is emitted for every block/asset.
_MANIFEST_KEYS = {"version", "blocks", "assets", "warnings", "parser", "metrics"}
_BLOCK_KEYS = {
    "ref",
    "kind",
    "page",
    "order",
    "text",
    "asset_id",
    "bbox",
    "locations",
    "latex",
    "original_text",
    "equation_number",
    "association",
    "associated_assets",
}
_ASSET_KEYS = {
    "asset_id",
    "kind",
    "page",
    "bbox",
    "caption",
    "caption_source",
    "generated_description",
    "image_file",
    "table",
    "captions",
    "footnotes",
}
_ANNOTATION_KEYS = {"ref", "text", "page", "bbox", "locations"}
_WARNING_KEYS = {"page", "kind", "count", "message"}
_TABLE_KEYS = {
    "num_rows",
    "num_cols",
    "orientation",
    "headers",
    "header_rows",
    "rows",
    "cells",
    "footnotes",
}
_LOCATION_KEYS = {"page", "bbox", "charspan"}

_EQUATION_NUMBER = re.compile(r"\((\d+(?:[A-Za-z]|\.\d+)?)\)\s*[,.;]?\s*$")
_EXPLICIT_EQUATION_NUMBER = re.compile(r"^\d+(?:[A-Za-z]|\.\d+)?$")

# ---------------------------------------------------------------------------
# Per-request options (multipart field ``options``, JSON)
# ---------------------------------------------------------------------------
#
# The Go backend sends an optional text field ``options`` next to ``file``::
#
#     {"vision": {"base_url": "https://host:8443/v1", "api_key": "sk-...",
#                 "model": "qwen2.5-vl-72b", "timeout": 120, "concurrency": 4}}
#
# ``vlm`` is accepted as a legacy alias for the same object; ``ocr`` was
# removed when OCR moved into the vision model and is rejected with a dedicated
# message. When the field is absent, empty or ``{}`` the process configuration
# (``RAG_VLM_*``) is used instead. Invalid JSON/types/fields raise
# :class:`ParserOptionsError` (HTTP 422) with a message that never echoes request
# values — ``options`` may carry the API key.

_OPTIONS_KEYS = {"vision", "vlm"}
_VISION_OPTION_KEYS = {"base_url", "api_key", "model", "timeout", "concurrency"}


@dataclass(frozen=True)
class ParseOptions:
    """Validated per-request options for one ``/v1/parse`` call.

    An empty instance means "use the server configuration": no endpoint
    override, so the ``RAG_VLM_*`` fallback applies.
    """

    vision: VisionSettings | None = None

    def vision_or(self, fallback: VisionSettings | None) -> VisionSettings | None:
        """Request endpoint wins over the process ``RAG_VLM_*`` fallback."""
        return self.vision if self.vision is not None else fallback


def parse_options_payload(raw: str | None) -> ParseOptions:
    """Parse the multipart ``options`` text field into :class:`ParseOptions`.

    ``None`` (field absent), an empty string and the JSON literal ``null`` all
    mean "no per-request overrides". Any malformed payload raises
    :class:`ParserOptionsError`; messages never include the submitted values.
    """
    if raw is None:
        return ParseOptions()
    if isinstance(raw, (bytes, bytearray)):
        try:
            raw = bytes(raw).decode("utf-8")
        except UnicodeDecodeError as error:
            raise ParserOptionsError("options 必须是 UTF-8 编码的 JSON 文本") from error
    if not isinstance(raw, str):
        raise ParserOptionsError("options 必须是文本字段")
    text = raw.strip()
    if not text:
        return ParseOptions()
    try:
        payload = json.loads(text)
    except ValueError as error:
        raise ParserOptionsError("options 不是合法的 JSON 对象") from error
    if payload is None:
        return ParseOptions()
    if not isinstance(payload, dict):
        raise ParserOptionsError("options 必须是 JSON 对象")
    if "ocr" in payload:
        raise ParserOptionsError(OCR_REMOVED_MESSAGE)
    _reject_unknown_keys(payload, _OPTIONS_KEYS, "options", "vision 与 vlm")
    if "vision" in payload and "vlm" in payload:
        raise ParserOptionsError("options 同时包含 vision 与 vlm：请只保留 options.vision")
    key = "vision" if "vision" in payload else "vlm"
    return ParseOptions(vision=_parse_vision_options(payload.get(key), f"options.{key}"))


def _parse_vision_options(value: Any, label: str) -> VisionSettings | None:
    """Build the request endpoint override; an empty object means "no override"."""
    if value is None:
        return None
    if not isinstance(value, dict):
        raise ParserOptionsError(f"{label} 必须是 JSON 对象")
    _reject_unknown_keys(
        value,
        _VISION_OPTION_KEYS,
        label,
        "base_url、api_key、model、timeout、concurrency",
    )
    base_url = _option_text(value, "base_url", f"{label}.base_url")
    model = _option_text(value, "model", f"{label}.model")
    api_key = _option_text(value, "api_key", f"{label}.api_key")
    if not base_url and not model and not api_key:
        return None
    if not base_url or not model:
        raise ParserOptionsError(
            f"{label}.base_url 与 {label}.model 必须同时提供且为非空字符串"
        )
    timeout = _option_timeout(value, "timeout", f"{label}.timeout", VISION_DEFAULT_TIMEOUT)
    concurrency = _option_concurrency(
        value, "concurrency", f"{label}.concurrency", VISION_DEFAULT_CONCURRENCY
    )
    return VisionSettings(
        base_url=base_url,
        api_key=api_key,
        model=model,
        timeout=timeout,
        concurrency=concurrency,
    )


def _reject_unknown_keys(value: dict[str, Any], allowed: set[str], label: str, names: str) -> None:
    if set(value) - allowed:
        raise ParserOptionsError(f"{label} 包含不支持的字段；仅支持 {names}")


def _option_text(value: dict[str, Any], key: str, label: str) -> str:
    raw = value.get(key)
    if raw is None:
        return ""
    if not isinstance(raw, str):
        raise ParserOptionsError(f"{label} 必须是字符串")
    return raw.strip()


def _option_timeout(value: dict[str, Any], key: str, label: str, default: float) -> float:
    raw = value.get(key)
    if raw is None:
        return default
    # bool is an int subclass; JSON ``true`` must not become ``timeout=1``.
    if isinstance(raw, bool) or not isinstance(raw, (int, float)):
        raise ParserOptionsError(f"{label} 必须是数字")
    number = float(raw)
    if not math.isfinite(number) or number <= 0:
        raise ParserOptionsError(f"{label} 必须是大于 0 的数字")
    return number


def _option_concurrency(value: dict[str, Any], key: str, label: str, default: int) -> int:
    raw = value.get(key)
    if raw is None:
        return default
    if isinstance(raw, bool) or not isinstance(raw, (int, float)):
        raise ParserOptionsError(f"{label} 必须是整数")
    if isinstance(raw, float):
        if not math.isfinite(raw) or not raw.is_integer():
            raise ParserOptionsError(f"{label} 必须是整数")
        raw = int(raw)
    if raw < 1:
        raise ParserOptionsError(f"{label} 必须是大于等于 1 的整数")
    return int(raw)


# ---------------------------------------------------------------------------
# Formula helpers (pure functions, unit-tested without any model)
# ---------------------------------------------------------------------------


def parse_equation_number(text: str) -> str | None:
    """Return the trailing equation number, for example ``"12"`` for ``(12)``."""
    match = _EQUATION_NUMBER.search(text or "")
    return match.group(1) if match else None


def render_formula_text(latex: str, equation_number: str | None) -> str:
    """Render the block text consumed as chunk content by the Go backend.

    ``$$\\n<latex>\\n$$`` plus the equation number when one is known and is not
    already present in the LaTeX itself.
    """
    if not latex.strip():
        raise ParserConversionError("Formula block is empty: refusing to index an empty formula")
    text = "$$\n" + latex.strip() + "\n$$"
    if equation_number and not re.search(r"\(\s*" + re.escape(equation_number) + r"\s*\)", latex):
        text += "\n\n(" + equation_number + ")"
    return text


def clean_description(text: str) -> str:
    """Normalize a model description into a complete sentence."""
    value = text.replace("<end_of_utterance>", "").replace("<end_of_utteranc", "").strip()
    if not value:
        return value
    if value[-1] not in ".!?。！？":
        value += "."
    return value


# ---------------------------------------------------------------------------
# Converter interface
# ---------------------------------------------------------------------------


class PdfConverter(Protocol):
    """Converter interface used by the HTTP layer (vision or fake)."""

    name: str

    def convert(
        self,
        source: Path,
        output_dir: Path,
        scratch_dir: Path,
        *,
        options: ParseOptions | None = None,
    ) -> dict[str, Any]:
        """Write ``manifest.json``-ready artifacts into ``output_dir``.

        ``scratch_dir`` is for intermediate files that must not be published
        (diagnostics). ``options`` are the per-request overrides; ``None`` means
        "use the server configuration" (fake mode ignores them by design).
        """


# ---------------------------------------------------------------------------
# Vision session: one client, counted calls, one retry per call
# ---------------------------------------------------------------------------


class VisionSession:
    """Counts and retries vision calls for one document.

    A failed call is attempted once more (the contract: at most two attempts per
    page). Counters are thread-safe because page calls run concurrently.
    """

    def __init__(self, client: Any, *, attempts: int = 2, retry_delay: float = 0.1) -> None:
        self.client = client
        self.attempts = max(1, int(attempts))
        self.retry_delay = max(0.0, float(retry_delay))
        self._lock = threading.Lock()
        self.requests = 0
        self.failures = 0

    def json(
        self,
        prompt: str,
        image_jpeg: bytes | None,
        *,
        page: int,
        max_tokens: int | None = None,
    ) -> dict[str, Any]:
        """One call that must answer with a JSON object (retried once)."""
        return self._run(
            lambda: self.client.complete_json(prompt, image_jpeg, max_tokens=max_tokens),
            page=page,
        )

    def text(
        self,
        prompt: str,
        image_jpeg: bytes | None,
        *,
        page: int,
        max_tokens: int | None = None,
    ) -> str:
        """One call that answers with free text (retried once)."""
        return self._run(
            lambda: self.client.complete(prompt, image_jpeg, max_tokens=max_tokens),
            page=page,
        )

    def _run(self, operation: Any, *, page: int) -> Any:
        last_error: VisionError | None = None
        for attempt in range(self.attempts):
            if attempt:
                time.sleep(self.retry_delay * attempt)
            try:
                result = operation()
            except VisionError as error:
                with self._lock:
                    self.requests += 1
                    self.failures += 1
                last_error = error
                continue
            with self._lock:
                self.requests += 1
            return result
        raise ParserConversionError(VISION_FAILED_MESSAGE.format(page=page)) from last_error


# ---------------------------------------------------------------------------
# Raw model blocks
# ---------------------------------------------------------------------------

_KIND_ALIASES = {
    "paragraph": "text",
    "plain_text": "text",
    "text_block": "text",
    "body": "text",
    "heading": "title",
    "section": "section_header",
    "header": "section_header",
    "subtitle": "title",
    "list": "list_item",
    "bullet": "list_item",
    "list_item": "list_item",
    "figure_caption": "caption",
    "table_caption": "caption",
    "table_title": "caption",
    "image_caption": "caption",
    "equation": "formula",
    "math": "formula",
    "figure": "picture",
    "image": "picture",
    "chart": "picture",
    "diagram": "picture",
    "photo": "picture",
    "code_block": "code",
    "footer": "footnote",
}


def _normalize_kind(raw: Any) -> str:
    kind = str(raw or "").strip().lower().replace("-", "_").replace(" ", "_")
    if kind in BLOCK_KINDS:
        return kind
    return _KIND_ALIASES.get(kind, "text")


@dataclass
class _RawBlock:
    """One block as returned by the per-page vision call, in page coordinates."""

    kind: str
    text: str
    bbox: PointBBox
    latex: str = ""
    original_text: str = ""
    equation_number: str | None = None
    caption: str = ""
    table: dict[str, Any] | None = None


@dataclass
class _PageResult:
    page: int
    width: float
    height: float
    blocks: list[_RawBlock] = field(default_factory=list)


def _extract_raw_blocks(
    payload: Any, *, page: int, page_width: float, page_height: float
) -> list[_RawBlock]:
    """Normalize the page-call JSON into :class:`_RawBlock` values."""
    if not isinstance(payload, dict):
        raise VisionError("vision response is not a JSON object")
    entries = payload.get("blocks")
    if entries is None:
        entries = [payload] if "kind" in payload else []
    if not isinstance(entries, list):
        raise VisionError("vision response blocks is not a list")
    blocks: list[_RawBlock] = []
    for entry in entries:
        if not isinstance(entry, dict):
            continue
        kind = _normalize_kind(entry.get("kind") or entry.get("label") or entry.get("type"))
        bbox_value = entry.get("bbox") or entry.get("box") or entry.get("bounding_box")
        if bbox_value is None:
            bbox = PointBBox(0.0, 0.0, page_width, page_height)
        else:
            try:
                bbox = points_to_bbox(
                    bbox_value, page_width=page_width, page_height=page_height
                )
            except ParserConversionError:
                # A single malformed box must not fail the page; fall back to
                # the whole page instead of dropping the recognized content.
                bbox = PointBBox(0.0, 0.0, page_width, page_height)
        table = entry.get("table")
        if kind == "table" and not isinstance(table, dict):
            table = entry if "rows" in entry else None
        raw_number = entry.get("equation_number")
        blocks.append(
            _RawBlock(
                kind=kind,
                text=str(entry.get("text") or entry.get("content") or "").strip(),
                bbox=bbox,
                latex=str(entry.get("latex") or "").strip(),
                original_text=str(
                    entry.get("original_text") or entry.get("original") or ""
                ).strip(),
                equation_number=raw_number if isinstance(raw_number, str) else None,
                caption=str(entry.get("caption") or "").strip(),
                table=table if isinstance(table, dict) else None,
            )
        )
    return blocks


def _explicit_equation_number(raw: str | None) -> str | None:
    value = (raw or "").strip().strip("()").strip()
    return value if value and _EXPLICIT_EQUATION_NUMBER.fullmatch(value) else None


# ---------------------------------------------------------------------------
# Table normalization
# ---------------------------------------------------------------------------


def _cell_text(cell: Any) -> str:
    if isinstance(cell, dict):
        return str(cell.get("text") or "").strip()
    if cell is None:
        return ""
    return str(cell).strip()


def _normalize_table_structure(raw: Any, *, page: int) -> dict[str, Any] | None:
    """Coerce a model-provided table payload into the manifest table contract.

    Returns ``None`` when no usable grid is present, so the converter can retry
    with a cropped table image. Rows are padded/truncated so every row width
    equals ``num_cols`` — the Go contract and :func:`validate_manifest` require it.
    """
    if not isinstance(raw, dict):
        return None
    rows_raw = raw.get("rows")
    if not isinstance(rows_raw, list) or not rows_raw:
        return None
    rows: list[list[str]] = []
    for row in rows_raw:
        if isinstance(row, dict):
            row = row.get("cells")
        if not isinstance(row, list):
            return None
        rows.append([_cell_text(cell) for cell in row])
    width = max((len(row) for row in rows), default=0)
    if width < 1:
        return None
    declared_cols = raw.get("num_cols")
    if isinstance(declared_cols, int) and not isinstance(declared_cols, bool) and declared_cols > 0:
        width = declared_cols
    rows = [(row + [""] * width)[:width] for row in rows]
    declared_rows = raw.get("num_rows")
    if (
        isinstance(declared_rows, int)
        and not isinstance(declared_rows, bool)
        and 0 < declared_rows < len(rows)
    ):
        rows = rows[:declared_rows]
    num_rows = len(rows)
    num_cols = width

    header_rows = _normalize_table_rows(raw.get("header_rows"), num_cols)
    if not header_rows:
        header_rows = [list(rows[0])]
    headers_raw = raw.get("headers")
    if isinstance(headers_raw, list) and headers_raw:
        headers = [_cell_text(value) for value in headers_raw][:num_cols]
        headers += [""] * (num_cols - len(headers))
    else:
        headers = [
            "\n".join(dict.fromkeys(row[column] for row in header_rows if row[column]))
            for column in range(num_cols)
        ]
    footnotes_raw = raw.get("footnotes")
    footnotes = (
        [_cell_text(value) for value in footnotes_raw if _cell_text(value)]
        if isinstance(footnotes_raw, list)
        else []
    )
    orientation_raw = raw.get("orientation")
    orientation = (
        orientation_raw.strip().lower()
        if isinstance(orientation_raw, str) and orientation_raw.strip().lower() in _TABLE_ORIENTATIONS
        else "horizontal"
    )
    cells: list[dict[str, Any]] = []
    header_row_count = len(header_rows)
    for row_index in range(num_rows):
        for column in range(num_cols):
            cells.append(
                {
                    "text": rows[row_index][column],
                    "row_span": 1,
                    "col_span": 1,
                    "start_row_offset_idx": row_index,
                    "end_row_offset_idx": row_index + 1,
                    "start_col_offset_idx": column,
                    "end_col_offset_idx": column + 1,
                    "column_header": row_index < header_row_count,
                    "row_header": False,
                    "row_section": False,
                }
            )
    return {
        "num_rows": num_rows,
        "num_cols": num_cols,
        "orientation": orientation,
        "headers": headers,
        "header_rows": header_rows,
        "rows": rows,
        "cells": cells,
        "footnotes": footnotes,
    }


def _normalize_table_rows(raw: Any, num_cols: int) -> list[list[str]]:
    if not isinstance(raw, list):
        return []
    rows: list[list[str]] = []
    for row in raw:
        if not isinstance(row, list):
            continue
        cells = [_cell_text(value) for value in row]
        rows.append((cells + [""] * num_cols)[:num_cols])
    return rows


def _validate_table_structure(table: dict[str, Any], reference: str) -> None:
    rows = table["rows"]
    headers = table["headers"]
    num_cols = int(table["num_cols"])
    if not rows or num_cols < 1:
        raise ParserConversionError(f"Table structure is empty: {reference}")
    if num_cols >= 8:
        header_count = sum(bool(str(value).strip()) for value in headers)
        leading = rows[: min(3, len(rows))]
        occupied = sum(bool(str(value).strip()) for row in leading for value in row)
        capacity = max(1, len(leading) * num_cols)
        if header_count < max(2, num_cols // 3) and occupied / capacity < 0.4:
            raise ParserConversionError(
                f"Wide table structure is unreliable after rotation/OCR: {reference}"
            )


# ---------------------------------------------------------------------------
# Caption / footnote association
# ---------------------------------------------------------------------------


def _horizontal_overlap(left: PointBBox, right: PointBBox) -> bool:
    overlap = min(left.r, right.r) - max(left.l, right.l)
    return overlap > 0.2 * max(1.0, min(left.width, right.width))


def _vertical_gap(block: PointBBox, asset: PointBBox) -> float:
    if block.t >= asset.b:
        return block.t - asset.b
    if asset.t >= block.b:
        return asset.t - block.b
    return 0.0


def _caption_owner(
    block: PointBBox, assets: list[tuple[PointBBox, dict[str, Any]]]
) -> dict[str, Any] | None:
    """Return the unique same-page asset a caption/footnote belongs to."""
    best: dict[str, Any] | None = None
    best_distance: float | None = None
    ambiguous = False
    for bbox, asset in assets:
        if not _horizontal_overlap(block, bbox):
            continue
        distance = _vertical_gap(block, bbox)
        if distance > max(36.0, 3.0 * max(block.height, 1.0)):
            continue
        if best is None or best_distance is None or distance < best_distance - 1e-6:
            best = asset
            best_distance = distance
            ambiguous = False
        elif abs(distance - best_distance) <= 1e-6:
            ambiguous = True
    return best if best is not None and not ambiguous else None


# ---------------------------------------------------------------------------
# Real vision converter
# ---------------------------------------------------------------------------


class VisionConverter:
    """Local PyMuPDF + remote OpenAI-compatible vision conversion."""

    name = "vision"

    def __init__(
        self,
        vlm: VisionSettings | None,
        *,
        client_factory: Any | None = None,
        page_dpi: int = DEFAULT_PAGE_DPI,
        max_tokens: int = DEFAULT_MAX_TOKENS,
        prompt_version: str = DEFAULT_PROMPT_VERSION,
        attempts: int = 2,
    ) -> None:
        self.vlm = vlm
        self.client_factory = client_factory or (lambda settings: VisionClient(settings))
        self.page_dpi = int(page_dpi)
        self.max_tokens = int(max_tokens)
        self.prompt_version = prompt_version or DEFAULT_PROMPT_VERSION
        self.attempts = max(1, int(attempts))

    def convert(
        self,
        source: Path,
        output_dir: Path,
        scratch_dir: Path,
        *,
        options: ParseOptions | None = None,
    ) -> dict[str, Any]:
        requested = options if options is not None else ParseOptions()
        vlm = requested.vision_or(self.vlm)
        if vlm is None:
            raise ParserConfigError(NOT_CONFIGURED_MESSAGE)
        started = time.perf_counter()
        output_dir.mkdir(parents=True, exist_ok=True)
        scratch_dir.mkdir(parents=True, exist_ok=True)
        picture_dir = output_dir / "pictures"
        picture_dir.mkdir(parents=True, exist_ok=True)

        client = self.client_factory(vlm)
        session = VisionSession(client, attempts=self.attempts)
        renderer = PdfRenderer(source, dpi=self.page_dpi)
        try:
            page_results = self._read_pages(renderer, session, vlm)
            manifest = self._assemble(page_results, renderer, session, vlm, picture_dir)
        finally:
            close = getattr(client, "close", None)
            if callable(close):
                close()
            renderer.close()
        manifest["metrics"] = {
            "pages": renderer.page_count,
            "vision_requests": session.requests,
            "vision_failures": session.failures,
            "page_dpi": self.page_dpi,
            "elapsed_seconds": round(time.perf_counter() - started, 3),
        }
        return finalize_manifest(manifest)

    # -- page calls -------------------------------------------------------

    def _read_pages(
        self, renderer: PdfRenderer, session: VisionSession, vlm: VisionSettings
    ) -> list[_PageResult]:
        page_count = renderer.page_count
        workers = min(max(1, vlm.concurrency), page_count)
        if workers <= 1:
            return [
                self._read_page(renderer, session, index) for index in range(page_count)
            ]
        futures = []
        with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="pdf-vision") as executor:
            futures = [
                executor.submit(self._read_page, renderer, session, index)
                for index in range(page_count)
            ]
            results: list[_PageResult] = []
            try:
                for future in futures:
                    results.append(future.result())
            except BaseException:
                for future in futures:
                    future.cancel()
                raise
        return results

    def _read_page(
        self, renderer: PdfRenderer, session: VisionSession, index: int
    ) -> _PageResult:
        page = index + 1
        try:
            rendered: RenderedPage = renderer.page_jpeg(index)
        except ParserConversionError:
            raise
        except Exception as error:  # corrupt page stream
            raise ParserConversionError(f"PDF 解析失败：第 {page} 页渲染失败") from error
        payload = session.json(
            PAGE_PROMPT, rendered.jpeg, page=page, max_tokens=self.max_tokens
        )
        blocks = _extract_raw_blocks(
            payload, page=page, page_width=rendered.width, page_height=rendered.height
        )
        return _PageResult(page=page, width=rendered.width, height=rendered.height, blocks=blocks)

    # -- manifest assembly -------------------------------------------------

    def _assemble(
        self,
        page_results: list[_PageResult],
        renderer: PdfRenderer,
        session: VisionSession,
        vlm: VisionSettings,
        picture_dir: Path,
    ) -> dict[str, Any]:
        blocks: list[dict[str, Any]] = []
        assets: list[dict[str, Any]] = []
        warnings: list[dict[str, Any]] = []
        counters = {"text": 0, "picture": 0, "table": 0}
        # Pictures are described after caption association: a caption block
        # found on the same page wins over a generated description.
        pending_descriptions: list[tuple[dict[str, Any], int, PointBBox]] = []

        def add_block(block: dict[str, Any]) -> dict[str, Any]:
            block["order"] = len(blocks)
            blocks.append(block)
            return block

        for result in page_results:
            page_assets: list[tuple[PointBBox, dict[str, Any]]] = []
            pending_annotations: list[tuple[dict[str, Any], str]] = []
            if not result.blocks:
                warnings.append(
                    {
                        "page": result.page,
                        "kind": "empty_page",
                        "count": 1,
                        "message": f"第 {result.page} 页未识别到文本、表格或图片",
                    }
                )
                continue
            for raw in result.blocks:
                if raw.kind == "picture":
                    asset, block = self._build_picture(raw, result, renderer, picture_dir, counters)
                    page_assets.append((raw.bbox, asset))
                    assets.append(asset)
                    add_block(block)
                    pending_descriptions.append((asset, result.page, raw.bbox))
                    continue
                if raw.kind == "table":
                    asset, block = self._build_table(raw, result, renderer, session, counters)
                    page_assets.append((raw.bbox, asset))
                    assets.append(asset)
                    add_block(block)
                    continue
                block = _build_text_block(raw, result, counters)
                if block is None:
                    continue
                add_block(block)
                if raw.kind in {"caption", "footnote"}:
                    pending_annotations.append((block, raw.kind))
            for block, kind in pending_annotations:
                owner = _caption_owner(block.pop("_bbox_points"), page_assets)
                if owner is None:
                    block["association"] = "unlinked"
                    continue
                block["association"] = "document"
                block["associated_assets"] = [owner["asset_id"]]
                owner["captions" if kind == "caption" else "footnotes"].append(
                    {
                        "ref": block["ref"],
                        "text": block["text"],
                        "page": block["page"],
                        "bbox": block["bbox"],
                        "locations": block["locations"],
                    }
                )
                if kind == "caption" and not owner["caption"]:
                    owner["caption"] = block["text"]
                    owner["caption_source"] = "document"

        # Describe only pictures that still have no caption (document captions
        # always win over a generated description).
        for asset, page, bbox in pending_descriptions:
            if asset["caption"]:
                continue
            description = session.text(
                DESCRIPTION_PROMPT,
                renderer.crop_jpeg(page - 1, bbox),
                page=page,
                max_tokens=min(self.max_tokens, 1024),
            )
            asset["generated_description"] = clean_description(description)

        return {
            "version": SERVICE_PARSER_VERSION,
            "blocks": blocks,
            "assets": assets,
            "warnings": warnings,
            "parser": self._parser_metadata(vlm),
            "metrics": {},
        }

    def _build_picture(
        self,
        raw: _RawBlock,
        result: _PageResult,
        renderer: PdfRenderer,
        picture_dir: Path,
        counters: dict[str, int],
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        index = counters["picture"]
        counters["picture"] += 1
        ref = f"#/pictures/{index}"
        image_name = f"picture-{index}.png"
        image_file = f"pictures/{image_name}"
        png = renderer.crop_png(result.page - 1, raw.bbox)
        (picture_dir / image_name).write_bytes(png)
        asset: dict[str, Any] = {
            "asset_id": ref,
            "kind": "picture",
            "page": result.page,
            "bbox": raw.bbox.as_manifest(),
            "caption": raw.caption,
            "caption_source": "document" if raw.caption else "none",
            "generated_description": "",
            "image_file": image_file,
            "table": None,
            "captions": [],
            "footnotes": [],
        }
        return asset, _asset_block(ref, "picture", result.page, raw.bbox)

    def _build_table(
        self,
        raw: _RawBlock,
        result: _PageResult,
        renderer: PdfRenderer,
        session: VisionSession,
        counters: dict[str, int],
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        page = result.page
        index = counters["table"]
        counters["table"] += 1
        ref = f"#/tables/{index}"
        table = _normalize_table_structure(raw.table, page=page)
        if table is None:
            payload = session.json(
                TABLE_PROMPT,
                renderer.crop_jpeg(page - 1, raw.bbox),
                page=page,
                max_tokens=min(self.max_tokens, 2048),
            )
            candidate = payload.get("table") if isinstance(payload.get("table"), dict) else payload
            table = _normalize_table_structure(candidate, page=page)
        if table is None:
            raise ParserConversionError(TABLE_FAILED_MESSAGE.format(page=page))
        try:
            _validate_table_structure(table, ref)
        except ParserConversionError as error:
            raise ParserConversionError(TABLE_FAILED_MESSAGE.format(page=page)) from error
        asset: dict[str, Any] = {
            "asset_id": ref,
            "kind": "table",
            "page": page,
            "bbox": raw.bbox.as_manifest(),
            "caption": raw.caption,
            "caption_source": "document" if raw.caption else "none",
            "generated_description": "",
            "image_file": None,
            "table": table,
            "captions": [],
            "footnotes": [],
        }
        return asset, _asset_block(ref, "table", page, raw.bbox)

    def _parser_metadata(self, vlm: VisionSettings) -> dict[str, Any]:
        """Provenance recorded in ``manifest.parser``.

        The endpoint description carries the model and host only — never the key.
        """
        return {
            "version": SERVICE_PARSER_VERSION,
            "service_version": __version__,
            "service": SERVICE_NAME,
            "converter": "vision",
            "backend": BACKEND,
            "model": vlm.model,
            "endpoint": vlm.endpoint_host,
            "prompt_version": self.prompt_version,
            "page_dpi": self.page_dpi,
            "capabilities": [
                "layout",
                "ocr",
                "table_structure",
                "formula_latex",
                "picture_description",
            ],
        }


def _asset_block(ref: str, kind: str, page: int, bbox: PointBBox) -> dict[str, Any]:
    """Block that points at an asset; its order is assigned by the assembler."""
    return {
        "ref": ref,
        "kind": kind,
        "page": page,
        "order": 0,
        "text": "",
        "asset_id": ref,
        "bbox": bbox.as_manifest(),
        "locations": [{"page": page, "bbox": bbox.as_manifest(), "charspan": [0, 0]}],
        "latex": "",
        "original_text": "",
        "equation_number": None,
        "association": "",
        "associated_assets": [],
    }


def _build_text_block(
    raw: _RawBlock,
    result: _PageResult,
    counters: dict[str, int],
) -> dict[str, Any] | None:
    """Build a non-asset block; ``None`` means "nothing usable, skip it"."""
    page = result.page
    bbox = raw.bbox
    latex = ""
    original_text = ""
    equation_number: str | None = None
    if raw.kind == "formula":
        latex = raw.latex
        if not latex:
            raise ParserConversionError(FORMULA_EMPTY_MESSAGE.format(page=page))
        original_text = raw.original_text or raw.text
        equation_number = (
            parse_equation_number(original_text)
            or parse_equation_number(latex)
            or _explicit_equation_number(raw.equation_number)
        )
        text = render_formula_text(latex, equation_number)
    else:
        text = raw.text
    if not text.strip():
        return None
    ref = f"#/texts/{counters['text']}"
    counters["text"] += 1
    return {
        "ref": ref,
        "kind": raw.kind,
        "page": page,
        "order": 0,
        "text": text,
        "asset_id": "",
        "bbox": bbox.as_manifest(),
        "locations": [{"page": page, "bbox": bbox.as_manifest(), "charspan": [0, len(text)]}],
        "latex": latex,
        "original_text": original_text,
        "equation_number": equation_number,
        "association": "",
        "associated_assets": [],
        "_bbox_points": bbox,
    }


# ---------------------------------------------------------------------------
# Fake converter (tests / Go integration without a vision endpoint)
# ---------------------------------------------------------------------------

_FAKE_BBOX: dict[str, Any] = {
    "l": 72.0,
    "t": 72.0,
    "r": 540.0,
    "b": 132.0,
    "coord_origin": "TOPLEFT",
}
_FAKE_TEXT = (
    "Praxis-RAG PDF service fake mode. "
    "Formula recognition and picture extraction are simulated for local testing."
)
FAKE_LATEX = "G = X_{basis}^{T} X_{basis}"
FAKE_EQUATION_NUMBER = "1"


def _fake_png_bytes(size: int = 8) -> bytes:
    """Render a small but real PNG with PyMuPDF (no Pillow dependency)."""
    import pymupdf

    document = pymupdf.open()
    try:
        page = document.new_page(width=size, height=size)
        page.draw_line((0, 0), (size, size), color=(0, 0, 0), width=0.5)
        page.draw_line((0, size), (size, 0), color=(0.8, 0.24, 0.24), width=0.5)
        return page.get_pixmap(colorspace=pymupdf.csRGB, alpha=False).tobytes("png")
    finally:
        document.close()


def write_fake_png(path: Path, size: int = 8) -> None:
    """Write a small real PNG for the fake manifest's picture asset."""
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(_fake_png_bytes(size))


def fake_parser_metadata() -> dict[str, Any]:
    """``manifest.parser`` for the fake converter (no model, no endpoint)."""
    return {
        "version": SERVICE_PARSER_VERSION,
        "service_version": __version__,
        "service": SERVICE_NAME,
        "converter": "fake",
        "backend": "local",
        "model": None,
        "endpoint": None,
        "prompt_version": None,
        "page_dpi": None,
        "capabilities": [],
    }


def fake_manifest() -> dict[str, Any]:
    """The fixed fake-mode manifest (also used by contract unit tests)."""
    text = _FAKE_TEXT
    formula_text = render_formula_text(FAKE_LATEX, FAKE_EQUATION_NUMBER)
    return {
        "version": SERVICE_PARSER_VERSION,
        "blocks": [
            {
                "ref": "#/texts/0",
                "kind": "text",
                "page": 1,
                "order": 0,
                "text": text,
                "asset_id": "",
                "bbox": dict(_FAKE_BBOX),
                "locations": [{"page": 1, "bbox": dict(_FAKE_BBOX), "charspan": [0, len(text)]}],
                "latex": "",
                "original_text": "",
                "equation_number": None,
                "association": "",
                "associated_assets": [],
            },
            {
                "ref": "#/texts/1",
                "kind": "formula",
                "page": 1,
                "order": 1,
                "text": formula_text,
                "asset_id": "",
                "bbox": dict(_FAKE_BBOX),
                "locations": [
                    {"page": 1, "bbox": dict(_FAKE_BBOX), "charspan": [0, len(formula_text)]}
                ],
                "latex": FAKE_LATEX,
                "original_text": "G = X_basis^T X_basis (1)",
                "equation_number": FAKE_EQUATION_NUMBER,
                "association": "",
                "associated_assets": [],
            },
            {
                "ref": "#/pictures/0",
                "kind": "picture",
                "page": 1,
                "order": 2,
                "text": "",
                "asset_id": "#/pictures/0",
                "bbox": dict(_FAKE_BBOX),
                "locations": [{"page": 1, "bbox": dict(_FAKE_BBOX), "charspan": [0, 0]}],
                "latex": "",
                "original_text": "",
                "equation_number": None,
                "association": "",
                "associated_assets": [],
            },
        ],
        "assets": [
            {
                "asset_id": "#/pictures/0",
                "kind": "picture",
                "page": 1,
                "bbox": dict(_FAKE_BBOX),
                "caption": "Figure 1: fake picture used by the PDF service fake converter",
                "caption_source": "document",
                "generated_description": "",
                "image_file": "pictures/picture-0.png",
                "table": None,
                "captions": [
                    {
                        "ref": "#/texts/2",
                        "text": "Figure 1: fake picture used by the PDF service fake converter",
                        "page": 1,
                        "bbox": dict(_FAKE_BBOX),
                        "locations": [{"page": 1, "bbox": dict(_FAKE_BBOX), "charspan": [0, 0]}],
                    }
                ],
                "footnotes": [],
            }
        ],
        "warnings": [],
        "parser": fake_parser_metadata(),
        "metrics": {
            "pages": 1,
            "vision_requests": 0,
            "vision_failures": 0,
            "page_dpi": None,
            "elapsed_seconds": 0.0,
        },
    }


class FakeConverter:
    """Deterministic converter for tests and Go integration without a vision model.

    Produces one text block, one formula block and one picture asset backed by a
    real 8x8 PNG, so the zip/manifest contract can be exercised end to end on a
    machine without any remote endpoint. Fake mode ignores per-request options
    (there is no vision pipeline to configure) but keeps the last payload in
    :attr:`last_options` so tests can assert the HTTP layer parsed and forwarded
    it.
    """

    name = "fake"

    def __init__(self, *, picture_size: int = 8) -> None:
        self.picture_size = picture_size
        self.last_options: ParseOptions | None = None

    def convert(
        self,
        source: Path,
        output_dir: Path,
        scratch_dir: Path,
        *,
        options: ParseOptions | None = None,
    ) -> dict[str, Any]:
        self.last_options = options
        started = time.perf_counter()
        picture_dir = output_dir / "pictures"
        picture_dir.mkdir(parents=True, exist_ok=True)
        write_fake_png(picture_dir / "picture-0.png", self.picture_size)
        manifest = fake_manifest()
        manifest["metrics"] = {
            "pages": 1,
            "vision_requests": 0,
            "vision_failures": 0,
            "page_dpi": None,
            "elapsed_seconds": round(time.perf_counter() - started, 3),
        }
        return manifest


def build_converter(
    mode: str,
    *,
    vlm: VisionSettings | None,
    page_dpi: int = DEFAULT_PAGE_DPI,
    max_tokens: int = DEFAULT_MAX_TOKENS,
    prompt_version: str = DEFAULT_PROMPT_VERSION,
) -> PdfConverter:
    """Select the converter from ``RAG_PDF_SERVICE_CONVERTER``.

    ``vision`` is the default; ``docling`` is accepted as a legacy alias for the
    same remote-vision converter so old deployments keep working.
    """
    normalized = (mode or "").strip().lower()
    if normalized in {"", "vision", "docling"}:
        return VisionConverter(
            vlm,
            page_dpi=page_dpi,
            max_tokens=max_tokens,
            prompt_version=prompt_version,
        )
    if normalized == "fake":
        return FakeConverter()
    raise ParserConfigError(
        f"Unknown RAG_PDF_SERVICE_CONVERTER: {mode!r}; expected 'vision' or 'fake'"
    )


def finalize_manifest(manifest: dict[str, Any]) -> dict[str, Any]:
    """Strip internal bookkeeping keys from the manifest."""
    for block in manifest.get("blocks", []):
        block.pop("_bbox_points", None)
    for asset in manifest.get("assets", []):
        asset.pop("_vision_path", None)
        asset.pop("_vision_jpeg", None)
    return manifest


# ---------------------------------------------------------------------------
# Contract validation
# ---------------------------------------------------------------------------


def validate_manifest(manifest: dict[str, Any], artifacts_dir: Path) -> None:
    """Reject a manifest that would break the Go ``PDFManifest`` contract.

    This is deliberately strict: the service owns the JSON contract, so a
    silently renamed or missing key would otherwise surface as an empty field in
    the Go backend long after the upload succeeded.
    """
    if not isinstance(manifest, dict):
        raise ParserConversionError("Parser produced an invalid manifest: expected an object")
    _require_keys(manifest, _MANIFEST_KEYS, "manifest")
    if manifest["version"] != SERVICE_PARSER_VERSION:
        raise ParserConversionError(
            f"Parser produced manifest version {manifest['version']!r}; "
            f"expected {SERVICE_PARSER_VERSION!r}"
        )
    blocks = manifest["blocks"]
    assets = manifest["assets"]
    if not isinstance(blocks, list) or not isinstance(assets, list):
        raise ParserConversionError("Parser manifest blocks/assets must be lists")
    for key in ("parser", "metrics"):
        if not isinstance(manifest[key], dict):
            raise ParserConversionError(f"Parser manifest {key} must be an object")

    asset_ids: dict[str, dict[str, Any]] = {}
    for asset in assets:
        _validate_asset(asset, artifacts_dir, asset_ids)

    for index, block in enumerate(blocks):
        _validate_block(block, index, asset_ids)

    warnings = manifest["warnings"]
    if not isinstance(warnings, list):
        raise ParserConversionError("Parser manifest warnings must be a list")
    for warning in warnings:
        if not isinstance(warning, dict):
            raise ParserConversionError("Parser warning entries must be objects")
        _require_keys(warning, _WARNING_KEYS, "warning")
        if not isinstance(warning["page"], int) or warning["page"] < 1:
            raise ParserConversionError("Parser warning page must be a positive integer")
        if not isinstance(warning["kind"], str) or not warning["kind"]:
            raise ParserConversionError("Parser warning kind must be a non-empty string")
        if not isinstance(warning["count"], int) or warning["count"] < 0:
            raise ParserConversionError("Parser warning count must be a non-negative integer")
        if not isinstance(warning["message"], str):
            raise ParserConversionError("Parser warning message must be a string")


def _validate_asset(
    asset: Any, artifacts_dir: Path, asset_ids: dict[str, dict[str, Any]]
) -> None:
    if not isinstance(asset, dict):
        raise ParserConversionError("Parser asset entries must be objects")
    _require_keys(asset, _ASSET_KEYS, "asset")
    asset_id = asset["asset_id"]
    if not isinstance(asset_id, str) or not asset_id:
        raise ParserConversionError("Parser asset_id must be a non-empty string")
    if asset_id in asset_ids:
        raise ParserConversionError(f"Parser produced a duplicate asset_id: {asset_id}")
    asset_ids[asset_id] = asset
    if asset["kind"] not in {"picture", "table"}:
        raise ParserConversionError(f"Parser asset kind must be picture or table: {asset['kind']!r}")
    if not isinstance(asset["page"], int) or asset["page"] < 1:
        raise ParserConversionError(f"Parser asset has an invalid page: {asset_id}")
    if not isinstance(asset["bbox"], dict):
        raise ParserConversionError(f"Parser asset has an invalid bbox: {asset_id}")
    for key in ("caption", "caption_source", "generated_description"):
        if not isinstance(asset[key], str):
            raise ParserConversionError(f"Parser asset {key} must be a string: {asset_id}")
    for key in ("captions", "footnotes"):
        entries = asset[key]
        if not isinstance(entries, list):
            raise ParserConversionError(f"Parser asset {key} must be a list: {asset_id}")
        for entry in entries:
            if not isinstance(entry, dict):
                raise ParserConversionError(f"Parser asset {key} entries must be objects: {asset_id}")
            _require_keys(entry, _ANNOTATION_KEYS, f"asset {key}")
            if not isinstance(entry["ref"], str) or not entry["ref"]:
                raise ParserConversionError(f"Parser asset {key} ref must be a string: {asset_id}")
            if not isinstance(entry["text"], str):
                raise ParserConversionError(f"Parser asset {key} text must be a string: {asset_id}")
            _validate_page_bbox(entry, f"asset {key}")
            locations = entry["locations"]
            if not isinstance(locations, list):
                raise ParserConversionError(
                    f"Parser asset {key} locations must be a list: {asset_id}"
                )
            for location in locations:
                if not isinstance(location, dict):
                    raise ParserConversionError(
                        f"Parser asset {key} locations must be objects: {asset_id}"
                    )
                _require_keys(location, _LOCATION_KEYS, f"asset {key} location")
                _validate_location_fields(location, f"asset {key} location")

    image_file = asset["image_file"]
    table = asset["table"]
    if asset["kind"] == "picture":
        if not isinstance(image_file, str) or not image_file:
            raise ParserConversionError(f"Picture asset has no image_file: {asset_id}")
        _require_readable_asset(artifacts_dir, image_file, asset_id)
        if table is not None:
            raise ParserConversionError(f"Picture asset must have a null table: {asset_id}")
    else:
        if image_file is not None:
            raise ParserConversionError(
                f"Table asset image_file must be null (tables keep structure only): {asset_id}"
            )
        _validate_table_payload(table, asset_id)


def _require_readable_asset(artifacts_dir: Path, relative: str, asset_id: str) -> None:
    if Path(relative).is_absolute() or "\\" in relative or ".." in Path(relative).parts:
        raise ParserConversionError(f"Picture asset image_file must be a safe relative path: {asset_id}")
    path = artifacts_dir / Path(relative)
    if not path.is_file():
        raise ParserConversionError(f"Picture asset image is missing: {relative}")
    if path.stat().st_size == 0:
        raise ParserConversionError(f"Picture asset image is empty: {relative}")


def _validate_table_payload(table: Any, asset_id: str) -> None:
    if not isinstance(table, dict):
        raise ParserConversionError(f"Table asset has no table payload: {asset_id}")
    _require_keys(table, _TABLE_KEYS, "table")
    for key in ("num_rows", "num_cols"):
        if not isinstance(table[key], int) or table[key] < 1:
            raise ParserConversionError(f"Table asset has an invalid {key}: {asset_id}")
    if not isinstance(table["orientation"], str):
        raise ParserConversionError(f"Table asset orientation must be a string: {asset_id}")
    rows = table["rows"]
    if not isinstance(rows, list) or not rows:
        raise ParserConversionError(f"Table structure is empty: {asset_id}")
    if len(rows) != table["num_rows"]:
        raise ParserConversionError(f"Table row count does not match num_rows: {asset_id}")
    for row in rows:
        if not isinstance(row, list) or len(row) != table["num_cols"]:
            raise ParserConversionError(f"Table row width does not match num_cols: {asset_id}")
        if not all(isinstance(cell, str) for cell in row):
            raise ParserConversionError(f"Table cells must be strings: {asset_id}")
    for key in ("headers", "footnotes"):
        values = table[key]
        if not isinstance(values, list) or not all(isinstance(value, str) for value in values):
            raise ParserConversionError(f"Table asset {key} must be a list of strings: {asset_id}")
    for row in table["header_rows"]:
        if not isinstance(row, list) or not all(isinstance(cell, str) for cell in row):
            raise ParserConversionError(f"Table asset header_rows must be string rows: {asset_id}")
    if not isinstance(table["cells"], list) or not all(
        isinstance(cell, dict) for cell in table["cells"]
    ):
        raise ParserConversionError(f"Table asset cells must be objects: {asset_id}")


def _validate_block(block: Any, index: int, asset_ids: dict[str, dict[str, Any]]) -> None:
    if not isinstance(block, dict):
        raise ParserConversionError("Parser block entries must be objects")
    _require_keys(block, _BLOCK_KEYS, "block")
    if not isinstance(block["kind"], str) or not block["kind"]:
        raise ParserConversionError(f"Parser block kind must be a non-empty string: index {index}")
    if not isinstance(block["page"], int) or block["page"] < 1:
        raise ParserConversionError(f"Parser block has an invalid page: index {index}")
    if block["order"] != index:
        raise ParserConversionError(f"Parser block order is not sequential: index {index}")
    if not isinstance(block["text"], str) or not isinstance(block["latex"], str):
        raise ParserConversionError(f"Parser block text/latex must be strings: index {index}")
    if not isinstance(block["original_text"], str):
        raise ParserConversionError(f"Parser block original_text must be a string: index {index}")
    if block["equation_number"] is not None and not isinstance(block["equation_number"], str):
        raise ParserConversionError(f"Parser block equation_number must be a string or null: index {index}")
    if not isinstance(block["association"], str):
        raise ParserConversionError(f"Parser block association must be a string: index {index}")
    if block["association"] not in {"", "document", "unlinked"}:
        raise ParserConversionError(f"Parser block has an invalid association: index {index}")
    if not isinstance(block["bbox"], dict):
        raise ParserConversionError(f"Parser block has an invalid bbox: index {index}")
    locations = block["locations"]
    if not isinstance(locations, list):
        raise ParserConversionError(f"Parser block locations must be a list: index {index}")
    for location in locations:
        if not isinstance(location, dict):
            raise ParserConversionError(f"Parser block locations must be objects: index {index}")
        _require_keys(location, _LOCATION_KEYS, "location")
        _validate_location_fields(location, f"block location (index {index})")
    associated = block["associated_assets"]
    if not isinstance(associated, list) or not all(isinstance(value, str) for value in associated):
        raise ParserConversionError(f"Parser block associated_assets must be strings: index {index}")
    for related in associated:
        if related not in asset_ids:
            raise ParserConversionError(
                f"Parser block references an unknown asset: {related} (index {index})"
            )

    asset_id = block["asset_id"]
    if not isinstance(asset_id, str):
        raise ParserConversionError(f"Parser block asset_id must be a string: index {index}")
    if asset_id:
        asset = asset_ids.get(asset_id)
        if asset is None:
            raise ParserConversionError(
                f"Parser block references an unknown asset: {asset_id} (index {index})"
            )
        if block["text"]:
            raise ParserConversionError(
                f"Asset block text must stay empty so the Go renderer uses the asset payload: "
                f"{asset_id}"
            )
        if block["kind"] != asset["kind"]:
            raise ParserConversionError(
                f"Asset block kind does not match its asset: {asset_id}"
            )
        return
    if not block["text"].strip():
        raise ParserConversionError(f"Parser produced an empty text block: index {index}")
    if block["kind"] == "formula":
        if not block["latex"].strip():
            raise ParserConversionError(f"Formula block has empty latex: index {index}")
        if "$$" not in block["text"]:
            raise ParserConversionError(f"Formula block text is not a $$ segment: index {index}")
        if block["latex"] not in block["text"]:
            raise ParserConversionError(
                f"Formula block text does not contain its latex: index {index}"
            )


def _validate_page_bbox(value: dict[str, Any], label: str) -> None:
    if not isinstance(value["page"], int) or value["page"] < 1:
        raise ParserConversionError(f"Parser {label} has an invalid page")
    if not isinstance(value["bbox"], dict):
        raise ParserConversionError(f"Parser {label} has an invalid bbox")


def _validate_location_fields(location: dict[str, Any], label: str) -> None:
    _validate_page_bbox(location, label)
    charspan = location["charspan"]
    if not isinstance(charspan, list) or not all(isinstance(value, int) for value in charspan):
        raise ParserConversionError(f"Parser {label} charspan must be integers")


def _require_keys(value: dict[str, Any], expected: set[str], label: str) -> None:
    missing = expected - set(value)
    extra = set(value) - expected
    if missing:
        raise ParserConversionError(
            f"Parser {label} is missing required keys: {', '.join(sorted(missing))}"
        )
    if extra:
        raise ParserConversionError(
            f"Parser {label} has unexpected keys: {', '.join(sorted(extra))}"
        )
