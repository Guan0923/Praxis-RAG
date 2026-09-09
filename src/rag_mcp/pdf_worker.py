"""Isolated Docling worker for PDF text, pictures, and tables."""

from __future__ import annotations

import argparse
import gc
import json
import re
import time
from pathlib import Path

from .pdf_content import PDF_PARSER_VERSION

_DOCLING_VERSION = "2.126.0"
_GRANITE_REVISION = "bf66a8244401a8e26d2ff429aa82df8550a289e5"
_FORMULA_REVISION = "ecedbe111d15c2dc60bfd4a823cbe80127b58af4"


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


def convert(source: Path, output: Path, models: Path | None) -> dict[str, object]:
    import docling
    import torch
    from docling.backend.pypdfium2_backend import PyPdfiumDocumentBackend
    from docling.datamodel.accelerator_options import AcceleratorDevice, AcceleratorOptions
    from docling.datamodel.base_models import InputFormat
    from docling.datamodel.pipeline_options import CodeFormulaVlmOptions, EasyOcrOptions, PdfPipelineOptions, TableFormerMode, TableStructureOptions
    from docling.datamodel.vlm_engine_options import TransformersVlmEngineOptions
    from docling.document_converter import DocumentConverter, PdfFormatOption
    from docling_core.types.doc import ImageRefMode

    if docling.__version__ != _DOCLING_VERSION:
        raise RuntimeError(f"Docling {_DOCLING_VERSION} is required, found {docling.__version__}")
    if not torch.cuda.is_available():
        raise RuntimeError("Granite Vision and Docling PDF parsing require a CUDA GPU")
    started = time.perf_counter()
    torch.cuda.reset_peak_memory_stats()
    formula = CodeFormulaVlmOptions.from_preset("codeformulav2")
    formula.model_spec.revision = _FORMULA_REVISION
    formula.extract_code = False
    formula.engine_options = TransformersVlmEngineOptions(
        device=AcceleratorDevice.CUDA, compile_model=False, use_kv_cache=True,
    )
    options = PdfPipelineOptions(
        artifacts_path=models,
        accelerator_options=AcceleratorOptions(device=AcceleratorDevice.CUDA, num_threads=4),
        ocr_options=EasyOcrOptions(lang=["ch_sim", "en"], use_gpu=True, download_enabled=False,
                                   model_storage_directory=str(models / "EasyOcr") if models else None,
                                   confidence_threshold=0.0),
        table_structure_options=TableStructureOptions(mode=TableFormerMode.ACCURATE, do_cell_matching=False),
        code_formula_options=formula,
        do_ocr=True, do_table_structure=True, do_formula_enrichment=True, do_code_enrichment=False,
        do_picture_description=False, do_picture_classification=False, enable_remote_services=False,
        generate_page_images=True, generate_picture_images=True, generate_table_images=True, images_scale=2.0,
        generate_parsed_pages=True,
    )
    converter = DocumentConverter(
        allowed_formats=[InputFormat.PDF],
        format_options={InputFormat.PDF: PdfFormatOption(pipeline_options=options, backend=PyPdfiumDocumentBackend)},
    )
    converter.initialize_pipeline(InputFormat.PDF)
    cache = _enable_formula_cache(converter)
    result = converter.convert(source, raises_on_error=False, max_file_size=10 * 1024 * 1024, max_num_pages=1000)
    if result.status.value != "success" or result.errors:
        raise RuntimeError("; ".join(str(error) for error in result.errors) or f"Docling status: {result.status.value}")
    doc = result.document
    # Embedded images survive staging-directory renames and Windows path limits.
    doc.save_as_json(output / "document.json", image_mode=ImageRefMode.EMBEDDED)
    manifest = _export(doc, output)
    manifest["warnings"] = _ocr_diagnostics(result.pages, output)
    manifest["parser"] = {"version": PDF_PARSER_VERSION, "docling": _DOCLING_VERSION,
                          "formula_model": "docling-project/CodeFormulaV2",
                          "formula_revision": _FORMULA_REVISION, "formula_cache": cache,
                          "formula_crop_scale": 3.0, "formula_crop_expansion": 0.0, "formula_batch_size": 1,
                          "ocr_confidence_threshold": 0.0}
    del converter, result, doc
    gc.collect()
    torch.cuda.empty_cache()
    missing = [asset for asset in manifest["assets"] if not asset["caption"]]
    if missing:
        if models is None:
            raise RuntimeError("RAG_PDF_MODELS_PATH is required when a figure or table has no caption")
        _describe_missing(missing, output, models)
    manifest["metrics"] = {"seconds": round(time.perf_counter() - started, 3),
                           "peak_cuda_allocated_bytes": torch.cuda.max_memory_allocated()}
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


def _enable_formula_cache(converter) -> bool:
    from docling.models.stages.code_formula.code_formula_vlm_model import CodeFormulaVlmModel
    from docling.models.utils.generation_utils import build_generation_config

    stages = [stage for pipeline in converter.initialized_pipelines.values()
              for stage in pipeline.enrichment_pipe if isinstance(stage, CodeFormulaVlmModel)]
    if len(stages) != 1:
        raise RuntimeError("Expected one CodeFormulaV2 stage")
    engine = stages[0].engine
    engine.vlm_model.generation_config.use_cache = True
    engine.generation_config.use_cache = True
    requested = build_generation_config(engine.generation_config, use_cache=True)
    effective, _ = engine.vlm_model._prepare_generation_config(requested)
    if effective.use_cache is not True:
        raise RuntimeError("Formula generation cache was overridden")
    # Tight high-resolution crops avoid including adjacent prose in formula input.
    stages[0].images_scale = 3.0
    stages[0].expansion_factor = 0.0
    stages[0].elements_batch_size = 1
    stages[0].engine = _CheckedFormulaEngine(engine)
    return True


class _CheckedFormulaEngine:
    """Keep truncated model output out of a successful document."""

    def __init__(self, engine):
        self.engine = engine

    def __getattr__(self, name):
        return getattr(self.engine, name)

    def predict_batch(self, inputs):
        outputs = []
        for item in inputs:
            output = self.engine.predict_batch([item])[0]
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


def _describe_missing(assets: list[dict[str, object]], output: Path, models: Path, *,
                      compile_model: bool = True, max_image_edge: int | None = None) -> None:
    import torch
    from PIL import Image
    from docling.datamodel.accelerator_options import AcceleratorDevice, AcceleratorOptions
    from docling.datamodel.pipeline_options import PictureDescriptionVlmOptions
    from docling.models.stages.picture_description.picture_description_vlm_model import PictureDescriptionVlmModel

    model_root = models / "ibm-granite--granite-vision-3.3-2b"
    metadata = model_root / ".cache" / "huggingface" / "download" / "config.json.metadata"
    if not metadata.is_file() or metadata.read_text(encoding="utf-8").splitlines()[0].strip() != _GRANITE_REVISION:
        raise RuntimeError(f"Granite Vision revision {_GRANITE_REVISION} is required")
    options = PictureDescriptionVlmOptions(
        repo_id="ibm-granite/granite-vision-3.3-2b",
        prompt=("Describe the visible content for document retrieval in 2 to 4 concise sentences. "
                "Preserve visible names, labels and relationships. For a table, summarize its subject only; "
                "never invent cell values. End with a complete sentence."),
        generation_config={"max_new_tokens": 256, "do_sample": False},
    )
    model = PictureDescriptionVlmModel(
        enabled=True, enable_remote_services=False, artifacts_path=models, options=options,
        accelerator_options=AcceleratorOptions(device=AcceleratorDevice.CUDA, num_threads=4),
    )
    if not compile_model:
        model.model = getattr(model.model, "_orig_mod", model.model)
        model.model.eval()
    for asset in assets:
        image_path = output / str(asset.pop("_vision_file"))
        if not image_path.is_file():
            raise RuntimeError(f"Vision input is missing: {image_path.name}")
        with Image.open(image_path) as image:
            rgb = image.convert("RGB")
            if max_image_edge is not None:
                rgb.thumbnail((max_image_edge, max_image_edge))
            descriptions = list(model._annotate_images([rgb]))
        if len(descriptions) != 1 or not descriptions[0].strip():
            raise RuntimeError(f"Granite Vision returned no description for {asset['asset_id']}")
        asset["generated_description"] = _clean_description(descriptions[0])
        asset["description_source"] = "granite_vision"
    for asset in assets:
        asset.pop("_vision_file", None)
    del model
    gc.collect()
    torch.cuda.empty_cache()


def _clean_description(text: str) -> str:
    value = text.replace("<end_of_utterance>", "").replace("<end_of_utteranc", "").strip()
    if not value:
        return value
    if value[-1] not in ".!?。！？":
        value += "."
    return value


if __name__ == "__main__":
    main()
