"""Verify real CUDA formula generation reuses its cache on the stalled PDF."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import time

from prepare import DEFAULT_OUTPUT
from runtime import converter, convert_document, formula_engine, offline


def verify(output: Path):
    offline()
    import torch

    destination = output / "cache-verification" / time.strftime("%Y%m%d_%H%M%S")
    destination.mkdir(parents=True)
    report = {"status": "running", "forward_calls": 0, "cached_decode_calls": 0,
              "cache_output_calls": 0, "disabled_cache_calls": 0, "max_cached_tokens": 0}
    handles = []

    def before_forward(module, args, kwargs):
        report["forward_calls"] += 1
        if kwargs.get("use_cache") is not True:
            report["disabled_cache_calls"] += 1
        cache = kwargs.get("past_key_values")
        previous_tokens = int(cache.get_seq_length()) if cache is not None else 0
        report["max_cached_tokens"] = max(report["max_cached_tokens"], previous_tokens)
        ids = kwargs.get("input_ids")
        if previous_tokens > 0 and ids is not None and ids.shape[-1] == 1:
            report["cached_decode_calls"] += 1
        if report["forward_calls"] <= 3:
            print(f"Formula call {report['forward_calls']}: use_cache={kwargs.get('use_cache')}, cached_tokens={previous_tokens}, input_tokens={ids.shape[-1] if ids is not None else None}", flush=True)

    def after_forward(module, args, result):
        if getattr(result, "past_key_values", None) is not None:
            report["cache_output_calls"] += 1

    try:
        instance, initialized, options = converter(output)
        report.update(initialization_seconds=initialized, options=options)
        model = formula_engine(instance).vlm_model
        report["device"] = str(next(model.parameters()).device)
        handles.append(model.register_forward_pre_hook(before_forward, with_kwargs=True))
        handles.append(model.register_forward_hook(after_forward))
        print(f"Converting original six-page PDF; formula model on {report['device']}", flush=True)
        torch.cuda.reset_peak_memory_stats()
        _, converted = convert_document(instance, output / "sources/mineru_demo2.pdf", destination / "mineru_demo2")
        report["conversion"] = converted
        report["peak_cuda_allocated_bytes"] = torch.cuda.max_memory_allocated()
        if report["disabled_cache_calls"] or not report["cached_decode_calls"] or not report["cache_output_calls"]:
            raise RuntimeError("Real generation did not consistently reuse the cache")
        if converted["status"] != "success":
            raise RuntimeError(f"Conversion status: {converted['status']}")
        report["status"] = "cache_verified"
    except BaseException as exc:
        report.update(status="failed_or_interrupted", error=f"{type(exc).__name__}: {exc}")
        raise
    finally:
        for handle in handles:
            handle.remove()
        (destination / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
        print(json.dumps({k: v for k, v in report.items() if k not in {"conversion", "options"}}, indent=2), flush=True)
        print(f"Report: {destination / 'report.json'}", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    verify(parser.parse_args().output.resolve())
