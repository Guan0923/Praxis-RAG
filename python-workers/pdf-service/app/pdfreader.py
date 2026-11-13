"""Local PDF reading, page rendering and region cropping with PyMuPDF.

This is the only place the service touches the PDF itself; layout, OCR, table
and formula recognition happen remotely (:mod:`app.vision`). PyMuPDF documents
are **not** thread-safe, so :class:`PdfRenderer` opens one document per worker
thread (``threading.local``) and closes all of them on :meth:`PdfRenderer.close`.
"""

from __future__ import annotations

import threading
from dataclasses import dataclass
from pathlib import Path

import pymupdf

from .errors import ParserConversionError

#: Default render DPI for the page image sent to the vision model. 200 DPI is a
#: good trade-off between OCR fidelity and payload size; the Go backend can
#: override it with ``RAG_PDF_PAGE_DPI`` (72..300).
DEFAULT_PAGE_DPI = 200
MIN_PAGE_DPI = 72
MAX_PAGE_DPI = 300

#: Hard page bound: a bigger document must be split before it is parsed.
MAX_PAGES = 1000

#: JPEG quality of the image sent to the vision endpoint.
JPEG_QUALITY = 85


@dataclass(frozen=True)
class RenderedPage:
    """One rendered page image plus its size in PDF points."""

    width: float
    height: float
    jpeg: bytes


@dataclass(frozen=True)
class PointBBox:
    """A region in PDF points with the origin at the top-left corner."""

    l: float
    t: float
    r: float
    b: float

    @property
    def width(self) -> float:
        return max(0.0, self.r - self.l)

    @property
    def height(self) -> float:
        return max(0.0, self.b - self.t)

    def as_manifest(self) -> dict[str, object]:
        """JSON shape stored in ``block.bbox`` / ``asset.bbox`` / ``location.bbox``."""
        return {
            "l": round(self.l, 3),
            "t": round(self.t, 3),
            "r": round(self.r, 3),
            "b": round(self.b, 3),
            "coord_origin": "TOPLEFT",
        }


def clamp_bbox(bbox: PointBBox, width: float, height: float) -> PointBBox:
    """Clamp a bbox into the page and guarantee a non-degenerate rectangle."""
    left = min(max(bbox.l, 0.0), width)
    top = min(max(bbox.t, 0.0), height)
    right = min(max(bbox.r, 0.0), width)
    bottom = min(max(bbox.b, 0.0), height)
    if right - left < 1.0:
        right = min(left + 1.0, width)
        left = max(right - 1.0, 0.0)
    if bottom - top < 1.0:
        bottom = min(top + 1.0, height)
        top = max(bottom - 1.0, 0.0)
    return PointBBox(l=left, t=top, r=right, b=bottom)


def points_to_bbox(
    values: dict[str, float] | list[float] | tuple[float, ...],
    *,
    page_width: float,
    page_height: float,
) -> PointBBox:
    """Normalize a model-provided bbox into PDF points.

    The prompt asks for ``{"l","t","r","b"}`` normalized to ``[0,1]`` with the
    origin at the top-left. Absolute point coordinates are also accepted (a
    model occasionally echoes the page coordinates), and the result is always
    clamped into the page.
    """
    if isinstance(values, dict):
        try:
            raw = [float(values["l"]), float(values["t"]), float(values["r"]), float(values["b"])]
        except (KeyError, TypeError, ValueError) as error:
            raise ParserConversionError("视觉模型返回的 bbox 无效") from error
    elif isinstance(values, (list, tuple)) and len(values) == 4:
        try:
            raw = [float(value) for value in values]
        except (TypeError, ValueError) as error:
            raise ParserConversionError("视觉模型返回的 bbox 无效") from error
    else:
        raise ParserConversionError("视觉模型返回的 bbox 无效")
    if all(abs(value) <= 1.5 for value in raw):
        raw = [raw[0] * page_width, raw[1] * page_height, raw[2] * page_width, raw[3] * page_height]
    left, top, right, bottom = raw
    if right < left:
        left, right = right, left
    if bottom < top:
        top, bottom = bottom, top
    return clamp_bbox(PointBBox(left, top, right, bottom), page_width, page_height)


class PdfRenderer:
    """Render pages and crop regions of one PDF, one PyMuPDF document per thread."""

    def __init__(self, source: Path, *, dpi: int = DEFAULT_PAGE_DPI) -> None:
        if not MIN_PAGE_DPI <= int(dpi) <= MAX_PAGE_DPI:
            raise ParserConversionError(
                f"PDF 渲染 DPI 必须在 {MIN_PAGE_DPI}-{MAX_PAGE_DPI} 之间"
            )
        self.source = Path(source)
        self.dpi = int(dpi)
        self._local = threading.local()
        self._documents: list[pymupdf.Document] = []
        self._lock = threading.Lock()
        try:
            self._page_count = self._document().page_count
        except ParserConversionError:
            raise
        except Exception as error:  # corrupt/unreadable PDF
            raise ParserConversionError("PDF 文件无法打开，可能已损坏或加密") from error
        if self._page_count < 1:
            raise ParserConversionError("PDF 文件没有可解析的页面")
        if self._page_count > MAX_PAGES:
            raise ParserConversionError(f"PDF 页数超过上限（{MAX_PAGES} 页），请拆分后重试")

    @property
    def page_count(self) -> int:
        return self._page_count

    def _document(self) -> pymupdf.Document:
        document = getattr(self._local, "document", None)
        if document is None:
            document = pymupdf.open(str(self.source))
            if document.needs_pass:
                document.close()
                raise ParserConversionError("PDF 文件已加密，无法解析")
            self._local.document = document
            with self._lock:
                self._documents.append(document)
        return document

    def _page(self, index: int) -> pymupdf.Page:
        if not 0 <= index < self._page_count:
            raise ParserConversionError(f"PDF 页码超出范围：{index + 1}")
        return self._document()[index]

    def page_size(self, index: int) -> tuple[float, float]:
        rect = self._page(index).rect
        return float(rect.width), float(rect.height)

    def page_jpeg(self, index: int) -> RenderedPage:
        """Render the whole page as JPEG (the vision model input)."""
        page = self._page(index)
        rect = page.rect
        pixmap = page.get_pixmap(
            matrix=self._matrix(), colorspace=pymupdf.csRGB, alpha=False
        )
        return RenderedPage(
            width=float(rect.width),
            height=float(rect.height),
            jpeg=pixmap.tobytes("jpeg", jpg_quality=JPEG_QUALITY),
        )

    def crop_png(self, index: int, bbox: PointBBox) -> bytes:
        """Crop one region and encode it as PNG (the manifest picture asset)."""
        return self._crop(index, bbox).tobytes("png")

    def crop_jpeg(self, index: int, bbox: PointBBox) -> bytes:
        """Crop one region and encode it as JPEG (vision model input)."""
        return self._crop(index, bbox).tobytes("jpeg", jpg_quality=JPEG_QUALITY)

    def _crop(self, index: int, bbox: PointBBox) -> pymupdf.Pixmap:
        page = self._page(index)
        page_rect = page.rect
        clip = pymupdf.Rect(bbox.l, bbox.t, bbox.r, bbox.b) & page_rect
        if clip.is_empty or clip.width < 0.5 or clip.height < 0.5:
            raise ParserConversionError(f"第 {index + 1} 页裁图区域为空")
        return page.get_pixmap(
            matrix=self._matrix(), clip=clip, colorspace=pymupdf.csRGB, alpha=False
        )

    def _matrix(self) -> pymupdf.Matrix:
        zoom = self.dpi / 72.0
        return pymupdf.Matrix(zoom, zoom)

    def close(self) -> None:
        with self._lock:
            documents = list(self._documents)
            self._documents.clear()
        for document in documents:
            try:
                document.close()
            except Exception:  # closing must never mask a conversion failure
                pass

    def __enter__(self) -> "PdfRenderer":
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()
