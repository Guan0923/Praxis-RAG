"""Measure real retrieval against separate CUDA conversion/embedding processes."""

from __future__ import annotations

import argparse
import json
import multiprocessing as mp
import os
from pathlib import Path
import random
import statistics
import subprocess
import threading
import time
import traceback
import unicodedata
from uuid import uuid4

from prepare import DEFAULT_OUTPUT
from runtime import converter, model_paths, offline


def save(path: Path, value) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2), encoding="utf-8")


def chunk_groups(output: Path, conversion_run: str):
    from rag_mcp.chunking import DocumentChunk

    corpus = json.loads(Path(__file__).with_name("corpus.json").read_text(encoding="utf-8"))["documents"]
    return [(item, [DocumentChunk(**c) for c in json.loads((output / conversion_run / item["id"] / "chunks.json").read_text(encoding="utf-8"))]) for item in corpus]


def gpu_stats():
    import torch

    return {"allocated_bytes": torch.cuda.memory_allocated(), "reserved_bytes": torch.cuda.memory_reserved(),
            "peak_allocated_bytes": torch.cuda.max_memory_allocated(), "peak_reserved_bytes": torch.cuda.max_memory_reserved()}


def retrieval_worker(connection, output: Path, conversion_run: str, run_dir: Path, url: str, collection: str):
    offline()
    try:
        import torch
        from rag_mcp.config import RagConfig
        from rag_mcp.service_knowledge import RagServiceRuntime

        torch.set_num_threads(4)
        if not torch.cuda.is_available():
            raise RuntimeError("CUDA unavailable; no fallback permitted")
        locks = model_paths(output)
        started = time.perf_counter()
        service = RagServiceRuntime(RagConfig(
            backend="qdrant", qdrant_url=url, collection=collection,
            embedding_model=locks["BAAI/bge-m3"]["path"], reranker_model=locks["BAAI/bge-reranker-base"]["path"],
            embedding_device="cuda:0", reranker_device="cuda:0", preload_reranker=True,
        ), run_dir / "reader-data")
        service.warmup()
        torch.cuda.synchronize()
        loading_seconds = time.perf_counter() - started
        tenant = service._tenant("benchmark")
        started = time.perf_counter()
        count = 0
        for _, chunks in chunk_groups(output, conversion_run):
            vectors = service._embedder.embed_documents([c.text for c in chunks])
            sparse = service._sparse_encoder.encode_documents([c.text for c in chunks])
            count += tenant.ingester._store.replace_document(chunks, vectors, sparse)
        connection.send({"event": "ready", "pid": os.getpid(), "loading_seconds": loading_seconds,
                         "indexing_seconds": time.perf_counter() - started, "chunks": count, "gpu": gpu_stats()})
        while True:
            message = connection.recv()
            if message["command"] == "stop":
                break
            started = time.perf_counter()
            try:
                result = service.search("benchmark", {"query": message["query"], "retrieval": "hybrid",
                                                     "rerank": "cross_encoder", "candidate_k": 10, "top_k": 5})
                torch.cuda.synchronize()
                connection.send({"event": "result", "seconds": time.perf_counter() - started,
                                 "result": result, "gpu": gpu_stats()})
            except Exception as exc:
                connection.send({"event": "search_error", "seconds": time.perf_counter() - started,
                                 "error": f"{type(exc).__name__}: {exc}", "gpu": gpu_stats()})
        service._client.close()
    except Exception as exc:
        traceback.print_exc()
        connection.send({"event": "fatal", "error": f"{type(exc).__name__}: {exc}"})
    finally:
        connection.close()


def import_worker(connection, stop_stage, output: Path, conversion_run: str, url: str, collection: str):
    offline()
    try:
        import torch
        from rag_mcp.embeddings import BgeM3Embedder
        from rag_mcp.sparse import Bm25SparseEncoder
        from rag_mcp.storage.qdrant_store import QdrantStore

        instance, initializing, _ = converter(output)
        embedder = BgeM3Embedder(model_paths(output)["BAAI/bge-m3"]["path"], device="cuda:0")
        started = time.perf_counter()
        embedder.warmup()
        store = QdrantStore(collection=collection, url=url, namespace="import-staging")
        sparse_encoder = Bm25SparseEncoder()
        groups = chunk_groups(output, conversion_run)
        connection.send({"event": "ready", "pid": os.getpid(), "docling_initialization_seconds": initializing,
                         "embedding_loading_seconds": time.perf_counter() - started, "gpu": gpu_stats()})
        while True:
            message = connection.recv()
            phase = message["command"]
            if phase == "stop":
                break
            stop_stage.clear()
            torch.cuda.reset_peak_memory_stats()
            connection.send({"event": "started", "phase": phase, "at": time.time()})
            measurements = []
            try:
                index = 0
                while not stop_stage.is_set():
                    sample, chunks = groups[index % len(groups)]
                    started = time.perf_counter()
                    if phase == "conversion":
                        result = instance.convert(output / "sources" / sample["file"], raises_on_error=True,
                                                  max_file_size=10 * 1024 * 1024, max_num_pages=1000)
                        if result.status.value != "success":
                            raise RuntimeError(f"Conversion status: {result.status.value}")
                        del result
                    elif phase == "embedding":
                        vectors = embedder.embed_documents([c.text for c in chunks])
                        sparse = sparse_encoder.encode_documents([c.text for c in chunks])
                        store.replace_document(chunks, vectors, sparse)
                    else:
                        raise ValueError(f"Unknown phase: {phase}")
                    torch.cuda.synchronize()
                    measurements.append({"id": sample["id"], "seconds": time.perf_counter() - started})
                    index += 1
                connection.send({"event": "phase_done", "phase": phase, "at": time.time(),
                                 "documents": measurements, "gpu": gpu_stats()})
            except Exception as exc:
                traceback.print_exc()
                connection.send({"event": "phase_failed", "phase": phase, "error": f"{type(exc).__name__}: {exc}",
                                 "documents": measurements, "gpu": gpu_stats()})
                break
        store._client.close()
    except Exception as exc:
        traceback.print_exc()
        connection.send({"event": "fatal", "error": f"{type(exc).__name__}: {exc}"})
    finally:
        connection.close()


def receive(connection, timeout=600):
    if not connection.poll(timeout):
        raise TimeoutError("Worker response deadline exceeded")
    message = connection.recv()
    if message["event"] in {"fatal", "phase_failed"}:
        raise RuntimeError(json.dumps(message, ensure_ascii=False))
    return message


def normalized(text: str) -> str:
    return "".join(c for c in unicodedata.normalize("NFKC", text).casefold() if c.isalnum())


def measure(reader, question: dict, phase: str, source_names: dict) -> dict:
    started = time.perf_counter()
    reader.send({"command": "search", "query": question["query"]})
    response = receive(reader)
    seconds = time.perf_counter() - started
    records = response.get("result", {}).get("results", [])
    hit = any(item["source"] == source_names[question["document"]] and
              all(normalized(term) in normalized(item["text"]) for term in question["terms"]) for item in records)
    return {"question": question["id"], "phase": phase, "seconds": seconds,
            "evidence_hit": hit, "response": response}


def summary(records: list[dict]) -> dict:
    values = sorted(r["seconds"] for r in records)
    return {"requests": len(records), "p50_seconds": statistics.median(values),
            "p95_seconds": values[max(0, int(len(values) * 0.95 + 0.999) - 1)],
            "errors": sum(r["response"]["event"] != "result" for r in records),
            "evidence_hits": sum(r["evidence_hit"] for r in records)}


def monitor(stop, pids: list[int], destination: Path):
    import psutil

    with destination.open("w", encoding="utf-8") as handle:
        while not stop.is_set():
            record = {"at": time.time(), "process_rss": {}}
            for pid in pids:
                try:
                    record["process_rss"][str(pid)] = psutil.Process(pid).memory_info().rss
                except psutil.Error:
                    pass
            try:
                info = subprocess.run(["nvidia-smi", "--query-gpu=memory.used,utilization.gpu", "--format=csv,noheader,nounits"],
                                      capture_output=True, text=True, timeout=5, creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
                if info.returncode == 0:
                    used, utilization = info.stdout.strip().splitlines()[0].split(",")
                    record.update(gpu_used_mib=int(used), gpu_utilization=int(utilization))
                else:
                    record["gpu_monitor_error"] = info.stderr.strip()
            except Exception as exc:
                record["gpu_monitor_error"] = str(exc)
            handle.write(json.dumps(record) + "\n")
            handle.flush()
            stop.wait(1)


def run(output: Path, conversion_run: str, url: str):
    context = mp.get_context("spawn")
    run_id = time.strftime("%Y%m%d_%H%M%S") + "_" + uuid4().hex[:6]
    run_dir = output / "runs" / run_id
    run_dir.mkdir(parents=True)
    report = {"run_id": run_id, "started_at": time.time(), "qdrant_url": url, "conversion_run": conversion_run,
              "scope": "Phase-one IPC retrieval timings; production HTTP/transaction workflow is not implemented.",
              "parallel_design": "50 searches during sustained conversion, then 50 during sustained embedding and Qdrant writes; not whole-upload latency.",
              "records": []}
    save(output / "latest-run.json", {"path": str(run_dir)})
    reader, child_reader = context.Pipe()
    importer, child_importer = context.Pipe()
    stage_stop = context.Event()
    workers = [context.Process(target=retrieval_worker, args=(child_reader, output, conversion_run, run_dir, url, "docling_read_" + run_id)),
               context.Process(target=import_worker, args=(child_importer, stage_stop, output, conversion_run, url, "docling_import_" + run_id))]
    monitoring_stop = threading.Event()
    tracked_pids = []
    monitoring = threading.Thread(target=monitor, args=(monitoring_stop, tracked_pids, run_dir / "resources.jsonl"), daemon=True)
    monitoring.start()
    try:
        workers[0].start()
        tracked_pids.append(workers[0].pid)
        report["reader"] = receive(reader)
        print(f"Reader ready: {report['reader']['chunks']} chunks", flush=True)
        workers[1].start()
        tracked_pids.append(workers[1].pid)
        report["importer"] = receive(importer)
        print("Importer ready on CUDA", flush=True)
        questions = json.loads(Path(__file__).with_name("questions.json").read_text(encoding="utf-8"))
        corpus = json.loads(Path(__file__).with_name("corpus.json").read_text(encoding="utf-8"))["documents"]
        source_names = {item["id"]: item["file"] for item in corpus}
        for question in questions[:5]:
            measure(reader, question, "warmup", source_names)
        report["quality"] = [measure(reader, question, "quality", source_names) for question in questions]
        sequence = [questions[i % len(questions)] for i in range(100)]
        random.Random(0).shuffle(sequence)
        for question in sequence:
            report["records"].append(measure(reader, question, "baseline", source_names))
        save(run_dir / "report.json", report)
        print("100 baseline searches complete", flush=True)
        for phase, selected in (("conversion", sequence[:50]), ("embedding", sequence[50:])):
            importer.send({"command": phase})
            report[phase + "_start"] = receive(importer)
            for question in selected:
                if importer.poll():
                    receive(importer)
                    raise RuntimeError("Importer stopped before concurrent requests completed")
                report["records"].append(measure(reader, question, phase, source_names))
            stage_stop.set()
            report[phase + "_end"] = receive(importer)
            save(run_dir / "report.json", report)
            print(f"50 {phase}-concurrent searches complete", flush=True)
        report["summaries"] = {phase: summary([r for r in report["records"] if r["phase"] == phase]) for phase in ("baseline", "conversion", "embedding")}
        report["summaries"]["concurrent"] = summary([r for r in report["records"] if r["phase"] != "baseline"])
        report["quality_summary"] = summary(report["quality"])
        print(json.dumps(report["summaries"], indent=2), flush=True)
    except Exception as exc:
        report["fatal_error"] = f"{type(exc).__name__}: {exc}"
        raise
    finally:
        monitoring_stop.set()
        if monitoring:
            monitoring.join(10)
        stage_stop.set()
        for connection, process in zip((reader, importer), workers, strict=True):
            if process.pid is None:
                continue
            if process.is_alive():
                try:
                    connection.send({"command": "stop"})
                except (OSError, EOFError):
                    pass
                process.join(10)
            if process.is_alive():
                process.terminate()
                process.join(10)
            connection.close()
        report["finished_at"] = time.time()
        save(run_dir / "report.json", report)
        print(f"Report: {run_dir / 'report.json'}", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument("--conversion-run", default="conversion-cached")
    parser.add_argument("--qdrant-url", required=True)
    args = parser.parse_args()
    run(args.output.resolve(), args.conversion_run, args.qdrant_url)
