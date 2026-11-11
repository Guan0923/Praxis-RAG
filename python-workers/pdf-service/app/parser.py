"""Docling-based PDF conversion used by the Praxis-RAG PDF microservice.

This module is the service-side port of the Docling worker that was removed in
commit ``f778504`` (``git show f778504^:python-workers/rag_mcp/pdf_worker.py``).
The CLI wrapper is gone: :class:`DoclingConverter` receives a PDF path plus
output directories and returns the manifest dictionary that the Go backend
unmarshals into ``document.PDFManifest`` (``internal/document/pdf.go``).

Design rules:

* Docling, docling-core and torch are imported lazily inside the conversion
  functions. Importing :mod:`app.parser` / :mod:`app.main` must work on a host
  where only FastAPI, Pillow and pytest are installed (tests and ``fake`` mode).
* The manifest matches the Go JSON contract exactly: version
  ``docling-service-v1``, block/asset/warning keys are always present, picture
  assets carry a ``pictures/picture-N.png`` relative path and table assets keep
  ``image_file`` null. :func:`validate_manifest` enforces that contract before a
  document is returned to the caller.
* Formula blocks are rendered as ``$$ ... $$`` plus the equation number when one
  can be parsed, and both ``text`` and ``latex`` are required to be non-empty:
  an empty formula fails the document instead of silently indexing nothing.
"""

from __future__ import annotations

import gc
import json
import os
import re
import time
from pathlib import Path
from typing import Any, Protocol
from urllib.parse import urlsplit

from . import SERVICE_NAME, __version__

#: Manifest contract version consumed by the Go backend. This is the service
#: version, not the removed worker's ``docling-formula-remote-v3``.
SERVICE_PARSER_VERSION = "docling-service-v1"

#: Exact Docling release the pipeline options were validated against.
DOCLING_VERSION = "2.126.0"

#: Pinned CodeFormulaV2 revision used by the former worker.
FORMULA_REVISION = "ecedbe111d15c2dc60bfd4a823cbe80127b58af4"
FORMULA_MAX_TOKENS = 1024
DESCRIPTION_MAX_TOKENS = 256
DESCRIPTION_TIMEOUT_FLOOR = 30.0
DESCRIPTION_PROMPT = (
    "Describe the visible content for document retrieval in 2 to 4 concise sentences. "
    "Preserve visible names, labels and relationships. For a table, summarize its subject only; "
    "never invent cell values. End with a complete sentence."
)

#: Bounds that mirror the old worker/converter call.
MAX_FILE_BYTES = 100 * 1024 * 1024
MAX_PAGES = 1000

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


class ParserError(RuntimeError):
    """Base class for parse failures with a message safe to return to callers.

    Messages built by this module never contain document text; they only carry
    references, page numbers and configuration names.
    """


class ParserConfigError(ParserError):
    """The deployment is misconfigured (missing models, VLM or dependency)."""


class ParserConversionError(ParserError):
    """The document could not be converted into a valid manifest."""


class VlmSettings:
    """Remote OpenAI-compatible vision-language endpoint.

    Ported from the old worker: formulas use it as the Docling CodeFormulaV2
    inference engine (``ApiVlmEngineOptions``) and uncaptioned pictures/tables
    use it for descriptions.
    """

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
        """Build the Docling API engine options (imports docling lazily)."""
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
        """Record the model and host, never the API key."""
        return {"backend": "remote", "model": self.model, "endpoint": urlsplit(self.base_url).netloc}


def vlm_settings_from_env() -> VlmSettings | None:
    """Read ``RAG_VLM_*``; return ``None`` when no remote endpoint is configured.

    Both ``RAG_VLM_BASE_URL`` and ``RAG_VLM_MODEL`` must be set together. A
    partial configuration is an error instead of a silent fallback, because the
    service cannot know whether the operator wanted local or remote inference.
    """
    base_url = os.environ.get("RAG_VLM_BASE_URL", "").strip()
    model = os.environ.get("RAG_VLM_MODEL", "").strip()
    if not base_url and not model:
        return None
    if not base_url:
        raise ParserConfigError(
            "RAG_VLM_BASE_URL is not configured: set both RAG_VLM_BASE_URL and RAG_VLM_MODEL "
            "for remote formula recognition, or unset both to use local CodeFormulaV2 weights"
        )
    if not model:
        raise ParserConfigError(
            "RAG_VLM_MODEL is not configured: set both RAG_VLM_BASE_URL and RAG_VLM_MODEL "
            "for remote formula recognition, or unset both to use local CodeFormulaV2 weights"
        )
    timeout_raw = os.environ.get("RAG_VLM_TIMEOUT", "").strip()
    concurrency_raw = os.environ.get("RAG_VLM_CONCURRENCY", "").strip()
    try:
        timeout = float(timeout_raw or 120.0)
    except ValueError as error:
        raise ParserConfigError("RAG_VLM_TIMEOUT must be a number of seconds") from error
    try:
        concurrency = int(concurrency_raw or 4)
    except ValueError as error:
        raise ParserConfigError("RAG_VLM_CONCURRENCY must be an integer") from error
    return VlmSettings(
        base_url,
        os.environ.get("RAG_VLM_API_KEY", "").strip(),
        model,
        max(timeout, DESCRIPTION_TIMEOUT_FLOOR),
        max(concurrency, 1),
    )


class PdfConverter(Protocol):
    """Converter interface used by the HTTP layer (real Docling or fake)."""

    name: str

    def convert(self, source: Path, output_dir: Path, scratch_dir: Path) -> dict[str, Any]:
        """Write ``manifest.json``-ready artifacts into ``output_dir``.

        ``scratch_dir`` is for intermediate files that must not be published
        (table crops used for descriptions, OCR diagnostics).
        """


# ---------------------------------------------------------------------------
# Formula helpers (pure functions, unit-tested without docling)
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
# Remote VLM descriptions (ported from the old worker)
# ---------------------------------------------------------------------------


def describe_missing_assets(
    assets: list[dict[str, Any]],
    vlm: VlmSettings,
    *,
    max_image_edge: int | None = None,
) -> None:
    """Describe uncaptioned figures and tables through the remote VLM.

    Assets must carry the internal ``_vision_path`` key set by the exporter; it
    is removed for every asset, even when the call fails.
    """
    import base64
    from io import BytesIO

    import requests
    from PIL import Image

    session = requests.Session()
    session.headers.update(vlm.headers())
    try:
        for asset in assets:
            raw_path = asset.get("_vision_path")
            if not raw_path:
                continue
            image_path = Path(str(raw_path))
            if not image_path.is_file():
                raise ParserConversionError(f"Vision input is missing: {image_path.name}")
            with Image.open(image_path) as image:
                rgb = image.convert("RGB")
                if max_image_edge is not None:
                    rgb.thumbnail((max_image_edge, max_image_edge))
                buffer = BytesIO()
                rgb.save(buffer, format="PNG")
            description = _describe_image(
                session, vlm, base64.b64encode(buffer.getvalue()).decode("ascii")
            )
            asset["generated_description"] = clean_description(description)
    finally:
        session.close()
        for asset in assets:
            asset.pop("_vision_path", None)


def _describe_image(session: Any, vlm: VlmSettings, image_base64: str) -> str:
    """One OpenAI-compatible chat completion with the image plus a fixed prompt."""
    payload = {
        "model": vlm.model,
        "temperature": 0.0,
        "max_tokens": DESCRIPTION_MAX_TOKENS,
        "messages": [
            {
                "role": "user",
                "content": [
                    {"type": "image_url", "image_url": {"url": "data:image/png;base64," + image_base64}},
                    {"type": "text", "text": DESCRIPTION_PROMPT},
                ],
            }
        ],
    }
    headers = {"Content-Type": "application/json"}
    headers.update(vlm.headers())
    last_error = ""
    for attempt in range(3):
        if attempt:
            time.sleep(0.5 * attempt)
        try:
            response = session.post(vlm.chat_url, headers=headers, json=payload, timeout=vlm.timeout)
        except Exception as error:  # network errors must fail the document, not hang
            last_error = f"request failed: {error}"
            continue
        if response.status_code in {429, 500, 502, 503, 504}:
            last_error = f"HTTP {response.status_code}: {response.text[:200]}"
            continue
        if response.status_code != 200:
            raise ParserConversionError(
                f"Remote VLM returned HTTP {response.status_code}: {response.text[:200]}"
            )
        try:
            data = response.json()
        except ValueError as error:
            raise ParserConversionError("Remote VLM returned invalid JSON") from error
        try:
            content = data["choices"][0]["message"]["content"]
        except (KeyError, IndexError, TypeError) as error:
            raise ParserConversionError("Remote VLM response has no choices[0].message.content") from error
        if isinstance(content, list):
            content = "".join(str(part.get("text", "")) for part in content if isinstance(part, dict))
        text = str(content or "").strip()
        if not text:
            raise ParserConversionError("Remote VLM returned an empty description")
        return text
    raise ParserConversionError(f"Remote VLM call failed after 3 attempts: {last_error}")


# ---------------------------------------------------------------------------
# Docling converter
# ---------------------------------------------------------------------------


class DoclingConverter:
    """Real Docling conversion using CodeFormulaV2 (local weights or remote VLM)."""

    name = "docling"

    def __init__(self, models_path: Path | None, vlm: VlmSettings | None) -> None:
        self.models_path = models_path
        self.vlm = vlm

    def convert(self, source: Path, output_dir: Path, scratch_dir: Path) -> dict[str, Any]:
        return convert_pdf(
            source, output_dir, scratch_dir, models=self.models_path, vlm=self.vlm
        )


class FakeConverter:
    """Deterministic converter for tests and Go integration without weights.

    Produces one text block, one formula block and one picture asset backed by a
    real 8x8 PNG, so the zip/manifest contract can be exercised end to end on a
    machine without docling or torch.
    """

    name = "fake"

    def __init__(self, *, picture_size: int = 8) -> None:
        self.picture_size = picture_size

    def convert(self, source: Path, output_dir: Path, scratch_dir: Path) -> dict[str, Any]:
        started = time.perf_counter()
        picture_dir = output_dir / "pictures"
        picture_dir.mkdir(parents=True, exist_ok=True)
        write_fake_png(picture_dir / "picture-0.png", self.picture_size)
        manifest = fake_manifest()
        manifest["metrics"] = {
            "seconds": round(time.perf_counter() - started, 3),
            "pages": 1,
            "peak_cuda_allocated_bytes": 0,
        }
        return manifest


def write_fake_png(path: Path, size: int = 8) -> None:
    """Write a small but real PNG (diagonal lines on a white canvas)."""
    try:
        from PIL import Image
    except ImportError as error:  # pragma: no cover - pillow is a hard dependency
        raise ParserConfigError("Pillow is required for the fake converter") from error
    image = Image.new("RGB", (size, size), (255, 255, 255))
    for index in range(size):
        image.putpixel((index, index), (0, 0, 0))
        image.putpixel((size - 1 - index, index), (200, 60, 60))
    image.save(path, format="PNG")


_FAKE_BBOX: dict[str, Any] = {
    "l": 72.0,
    "t": 640.0,
    "r": 540.0,
    "b": 700.0,
    "coord_origin": "BOTTOMLEFT",
}
_FAKE_TEXT = (
    "Praxis-RAG PDF service fake mode. "
    "Formula recognition and picture extraction are simulated for local testing."
)
FAKE_LATEX = "G = X_{basis}^{T} X_{basis}"
FAKE_EQUATION_NUMBER = "1"


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
                "locations": [{"page": 1, "bbox": dict(_FAKE_BBOX), "charspan": [0, len(formula_text)]}],
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
        "parser": {
            "version": SERVICE_PARSER_VERSION,
            "service_version": __version__,
            "converter": "fake",
            "docling": None,
            "formula_model": "fake",
            "formula_revision": None,
            "vlm": None,
            "formula_crop_scale": 3.0,
            "formula_crop_expansion": 0.0,
            "formula_batch_size": 1,
            "ocr_confidence_threshold": 0.0,
        },
        "metrics": {"seconds": 0.0, "pages": 1, "peak_cuda_allocated_bytes": 0},
    }


def convert_pdf(
    source: Path,
    output_dir: Path,
    scratch_dir: Path,
    *,
    models: Path | None = None,
    vlm: VlmSettings | None = None,
) -> dict[str, Any]:
    """Convert one PDF into the Go ``PDFManifest`` contract.

    Docling and torch are imported lazily here, so merely importing the service
    never requires the deep-learning stack.
    """
    modules = _load_docling_modules()
    _validate_local_models(models, vlm)
    device, use_gpu = _local_device()
    started = time.perf_counter()
    _reset_cuda_stats()

    output_dir.mkdir(parents=True, exist_ok=True)
    scratch_dir.mkdir(parents=True, exist_ok=True)

    formula = _formula_options(modules, vlm)
    options = modules.PdfPipelineOptions(
        artifacts_path=str(models) if models else None,
        accelerator_options=modules.AcceleratorOptions(device=device, num_threads=4),
        ocr_options=modules.EasyOcrOptions(
            lang=["ch_sim", "en"],
            use_gpu=use_gpu,
            download_enabled=False,
            model_storage_directory=str(models / "EasyOcr") if models else None,
            confidence_threshold=0.0,
        ),
        table_structure_options=modules.TableStructureOptions(
            mode=modules.TableFormerMode.ACCURATE, do_cell_matching=False
        ),
        code_formula_options=formula,
        do_ocr=True,
        do_table_structure=True,
        do_formula_enrichment=True,
        do_code_enrichment=False,
        do_picture_description=False,
        do_picture_classification=False,
        enable_remote_services=True,
        generate_page_images=True,
        generate_picture_images=True,
        generate_table_images=True,
        images_scale=2.0,
        generate_parsed_pages=True,
    )
    try:
        converter = modules.DocumentConverter(
            allowed_formats=[modules.InputFormat.PDF],
            format_options={
                modules.InputFormat.PDF: modules.PdfFormatOption(
                    pipeline_options=options, backend=modules.PyPdfiumDocumentBackend
                )
            },
        )
        converter.initialize_pipeline(modules.InputFormat.PDF)
        _tune_formula_converter(converter)
        result = converter.convert(
            source,
            raises_on_error=False,
            max_file_size=MAX_FILE_BYTES,
            max_num_pages=MAX_PAGES,
        )
    except ParserError:
        raise
    except Exception as error:  # docling/backend failures must never leak document text
        raise ParserConversionError(f"Docling failed to convert the PDF: {error}") from error

    if result.status.value != "success" or result.errors:
        raise ParserConversionError(
            "; ".join(str(error) for error in result.errors)
            or f"Docling status: {result.status.value}"
        )
    doc = result.document
    pages = list(result.pages)
    manifest = export_manifest(doc, output_dir, scratch_dir, vlm=vlm)
    manifest["warnings"] = _ocr_diagnostics(pages, scratch_dir / "ocr.json")
    manifest["parser"] = _parser_metadata(vlm)
    missing = [asset for asset in manifest["assets"] if not asset["caption"]]
    if missing and vlm is not None:
        describe_missing_assets(missing, vlm)
    manifest["metrics"] = {
        "seconds": round(time.perf_counter() - started, 3),
        "pages": len(pages),
        "peak_cuda_allocated_bytes": _peak_cuda_bytes(),
    }
    del converter, result, doc
    gc.collect()
    return finalize_manifest(manifest)


def _load_docling_modules():
    """Import Docling lazily and check the pinned release."""
    try:
        import docling
        from docling.backend.pypdfium2_backend import PyPdfiumDocumentBackend
        from docling.datamodel.accelerator_options import AcceleratorDevice, AcceleratorOptions
        from docling.datamodel.base_models import InputFormat
        from docling.datamodel.pipeline_options import (
            CodeFormulaVlmOptions,
            EasyOcrOptions,
            PdfPipelineOptions,
            TableFormerMode,
            TableStructureOptions,
        )
        from docling.document_converter import DocumentConverter, PdfFormatOption
    except ImportError as error:
        raise ParserConfigError(
            "Docling is not installed in the PDF service image: "
            f"{error}. Install the service requirements (docling=={DOCLING_VERSION})."
        ) from error
    if docling.__version__ != DOCLING_VERSION:
        raise ParserConfigError(
            f"Docling {DOCLING_VERSION} is required, found {docling.__version__}"
        )
    return _Modules(
        AcceleratorDevice=AcceleratorDevice,
        AcceleratorOptions=AcceleratorOptions,
        CodeFormulaVlmOptions=CodeFormulaVlmOptions,
        DocumentConverter=DocumentConverter,
        EasyOcrOptions=EasyOcrOptions,
        InputFormat=InputFormat,
        PdfFormatOption=PdfFormatOption,
        PdfPipelineOptions=PdfPipelineOptions,
        PyPdfiumDocumentBackend=PyPdfiumDocumentBackend,
        TableFormerMode=TableFormerMode,
        TableStructureOptions=TableStructureOptions,
    )


class _Modules:
    """Tiny namespace for the lazily imported docling symbols."""

    def __init__(self, **symbols: Any) -> None:
        self.__dict__.update(symbols)


def _validate_local_models(models: Path | None, vlm: VlmSettings | None) -> None:
    """Fail with an actionable message when formula recognition has no backend."""
    if models is not None and not models.is_dir():
        raise ParserConfigError(f"RAG_PDF_MODELS_PATH is not a directory: {models}")
    if vlm is not None:
        return
    if models is None:
        raise ParserConfigError(
            "Formula recognition is not configured: set RAG_PDF_MODELS_PATH to local Docling/"
            "CodeFormulaV2 artifacts, or set RAG_VLM_BASE_URL and RAG_VLM_MODEL for a remote "
            "OpenAI-compatible VLM. Refusing to index formulas without a model."
        )
    entries = sorted(entry.name for entry in models.iterdir())
    if not entries:
        raise ParserConfigError(
            f"RAG_PDF_MODELS_PATH is empty: {models}. Run 'docling-tools models download "
            f"--output-dir {models}' before starting the service."
        )
    if not any("codeformula" in name.lower() for name in entries):
        raise ParserConfigError(
            f"RAG_PDF_MODELS_PATH has no CodeFormulaV2 weights: {models}. Expected a "
            "'docling-project--CodeFormulaV2' directory, or configure RAG_VLM_* to use a remote VLM."
        )


def _formula_options(modules: _Modules, vlm: VlmSettings | None):
    formula = modules.CodeFormulaVlmOptions.from_preset("codeformulav2")
    formula.model_spec.revision = FORMULA_REVISION
    formula.extract_code = False
    if vlm is not None:
        formula.engine_options = vlm.engine_options(max_tokens=FORMULA_MAX_TOKENS)
    return formula


def _tune_formula_converter(converter: Any) -> None:
    """Patch the initialized CodeFormulaV2 stage to use tight, high-resolution crops."""
    from docling.models.stages.code_formula.code_formula_vlm_model import CodeFormulaVlmModel

    stages = [
        stage
        for pipeline in converter.initialized_pipelines.values()
        for stage in pipeline.enrichment_pipe
        if isinstance(stage, CodeFormulaVlmModel)
    ]
    if len(stages) != 1:
        raise ParserConversionError("Expected exactly one CodeFormulaV2 stage in the Docling pipeline")
    stages[0].images_scale = 3.0
    stages[0].expansion_factor = 0.0
    stages[0].elements_batch_size = 1
    stages[0].engine = _CheckedFormulaEngine(stages[0].engine)


class _CheckedFormulaEngine:
    """Keep failed or truncated model output out of a successful document."""

    def __init__(self, engine: Any) -> None:
        self.engine = engine

    def __getattr__(self, name: str) -> Any:
        return getattr(self.engine, name)

    def predict_batch(self, inputs: list[Any]) -> list[Any]:
        outputs = []
        for item in inputs:
            output = self.engine.predict_batch([item])[0]
            if not output.text.strip():
                raise ParserConversionError(
                    "The formula model returned no text; check the local CodeFormulaV2 weights "
                    "or RAG_VLM_BASE_URL/RAG_VLM_MODEL and the endpoint logs"
                )
            if (
                output.metadata.get("num_tokens", 0) >= item.max_new_tokens
                or "</formula>" not in output.text
            ):
                raise ParserConversionError(
                    "Formula generation did not finish; the document was not indexed"
                )
            outputs.append(output)
        return outputs


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
    try:
        import torch
    except ImportError:  # pragma: no cover - only reachable without torch
        return 0
    return torch.cuda.max_memory_allocated() if torch.cuda.is_available() else 0


# ---------------------------------------------------------------------------
# Manifest export (ported from the old worker)
# ---------------------------------------------------------------------------


def export_manifest(
    doc: Any,
    output_dir: Path,
    scratch_dir: Path,
    *,
    vlm: VlmSettings | None = None,
) -> dict[str, Any]:
    """Build the ``version``/``blocks``/``assets`` part of the manifest."""
    from docling_core.types.doc import DocItemLabel, PictureItem, TableItem

    owners: dict[str, list[str]] = {}
    for item in [*doc.pictures, *doc.tables]:
        for ref in [*item.captions, *item.footnotes]:
            owners.setdefault(ref.cref, []).append(item.self_ref)

    blocks: list[dict[str, Any]] = []
    assets: list[dict[str, Any]] = []
    seen: set[str] = set()
    picture_dir = output_dir / "pictures"
    picture_dir.mkdir(parents=True, exist_ok=True)
    table_crop_dir = scratch_dir / "tables"
    picture_count = 0
    table_count = 0

    for item, _ in doc.iterate_items():
        if item.self_ref in seen or item.label in {DocItemLabel.PAGE_HEADER, DocItemLabel.PAGE_FOOTER}:
            continue
        seen.add(item.self_ref)
        page, bbox = _location(item)
        block: dict[str, Any] = {
            "ref": item.self_ref,
            "kind": item.label.value,
            "page": page,
            "order": len(blocks),
            "text": "",
            "asset_id": "",
            "bbox": bbox,
            "locations": _locations(item),
            "latex": "",
            "original_text": "",
            "equation_number": None,
            "association": "",
            "associated_assets": [],
        }
        asset: dict[str, Any] | None = None
        caption = ""
        if isinstance(item, (PictureItem, TableItem)):
            captions = _annotations(item.captions, doc)
            footnotes = _annotations(item.footnotes, doc)
            caption = "\n".join(part["text"] for part in captions)
            block["asset_id"] = item.self_ref
            asset = {
                "asset_id": item.self_ref,
                "kind": "picture",
                "page": page,
                "bbox": bbox,
                "caption": caption,
                "caption_source": "document" if caption else "none",
                "generated_description": "",
                "image_file": None,
                "table": None,
                "captions": captions,
                "footnotes": footnotes,
            }
        if isinstance(item, PictureItem):
            image_name = f"picture-{picture_count}.png"
            picture_count += 1
            image = item.get_image(doc)
            if image is None:
                raise ParserConversionError(f"Picture crop is empty: {item.self_ref}")
            image_file = f"pictures/{image_name}"
            image.save(picture_dir / image_name, format="PNG")
            assert asset is not None
            asset["image_file"] = image_file
            if vlm is not None and not caption:
                asset["_vision_path"] = str(output_dir / image_file)
            assets.append(asset)
        elif isinstance(item, TableItem):
            table = _table_data(item, doc)
            _validate_table_structure(table, item.self_ref)
            image_name = f"table-{table_count}.png"
            table_count += 1
            assert asset is not None
            asset["kind"] = "table"
            asset["table"] = table
            if vlm is not None and not caption:
                image = item.get_image(doc)
                if image is not None:
                    table_crop_dir.mkdir(parents=True, exist_ok=True)
                    image_path = table_crop_dir / image_name
                    image.save(image_path, format="PNG")
                    asset["_vision_path"] = str(image_path)
            assets.append(asset)
        else:
            text = str(getattr(item, "text", "") or "").strip()
            if item.label == DocItemLabel.FORMULA:
                latex = text
                if not latex:
                    raise ParserConversionError(
                        f"Formula extraction returned empty LaTeX: page {page}, {item.self_ref}"
                    )
                original = str(getattr(item, "orig", "") or "").strip()
                number = parse_equation_number(original) or parse_equation_number(latex)
                block["latex"] = latex
                block["original_text"] = original
                block["equation_number"] = number
                text = render_formula_text(latex, number)
            if not text:
                continue
            block["text"] = text
            if item.label in {DocItemLabel.CAPTION, DocItemLabel.FOOTNOTE}:
                block["associated_assets"] = owners.get(item.self_ref, [])
                block["association"] = "document" if item.self_ref in owners else "unlinked"
        blocks.append(block)

    for asset in assets:
        if asset["kind"] == "table" and not asset["table"]["rows"]:
            raise ParserConversionError(f"Table structure is empty: {asset['asset_id']}")
    return {"version": SERVICE_PARSER_VERSION, "blocks": blocks, "assets": assets}


def _location(item: Any) -> tuple[int, dict[str, Any]]:
    if not item.prov:
        raise ParserConversionError(f"PDF item has no page location: {item.self_ref}")
    prov = item.prov[0]
    return int(prov.page_no), prov.bbox.model_dump(mode="json")


def _locations(item: Any) -> list[dict[str, Any]]:
    return [
        {
            "page": int(prov.page_no),
            "bbox": prov.bbox.model_dump(mode="json"),
            "charspan": list(prov.charspan),
        }
        for prov in item.prov
    ]


def _annotations(refs: list[Any], doc: Any) -> list[dict[str, Any]]:
    result: list[dict[str, Any]] = []
    seen: set[str] = set()
    for ref in refs:
        if ref.cref in seen:
            continue
        seen.add(ref.cref)
        item = ref.resolve(doc)
        page, bbox = _location(item)
        result.append(
            {
                "ref": ref.cref,
                "text": str(getattr(item, "text", "") or ""),
                "page": page,
                "bbox": bbox,
                "locations": _locations(item),
            }
        )
    return result


def _table_data(item: Any, doc: Any) -> dict[str, Any]:
    data = item.data
    rows = [["" for _ in range(data.num_cols)] for _ in range(data.num_rows)]
    cells: list[dict[str, Any]] = []
    header_indices: set[int] = set()
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
        cells.append(
            {
                key: raw.get(key)
                for key in (
                    "text",
                    "row_span",
                    "col_span",
                    "start_row_offset_idx",
                    "end_row_offset_idx",
                    "start_col_offset_idx",
                    "end_col_offset_idx",
                    "column_header",
                    "row_header",
                    "row_section",
                )
            }
        )
    header_rows = [list(rows[index]) for index in sorted(header_indices)]
    headers = [
        "\n".join(dict.fromkeys(row[col] for row in header_rows if row[col]))
        for col in range(data.num_cols)
    ]
    footnotes = [part["text"] for part in _annotations(item.footnotes, doc)]
    return {
        "num_rows": data.num_rows,
        "num_cols": data.num_cols,
        "orientation": data.orientation.value,
        "headers": headers,
        "header_rows": header_rows,
        "rows": rows,
        "cells": cells,
        "footnotes": footnotes,
    }


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


def _ocr_diagnostics(pages: list[Any], ocr_path: Path) -> list[dict[str, Any]]:
    """Collect OCR cells and low-confidence warnings; diagnostics stay scratch-only."""
    records: list[dict[str, Any]] = []
    warnings: list[dict[str, Any]] = []
    for page in pages:
        cells = [cell for cell in page.cells if cell.from_ocr]
        for cell in cells:
            records.append(
                {
                    "page": page.page_no,
                    "text": cell.text,
                    "confidence": cell.confidence,
                    "bbox": cell.rect.to_bounding_box().model_dump(mode="json"),
                }
            )
        low = sum(cell.confidence < 0.5 for cell in cells)
        if low:
            warnings.append(
                {
                    "page": page.page_no,
                    "kind": "low_confidence_ocr",
                    "count": low,
                    "message": "Low-confidence OCR text was retained, not discarded; "
                    "verify against the PDF.",
                }
            )
    ocr_path.parent.mkdir(parents=True, exist_ok=True)
    ocr_path.write_text(json.dumps(records, ensure_ascii=False, indent=2), encoding="utf-8")
    return warnings


def _parser_metadata(vlm: VlmSettings | None) -> dict[str, Any]:
    return {
        "version": SERVICE_PARSER_VERSION,
        "service_version": __version__,
        "service": SERVICE_NAME,
        "converter": "docling",
        "docling": DOCLING_VERSION,
        "formula_model": "docling-project/CodeFormulaV2" if vlm is None else vlm.model,
        "formula_revision": FORMULA_REVISION if vlm is None else None,
        "vlm": vlm.describe() if vlm is not None else None,
        "formula_crop_scale": 3.0,
        "formula_crop_expansion": 0.0,
        "formula_batch_size": 1,
        "ocr_confidence_threshold": 0.0,
    }


def finalize_manifest(manifest: dict[str, Any]) -> dict[str, Any]:
    """Strip internal bookkeeping keys from the manifest."""
    for asset in manifest.get("assets", []):
        asset.pop("_vision_path", None)
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


def build_converter(mode: str, *, models_path: Path | None, vlm: VlmSettings | None) -> PdfConverter:
    """Select the converter from ``RAG_PDF_SERVICE_CONVERTER``."""
    normalized = (mode or "").strip().lower()
    if normalized == "fake":
        return FakeConverter()
    if normalized == "docling":
        return DoclingConverter(models_path=models_path, vlm=vlm)
    raise ParserConfigError(
        f"Unknown RAG_PDF_SERVICE_CONVERTER: {mode!r}; expected 'docling' or 'fake'"
    )
