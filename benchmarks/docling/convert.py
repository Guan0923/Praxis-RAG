"""Convert the real corpus offline with the fixed CUDA pipeline."""

from __future__ import annotations

import argparse
from dataclasses import asdict
import json
import os
from pathlib import Path
import subprocess
import threading
import time
import traceback

from prepare import DEFAULT_OUTPUT
from runtime import chunks_from_document, convert_document, converter, offline


def monitor_resources(stop, state, samples, destination):
    import psutil

    process = psutil.Process(os.getpid())
    with destination.open("w", encoding="utf-8") as handle:
        while not stop.is_set():
            record = {"at": time.time(), "phase": state["phase"], "rss_bytes": process.memory_info().rss}
            try:
                info = subprocess.run(
                    ["nvidia-smi", "--query-gpu=memory.used,utilization.gpu", "--format=csv,noheader,nounits"],
                    capture_output=True, text=True, timeout=5,
                    creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
                )
                if info.returncode == 0:
                    used, utilization = info.stdout.strip().splitlines()[0].split(",")
                    record.update(gpu_used_mib=int(used), gpu_utilization=int(utilization))
                else:
                    record["gpu_monitor_error"] = info.stderr.strip()
            except Exception as exc:
                record["gpu_monitor_error"] = str(exc)
            samples.append(record)
            handle.write(json.dumps(record) + "\n")
            handle.flush()
            stop.wait(1)


def resource_peak(samples, phase):
    selected = [item for item in samples if item["phase"] == phase]
    return {
        "peak_process_rss_bytes": max((item["rss_bytes"] for item in selected), default=None),
        "peak_system_gpu_used_mib": max((item.get("gpu_used_mib", 0) for item in selected), default=None),
        "peak_system_gpu_utilization": max((item.get("gpu_utilization", 0) for item in selected), default=None),
        "resource_samples": len(selected),
    }


def run(output: Path, selected: list[str] | None, run_name: str) -> None:
    offline()
    import torch

    root = output / run_name
    root.mkdir(parents=True, exist_ok=True)
    report = {"started_at": time.time(), "documents": []}
    resource_stop = threading.Event()
    resource_state = {"phase": "initialization"}
    resource_samples = []
    resource_thread = threading.Thread(
        target=monitor_resources,
        args=(resource_stop, resource_state, resource_samples, root / "resources.jsonl"),
        daemon=True,
    )
    resource_thread.start()
    try:
        instance, initialized, options = converter(output)
        report.update(initialization_seconds=initialized, initialization_resources=resource_peak(resource_samples, "initialization"), options=options)
        corpus = json.loads(Path(__file__).with_name("corpus.json").read_text(encoding="utf-8"))["documents"]
        for sample in corpus:
            if selected and sample["id"] not in selected:
                continue
            print(f"Converting {sample['id']}", flush=True)
            destination = root / sample["id"]
            resource_state["phase"] = sample["id"]
            torch.cuda.reset_peak_memory_stats()
            try:
                doc, details = convert_document(instance, output / "sources" / sample["file"], destination)
                details["id"] = sample["id"]
                details["peak_cuda_allocated_bytes"] = torch.cuda.max_memory_allocated()
                if details["status"] == "success":
                    chunks = chunks_from_document(doc, sample["id"], sample["file"], output)
                    (destination / "chunks.json").write_text(json.dumps([asdict(c) for c in chunks], ensure_ascii=False, indent=2), encoding="utf-8")
                    details["chunks"] = len(chunks)
                details.update(resource_peak(resource_samples, sample["id"]))
                report["documents"].append({key: value for key, value in details.items() if key != "elements"})
                print(f"{sample['id']}: {details['status']}, {details['conversion_seconds']:.2f}s, {details['tables']} tables", flush=True)
            except Exception as exc:
                traceback.print_exc()
                report["documents"].append({"id": sample["id"], "status": "exception", "error": f"{type(exc).__name__}: {exc}"})
                if isinstance(exc, torch.cuda.OutOfMemoryError):
                    raise
            finally:
                (root / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    except Exception as exc:
        report["fatal_error"] = f"{type(exc).__name__}: {exc}"
        raise
    finally:
        resource_state["phase"] = "finished"
        resource_stop.set()
        resource_thread.join(10)
        report["finished_at"] = time.time()
        (root / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument("--only", nargs="+")
    parser.add_argument("--run-name", default="conversion-cached")
    args = parser.parse_args()
    run(args.output.resolve(), args.only, args.run_name)
