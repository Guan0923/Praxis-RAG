"""Isolated Docling worker for PDF text, pictures, and tables.

Formula recognition and picture descriptions call a remote OpenAI-compatible
vision-language endpoint (base URL + API key + model name). Layout, table
structure and OCR still run locally through Docling; only the VLM stages leave
the machine.
"""

from __future__ import annotations

import argparse
import gc
import json
import os
import re
import time
from pathlib import Path
from urllib.parse import urlsplit

from .pdf_content import PDF_PARSER_VERSION

_DOCLING_VERSION = "2.126.0"
_FORMULA_REVISION = "ecedbe111d15c2dc60bfd4a823cbe80127b58af4"
_FORMULA_MAX_TOKENS = 1024
_DESCRIPTION_MAX_TOKENS = 256
_DESCRIPTION_TIMEOUT_FLOOR = 30.0
_DESCRIPTION_PROMPT = (
    "Describe the visible content for document retrieval in 2 to 4 concise sentences. "
    "Preserve visible names, labels and relationships. For a table, summarize its subject only; "
    "never invent cell values. End with a complete sentence."
)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--models", type=Path)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    manifest = convert(args.input.resolve(), args.output.resolve(), args.models.resolve() if args.models else None)
    args.manifest.write_text(json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8")


class VlmSettings:
    """Remote vision-language endpoint shared by formulas and descriptions."""

    def __init__(self, base_url: str, api_key: str, model: str, timeout: float, concurrency: int) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.model = model
        self.timeout = timeout
        self.concurrency = concurrency

    @property
    def chat_url(self) -> str:
        if self.base_url.endswith("/chat/completions"):
            return self.base_url
        if self.base_url.endswith("/v1"):
            return self.base_url + "/chat/completions"
        return self.base_url + "/v1/chat/completions"

    def headers(self) -> dict[str, str]:
        if not self.api_key:
            return {}
        return {"Authorization": f"Bearer {self.api_key}"}

    def engine_options(self, *, max_tokens: int | None = None):
        from docling.datamodel.vlm_engine_options import ApiVlmEngineOptions
        from docling.models.inference_engines.vlm import VlmEngineType

        params: dict[str, object] = {"model": self.model, "temperature": 0.0}
        if max_tokens:
            params["max_tokens"] = max_tokens
        return ApiVlmEngineOptions(
            engine_type=VlmEngineType.API,
            url=self.chat_url,
            headers=self.headers(),
            params=params,
            timeout=self.timeout,
            concurrency=self.concurrency,
        )

    def describe(self) -> dict[str, object]:
        # Record the model and host, never the API key.
        return {"backend": "remote", "model": self.model, "endpoint": urlsplit(self.base_url).netloc}


def vlm_settings() -> VlmSettings:
    """Read the remote VLM configuration from the environment.

    Fails instead of falling back to a local model: the worker has no local VLM
    weights, so a missing endpoint must be a configuration error.
    """
    base_url = os.environ.get("RAG_VLM_BASE_URL", "").strip()
    model = os.environ.get("RAG_VLM_MODEL", "").strip()
    if not base_url:
        raise RuntimeError("RAG_VLM_BASE_URL is not configured: formula recognition and picture "
                           "descriptions need a remote OpenAI-compatible VLM endpoint")
    if not model:
        raise RuntimeError("RAG_VLM_MODEL is not configured: the remote VLM endpoint needs a model name")
    try:
        timeout = float(os.environ.get("RAG_VLM_TIMEOUT", "") or 120.0)
    except ValueError as error:
        raise RuntimeError("RAG_VLM_TIMEOUT must be a number of seconds") from error
    try:
        concurrency = int(os.environ.get("RAG_VLM_CONCURRENCY", "") or 4)
    except ValueError as error:
        raise RuntimeError("RAG_VLM_CONCURRENCY must be an integer") from error
    return VlmSettings(base_url, os.environ.get("RAG_VLM_API_KEY", "").strip(), model,
                       max(timeout, _DESCRIPTION_TIMEOUT_FLOOR), max(concurrency, 1))


def _local_device():
    """Layout, table and OCR still run locally; use the GPU when there is one."""
    import torch
    from docling.datamodel.accelerator_options import AcceleratorDevice

    if torch.cuda.is_available():
        return AcceleratorDevice.CUDA, True
    return AcceleratorDevice.CPU, False


def _reset_cuda_stats() -> None:
    """Peak memory is only meaningful when the local stages run on a GPU."""
    import torch

    if torch.cuda.is_available():
        torch.cuda.reset_peak_memory_stats()


def _peak_cuda_bytes() -> int:
    import torch

    return torch.cuda.max_memory_allocated() if torch.cuda.is_available() else 0



def convert(source: Path, output: Path, models: Path | None) -> dict[str, object]:
    import docling
    from docling.backend.pypdfium2_backend import PyPdfiumDocumentBackend
    from docling.datamodel.accelerator_options import AcceleratorOptions
    from docling.datamodel.base_models import InputFormat
    from docling.datamodel.pipeline_options import CodeFormulaVlmOptions, EasyOcrOptions, PdfPipelineOptions, TableFormerMode, TableStructureOptions
    from docling.document_converter import DocumentConverter, PdfFormatOption
    from docling_core.types.doc import ImageRefMode

    if docling.__version__ != _DOCLING_VERSION:
        raise RuntimeError(f"Docling {_DOCLING_VERSION} is required, found {docling.__version__}")
    settings = vlm_settings()
    device, use_gpu = _local_device()
    started = time.perf_counter()
    _reset_cuda_stats()
    formula = CodeFormulaVlmOptions.from_preset("codeformulav2")
    formula.model_spec.revision = _FORMULA_REVISION
    formula.extract_code = False
    formula.engine_options = settings.engine_options(max_tokens=_FORMULA_MAX_TOKENS)
    options = PdfPipelineOptions(
        artifacts_path=models,
        accelerator_options=AcceleratorOptions(device=device, num_threads=4),
        ocr_options=EasyOcrOptions(lang=["ch_sim", "en"], use_gpu=use_gpu, download_enabled=False,
                                   model_storage_directory=str(models / "EasyOcr") if models else None,
                                   confidence_threshold=0.0),
        table_structure_options=TableStructureOptions(mode=TableFormerMode.ACCURATE, do_cell_matching=False),
        code_formula_options=formula,
        do_ocr=True, do_table_structure=True, do_formula_enrichment=True, do_code_enrichment=False,
        do_picture_description=False, do_picture_classification=False, enable_remote_services=True,
        generate_page_images=True, generate_picture_images=True, generate_table_images=True, images_scale=2.0,
        generate_parsed_pages=True,
    )
    converter = DocumentConverter(
        allowed_formats=[InputFormat.PDF],
        format_options={InputFormat.PDF: PdfFormatOption(pipeline_options=options, backend=PyPdfiumDocumentBackend)},
    )
    converter.initialize_pipeline(InputFormat.PDF)
    _tune_formula_stage(converter)
    result = converter.convert(source, raises_on_error=False, max_file_size=100 * 1024 * 1024, max_num_pages=1000)
    if result.status.value != "success" or result.errors:
        raise RuntimeError("; ".join(str(error) for error in result.errors) or f"Docling status: {result.status.value}")
    doc = result.document
    # Embedded images survive staging-directory renames and Windows path limits.
    doc.save_as_json(output / "document.json", image_mode=ImageRefMode.EMBEDDED)
    manifest = _export(doc, output)
    manifest["warnings"] = _ocr_diagnostics(result.pages, output)
    manifest["parser"] = {"version": PDF_PARSER_VERSION, "docling": _DOCLING_VERSION,
                          "formula_model": "docling-project/CodeFormulaV2",
                          "formula_revision": _FORMULA_REVISION, "vlm": settings.describe(),
                          "formula_crop_scale": 3.0, "formula_crop_expansion": 0.0, "formula_batch_size": 1,
                          "ocr_confidence_threshold": 0.0}
    del converter, result, doc
    gc.collect()
    missing = [asset for asset in manifest["assets"] if not asset["caption"]]
    if missing:
        _describe_missing(missing, output, settings)
    manifest["metrics"] = {"seconds": round(time.perf_counter() - started, 3),
                           "peak_cuda_allocated_bytes": _peak_cuda_bytes()}
    return manifest


def _ocr_diagnostics(pages, output: Path) -> list[dict]:
    records, warnings = [], []
    for page in pages:
        cells = [cell for cell in page.cells if cell.from_ocr]
        for cell in cells:
            records.append({"page": page.page_no, "text": cell.text, "confidence": cell.confidence,
                            "bbox": cell.rect.to_bounding_box().model_dump(mode="json")})
        low = sum(cell.confidence < 0.5 for cell in cells)
        if low:
            warnings.append({"page": page.page_no, "kind": "low_confidence_ocr", "count": low,
                             "message": "Low-confidence OCR text was retained, not discarded; verify against the PDF."})
    (output / "ocr.json").write_text(json.dumps(records, ensure_ascii=False, indent=2), encoding="utf-8")
    return warnings


def _tune_formula_stage(converter) -> None:
    """Tight high-resolution crops avoid including adjacent prose in formula input."""
    from docling.models.stages.code_formula.code_formula_vlm_model import CodeFormulaVlmModel

    stages = [stage for pipeline in converter.initialized_pipelines.values()
              for stage in pipeline.enrichment_pipe if isinstance(stage, CodeFormulaVlmModel)]
    if len(stages) != 1:
        raise RuntimeError("Expected one CodeFormulaV2 stage")
    stages[0].images_scale = 3.0
    stages[0].expansion_factor = 0.0
    stages[0].elements_batch_size = 1
    stages[0].engine = _CheckedFormulaEngine(stages[0].engine)


class _CheckedFormulaEngine:
    """Keep failed or truncated model output out of a successful document."""

    def __init__(self, engine):
        self.engine = engine

    def __getattr__(self, name):
        return getattr(self.engine, name)

    def predict_batch(self, inputs):
        outputs = []
        for item in inputs:
            output = self.engine.predict_batch([item])[0]
            if not output.text.strip():
                raise RuntimeError("The remote VLM returned no text for a formula; check "
                                   "RAG_VLM_BASE_URL, RAG_VLM_MODEL and the endpoint logs")
            if (output.metadata.get("num_tokens", 0) >= item.max_new_tokens
                    or "</formula>" not in output.text):
                raise RuntimeError("Formula generation did not finish; document was not indexed")
            outputs.append(output)
        return outputs


def _export(doc, output: Path) -> dict[str, object]:
    from docling_core.types.doc import DocItemLabel, PictureItem, TableItem

    owners: dict[str, list[str]] = {}
    for item in [*doc.pictures, *doc.tables]:
        for ref in [*item.captions, *item.footnotes]:
            owners.setdefault(ref.cref, []).append(item.self_ref)
    blocks, assets = [], []
    seen = set()
    picture_dir = output / "pictures"
    picture_dir.mkdir(exist_ok=True)
    for item, _ in doc.iterate_items():
        if item.self_ref in seen or item.label in {DocItemLabel.PAGE_HEADER, DocItemLabel.PAGE_FOOTER}:
            continue
        seen.add(item.self_ref)
        page, bbox = _location(item)
        base = {"ref": item.self_ref, "kind": item.label.value, "page": page,
                "bbox": bbox, "locations": _locations(item), "order": len(blocks), "text": ""}
        if isinstance(item, (PictureItem, TableItem)):
            captions = _annotations(item.captions, doc)
            footnotes = _annotations(item.footnotes, doc)
            caption = "\n".join(part["text"] for part in captions)
            origin = "document" if caption else "none"
            base["asset_id"] = item.self_ref
        if isinstance(item, PictureItem):
            image_name = f"picture-{len([a for a in assets if a['kind'] == 'picture'])}.png"
            image = item.get_image(doc)
            if image is None:
                raise RuntimeError(f"Picture crop is empty: {item.self_ref}")
            image.save(picture_dir / image_name, format="PNG")
            assets.append({"asset_id": item.self_ref, "kind": "picture", "page": page, "bbox": bbox,
                           "caption": caption, "caption_source": origin, "generated_description": "",
                           "captions": captions, "footnotes": footnotes,
                           "image_file": f"pictures/{image_name}", "table": None,
                           "_vision_file": f"pictures/{image_name}"})
        elif isinstance(item, TableItem):
            table = _table_data(item, doc)
            _validate_table(table, item.self_ref)
            image_name = f"table-{len([a for a in assets if a['kind'] == 'table'])}.png"
            image = item.get_image(doc)
            if image is not None:
                image.save(output / image_name, format="PNG")
            assets.append({"asset_id": item.self_ref, "kind": "table", "page": page, "bbox": bbox,
                           "caption": caption, "caption_source": origin, "generated_description": "",
                           "captions": captions, "footnotes": footnotes,
                           "image_file": None, "table": table, "_vision_file": image_name})
        else:
            text = str(getattr(item, "text", "") or "").strip()
            if item.label == DocItemLabel.FORMULA:
                if not text:
                    raise RuntimeError(f"Formula extraction returned empty LaTeX: page {page}, {item.self_ref}")
                base.update(latex=text, original_text=str(getattr(item, "orig", "") or ""))
                number = re.search(r"\((\d+(?:[A-Za-z]|\.\d+)?)\)\s*[,.;]?\s*$", base["original_text"])
                base["equation_number"] = number.group(1) if number else None
                text = "$$\n" + text + "\n$$"
                if number and not re.search(r"\(\s*" + re.escape(number.group(1)) + r"\s*\)", base["latex"]):
                    text += "\n\n(" + number.group(1) + ")"
            if not text:
                continue
            base["text"] = text
            if item.label in {DocItemLabel.CAPTION, DocItemLabel.FOOTNOTE}:
                base["associated_assets"] = owners.get(item.self_ref, [])
                base["association"] = "document" if item.self_ref in owners else "unlinked"
        blocks.append(base)
    for asset in assets:
        if asset["kind"] == "table" and not asset["table"]["rows"]:
            raise RuntimeError(f"Table structure is empty: {asset['asset_id']}")
    return {"version": PDF_PARSER_VERSION, "blocks": blocks, "assets": assets}


def _location(item) -> tuple[int, dict[str, object]]:
    if not item.prov:
        raise RuntimeError(f"PDF item has no page location: {item.self_ref}")
    prov = item.prov[0]
    return int(prov.page_no), prov.bbox.model_dump(mode="json")


def _locations(item) -> list[dict]:
    return [{"page": int(prov.page_no), "bbox": prov.bbox.model_dump(mode="json"),
             "charspan": list(prov.charspan)} for prov in item.prov]


def _annotations(refs, doc) -> list[dict[str, object]]:
    result = []
    seen = set()
    for ref in refs:
        if ref.cref in seen:
            continue
        seen.add(ref.cref)
        item = ref.resolve(doc)
        page, bbox = _location(item)
        result.append({"ref": ref.cref, "text": str(getattr(item, "text", "") or ""),
                       "page": page, "bbox": bbox, "locations": _locations(item)})
    return result


def _table_data(item, doc) -> dict[str, object]:
    data = item.data
    rows = [["" for _ in range(data.num_cols)] for _ in range(data.num_rows)]
    cells = []
    header_indices = set()
    for cell in data.table_cells:
        raw = cell.model_dump(mode="json")
        text = str(cell.text or "").strip()
        row_start, row_end = cell.start_row_offset_idx, cell.end_row_offset_idx
        col_start, col_end = cell.start_col_offset_idx, cell.end_col_offset_idx
        for row in range(max(0, row_start), min(data.num_rows, row_end)):
            for col in range(max(0, col_start), min(data.num_cols, col_end)):
                rows[row][col] = text
        if cell.column_header:
            header_indices.update(range(max(0, row_start), min(data.num_rows, row_end)))
        cells.append({key: raw.get(key) for key in (
            "text", "row_span", "col_span", "start_row_offset_idx", "end_row_offset_idx",
            "start_col_offset_idx", "end_col_offset_idx", "column_header", "row_header", "row_section"
        )})
    header_rows = [list(rows[index]) for index in sorted(header_indices)]
    headers = ["\n".join(dict.fromkeys(row[col] for row in header_rows if row[col]))
               for col in range(data.num_cols)]
    footnotes = [part["text"] for part in _annotations(item.footnotes, doc)]
    return {"num_rows": data.num_rows, "num_cols": data.num_cols, "orientation": data.orientation.value,
            "headers": headers, "header_rows": header_rows,
            "rows": rows, "cells": cells, "footnotes": footnotes}


def _validate_table(table: dict[str, object], reference: str) -> None:
    rows = table["rows"]
    headers = table["headers"]
    num_cols = int(table["num_cols"])
    if not rows or num_cols < 1:
        raise RuntimeError(f"Table structure is empty: {reference}")
    if num_cols >= 8:
        header_count = sum(bool(str(value).strip()) for value in headers)
        leading = rows[: min(3, len(rows))]
        occupied = sum(bool(str(value).strip()) for row in leading for value in row)
        capacity = max(1, len(leading) * num_cols)
        if header_count < max(2, num_cols // 3) and occupied / capacity < 0.4:
            raise RuntimeError(f"Wide table structure is unreliable after rotation/OCR: {reference}")


def _describe_missing(assets: list[dict[str, object]], output: Path, settings: VlmSettings, *,
                      max_image_edge: int | None = None) -> None:
    """Describe uncaptioned figures and tables through the remote VLM endpoint."""
    import base64
    from io import BytesIO

    import requests
    from PIL import Image

    session = requests.Session()
    session.headers.update(settings.headers())
    try:
        for asset in assets:
            image_path = output / str(asset.pop("_vision_file"))
            if not image_path.is_file():
                raise RuntimeError(f"Vision input is missing: {image_path.name}")
            with Image.open(image_path) as image:
                rgb = image.convert("RGB")
                if max_image_edge is not None:
                    rgb.thumbnail((max_image_edge, max_image_edge))
                buffer = BytesIO()
                rgb.save(buffer, format="PNG")
            description = _describe_image(session, settings, base64.b64encode(buffer.getvalue()).decode("ascii"))
            asset["generated_description"] = _clean_description(description)
            asset["description_source"] = "remote_vlm"
    finally:
        session.close()
    for asset in assets:
        asset.pop("_vision_file", None)


def _describe_image(session, settings: VlmSettings, image_base64: str) -> str:
    """One OpenAI-compatible chat completion with the image plus a fixed prompt."""
    payload = {
        "model": settings.model,
        "temperature": 0.0,
        "max_tokens": _DESCRIPTION_MAX_TOKENS,
        "messages": [{"role": "user", "content": [
            {"type": "image_url", "image_url": {"url": "data:image/png;base64," + image_base64}},
            {"type": "text", "text": _DESCRIPTION_PROMPT},
        ]}],
    }
    headers = {"Content-Type": "application/json"}
    headers.update(settings.headers())
    last_error = ""
    for attempt in range(3):
        if attempt:
            time.sleep(0.5 * attempt)
        try:
            response = session.post(settings.chat_url, headers=headers, json=payload, timeout=settings.timeout)
        except Exception as error:  # network errors must fail the document, not hang
            last_error = f"request failed: {error}"
            continue
        if response.status_code in {429, 500, 502, 503, 504}:
            last_error = f"HTTP {response.status_code}: {response.text[:200]}"
            continue
        if response.status_code != 200:
            raise RuntimeError(f"Remote VLM returned HTTP {response.status_code}: {response.text[:200]}")
        try:
            data = response.json()
        except ValueError as error:
            raise RuntimeError("Remote VLM returned invalid JSON") from error
        try:
            content = data["choices"][0]["message"]["content"]
        except (KeyError, IndexError, TypeError) as error:
            raise RuntimeError("Remote VLM response has no choices[0].message.content") from error
        if isinstance(content, list):
            content = "".join(str(part.get("text", "")) for part in content if isinstance(part, dict))
        text = str(content or "").strip()
        if not text:
            raise RuntimeError("Remote VLM returned an empty description")
        return text
    raise RuntimeError(f"Remote VLM call failed after 3 attempts: {last_error}")


def _clean_description(text: str) -> str:
    value = text.replace("<end_of_utterance>", "").replace("<end_of_utteranc", "").strip()
    if not value:
        return value
    if value[-1] not in ".!?。！？":
        value += "."
    return value


if __name__ == "__main__":
    main()
