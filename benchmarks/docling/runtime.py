"""Benchmark-only bridge from Docling to the existing structural chunker."""

from __future__ import annotations

import json
import os
import time
from pathlib import Path


def offline() -> None:
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["TRANSFORMERS_OFFLINE"] = "1"
    os.environ["TOKENIZERS_PARALLELISM"] = "false"


def model_paths(output: Path) -> dict:
    return json.loads((output / "environment" / "model-revisions.json").read_text(encoding="utf-8"))


def formula_engine(instance):
    from docling.models.stages.code_formula.code_formula_vlm_model import CodeFormulaVlmModel
    from docling.models.inference_engines.vlm.transformers_engine import TransformersVlmEngine

    stages = [stage for pipeline in instance.initialized_pipelines.values()
              for stage in pipeline.enrichment_pipe if isinstance(stage, CodeFormulaVlmModel)]
    if len(stages) != 1 or not isinstance(stages[0].engine, TransformersVlmEngine):
        raise RuntimeError("Expected one initialized Transformers formula engine")
    return stages[0].engine


def enable_formula_cache(instance) -> dict:
    from docling.models.utils.generation_utils import build_generation_config

    engine = formula_engine(instance)
    model = engine.vlm_model
    previous = model.generation_config.use_cache
    # Transformers 4.57 can replace a global-default True with the model's False.
    # Align both in-memory defaults; never change weights or shared library files.
    model.generation_config.use_cache = True
    engine.generation_config.use_cache = True
    requested = build_generation_config(engine.generation_config, use_cache=True)
    effective, _ = model._prepare_generation_config(requested)
    if effective.use_cache is not True:
        raise RuntimeError("Formula generation cache was overridden")
    return {"original_model_use_cache": previous, "effective_use_cache": effective.use_cache}


def converter(output: Path):
    import torch
    from docling.datamodel.accelerator_options import AcceleratorDevice, AcceleratorOptions
    from docling.datamodel.base_models import InputFormat
    from docling.datamodel.pipeline_options import (
        CodeFormulaVlmOptions, EasyOcrOptions, LayoutObjectDetectionOptions,
        PdfPipelineOptions, TableFormerMode, TableStructureOptions,
    )
    from docling.datamodel.vlm_engine_options import TransformersVlmEngineOptions
    from docling.document_converter import DocumentConverter, PdfFormatOption

    if not torch.cuda.is_available():
        raise RuntimeError("CUDA is required; CPU fallback is prohibited")
    torch.set_num_threads(4)
    revisions = model_paths(output)
    layout = LayoutObjectDetectionOptions.from_preset("layout_heron_default")
    layout.model_spec.revision = revisions["docling-project/docling-layout-heron"]["revision"]
    formula = CodeFormulaVlmOptions.from_preset("codeformulav2")
    formula.model_spec.revision = revisions["docling-project/CodeFormulaV2"]["revision"]
    formula.extract_code = False
    formula.engine_options = TransformersVlmEngineOptions(device=AcceleratorDevice.CUDA, compile_model=False, use_kv_cache=True)
    options = PdfPipelineOptions(
        artifacts_path=output / "models",
        accelerator_options=AcceleratorOptions(device=AcceleratorDevice.CUDA, num_threads=4),
        layout_options=layout,
        ocr_options=EasyOcrOptions(
            lang=["ch_sim", "en"], use_gpu=True, download_enabled=False,
            model_storage_directory=str(output / "models" / "EasyOcr"),
        ),
        table_structure_options=TableStructureOptions(mode=TableFormerMode.ACCURATE, do_cell_matching=True),
        code_formula_options=formula,
        do_ocr=True, do_table_structure=True, do_formula_enrichment=True,
        do_code_enrichment=False, do_picture_description=False, do_picture_classification=False,
        enable_remote_services=False, generate_page_images=True, generate_picture_images=True,
        images_scale=1.5,
    )
    instance = DocumentConverter(
        allowed_formats=[InputFormat.PDF, InputFormat.DOCX],
        format_options={InputFormat.PDF: PdfFormatOption(pipeline_options=options)},
    )
    started = time.perf_counter()
    instance.initialize_pipeline(InputFormat.PDF)
    cache = enable_formula_cache(instance)
    torch.cuda.synchronize()
    initialized = time.perf_counter() - started
    recorded = options.model_dump(mode="json", serialize_as_any=True)
    recorded["formula_cache"] = cache
    return instance, initialized, recorded


def convert_document(instance, source: Path, destination: Path) -> tuple[object, dict]:
    import torch
    from docling_core.types.doc import ImageRefMode

    destination.mkdir(parents=True, exist_ok=True)
    started = time.perf_counter()
    result = instance.convert(source, raises_on_error=False, max_file_size=10 * 1024 * 1024, max_num_pages=1000)
    torch.cuda.synchronize()
    conversion_seconds = time.perf_counter() - started
    doc = result.document
    started = time.perf_counter()
    doc.save_as_json(destination / "document.json", image_mode=ImageRefMode.REFERENCED)
    doc.save_as_markdown(destination / "document.md", image_mode=ImageRefMode.REFERENCED)
    doc.save_as_html(destination / "document.html", image_mode=ImageRefMode.REFERENCED)
    elements = []
    for item, _ in doc.iterate_items():
        elements.append({
            "ref": item.self_ref, "label": str(item.label.value), "text": getattr(item, "text", ""),
            "provenance": [p.model_dump(mode="json") for p in item.prov],
        })
    details = {
        "source": source.name, "status": result.status.value,
        "conversion_seconds": conversion_seconds, "export_seconds": time.perf_counter() - started,
        "pages": len(doc.pages), "tables": len(doc.tables), "pictures": len(doc.pictures),
        "elements": elements, "errors": [str(e) for e in result.errors],
    }
    (destination / "conversion.json").write_text(json.dumps(details, ensure_ascii=False, indent=2), encoding="utf-8")
    return doc, details


def chunks_from_document(doc, sample_id: str, source: str, output: Path):
    from docling_core.types.doc import DocItemLabel, PictureItem, TableItem
    from transformers import AutoTokenizer
    from rag_mcp.chunk_strategies import ChunkingConfig, TokenBudget, source_metadata, split_content
    from rag_mcp.chunking import DocumentChunk
    from rag_mcp.document_readers import DocumentContent, SourceBlock, SourceSpan

    parts, spans, references = [], [], []
    headings = []
    position = 0
    for item, _ in doc.iterate_items():
        if item.label in {DocItemLabel.PAGE_HEADER, DocItemLabel.PAGE_FOOTER}:
            continue
        if isinstance(item, TableItem):
            text = item.export_to_markdown(doc=doc)
            kind = "table"
        elif isinstance(item, PictureItem):
            text = item.caption_text(doc=doc)
            kind = "picture"
        else:
            text = getattr(item, "text", "")
            kind = "paragraph"
        if not text.strip():
            continue
        if item.label in {DocItemLabel.TITLE, DocItemLabel.SECTION_HEADER}:
            level = max(1, getattr(item, "level", 1))
            headings = headings[:level - 1] + [text]
            kind = "heading"
        if parts:
            position += 2
        page = item.prov[0].page_no if item.prov and source.endswith(".pdf") else None
        block = SourceBlock(text, kind, page, tuple(headings), len(references) if kind == "table" else None)
        spans.append(SourceSpan(position, position + len(text), block, len(spans) + 1))
        references.append({"ref": item.self_ref, "provenance": [p.model_dump(mode="json") for p in item.prov]})
        parts.append(text)
        position += len(text)
    text = "\n\n".join(parts)
    content = DocumentContent(text, spans, text)
    tokenizer = AutoTokenizer.from_pretrained(model_paths(output)["BAAI/bge-m3"]["path"], local_files_only=True, use_fast=True)
    config = ChunkingConfig("structure", 512, 64)
    budget = TokenBudget(tokenizer, config)
    pieces = split_content(content, Path(source).suffix, config, budget)
    result = []
    for index, (start, end) in enumerate(pieces):
        piece = text[start:end].strip()
        if not piece:
            continue
        locations = [references[i] for i, span in enumerate(spans) if span.start < end and span.end > start]
        result.append(DocumentChunk(
            sample_id, f"{sample_id}-{index}", piece, doc.name, source,
            {"chunk_index": index, "chunking": {"strategy": "structure", "chunk_size": 512, "chunk_overlap": 64},
             "source_items": locations, **source_metadata(content, start, end)},
        ))
    return result
