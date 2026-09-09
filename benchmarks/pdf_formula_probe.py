"""Compare local CodeFormulaV2 crops against saved Docling formula locations."""

import argparse
import json
import time
from pathlib import Path
from types import SimpleNamespace


def main():
    import pypdfium2 as pdfium
    from docling.datamodel.accelerator_options import AcceleratorDevice, AcceleratorOptions
    from docling.datamodel.pipeline_options import CodeFormulaVlmOptions
    from docling.datamodel.vlm_engine_options import TransformersVlmEngineOptions
    from docling.models.stages.code_formula.code_formula_vlm_model import CodeFormulaVlmModel
    from docling.models.inference_engines.vlm import VlmEngineInput
    from rag_mcp.pdf_worker import _FORMULA_REVISION, _enable_formula_cache

    parser = argparse.ArgumentParser()
    parser.add_argument("--pdf", type=Path, required=True)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--models", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    options = CodeFormulaVlmOptions.from_preset("codeformulav2")
    options.model_spec.revision = _FORMULA_REVISION
    options.extract_code = False
    options.engine_options = TransformersVlmEngineOptions(
        device=AcceleratorDevice.CUDA, compile_model=False, use_kv_cache=True,
    )
    model = CodeFormulaVlmModel(True, False, args.models, options,
                               AcceleratorOptions(device=AcceleratorDevice.CUDA, num_threads=4))
    _enable_formula_cache(SimpleNamespace(initialized_pipelines={0: SimpleNamespace(enrichment_pipe=[model])}))
    manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
    formulas = [block for block in manifest["blocks"] if block["kind"] == "formula"]
    reports = []
    with pdfium.PdfDocument(args.pdf) as pdf:
        for scale, expansion in ((1.67, 0.18), (1.67, 0.0), (2.0, 0.0), (3.0, 0.0)):
            for block in formulas:
                if block["ref"] not in {"#/texts/33", "#/texts/41", "#/texts/47"}:
                    continue
                page = pdf[block["page"] - 1]
                image = page.render(scale=scale).to_pil()
                box = block["bbox"]
                left, right = box["l"], box["r"]
                top, bottom = box["t"], box["b"]
                if box["coord_origin"] == "BOTTOMLEFT":
                    top, bottom = page.get_height() - top, page.get_height() - bottom
                dx, dy = (right - left) * expansion / 2, (bottom - top) * expansion / 2
                crop = image.crop((max(0, int((left - dx) * scale)), max(0, int((top - dy) * scale)),
                                   min(image.width, int((right + dx) * scale)), min(image.height, int((bottom + dy) * scale))))
                name = f"{block['ref'].rsplit('/', 1)[-1]}-{scale}-{expansion}"
                crop.save(args.output / f"{name}.png")
                started = time.perf_counter()
                response = model.engine.predict_batch([VlmEngineInput(
                    image=crop, prompt="<formula>", temperature=0, max_new_tokens=2048,
                    extra_generation_config={"skip_special_tokens": False},
                )])[0]
                report = {"ref": block["ref"], "scale": scale, "expansion": expansion,
                          "latex": model._post_process([response.text])[0], "raw": response.text,
                          "metadata": response.metadata, "seconds": time.perf_counter() - started}
                reports.append(report)
                (args.output / "report.json").write_text(json.dumps(reports, ensure_ascii=False, indent=2), encoding="utf-8")
                print(json.dumps(report, ensure_ascii=False), flush=True)
                image.close()
                page.close()


if __name__ == "__main__":
    main()
