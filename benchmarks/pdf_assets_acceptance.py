"""Real PDF -> BGE-M3 -> Qdrant acceptance run for picture evidence."""

from __future__ import annotations

import argparse
import base64
import json
import shutil
import time
from pathlib import Path

from qdrant_client import QdrantClient

from rag_mcp.config import RagConfig
from rag_mcp.embeddings import BgeM3Embedder
from rag_mcp.pdf_assets import project_results
from rag_mcp.retrievers.common import project_candidates
from rag_mcp.service_knowledge import DenseDocumentIngester
from rag_mcp.sparse import Bm25SparseEncoder
from rag_mcp.storage.qdrant_store import QdrantStore


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--pdf", type=Path, required=True)
    parser.add_argument("--work", type=Path, required=True)
    parser.add_argument("--parser-python", type=Path, required=True)
    parser.add_argument("--models", type=Path, required=True)
    parser.add_argument("--embedding-model", type=Path, required=True)
    parser.add_argument("--query", default="Self-locking nuts")
    args = parser.parse_args()

    knowledge = args.work.resolve() / "knowledge"
    knowledge.mkdir(parents=True, exist_ok=True)
    source = knowledge / args.pdf.name
    shutil.copyfile(args.pdf.resolve(), source)
    config = RagConfig(
        backend="qdrant", collection="pdf_assets_acceptance_20260909",
        embedding_model=str(args.embedding_model.resolve()), embedding_device="cuda:0",
        pdf_parser_python=args.parser_python.resolve(), pdf_models_path=args.models.resolve(),
    )
    client = QdrantClient(":memory:")
    store = QdrantStore(collection=config.collection, client=client, namespace="acceptance")
    embedder = BgeM3Embedder(config.embedding_model, device=config.embedding_device, batch_size=4)
    sparse = Bm25SparseEncoder()
    ingester = DenseDocumentIngester(config, embedder, sparse, store)
    started = time.perf_counter()
    indexed = ingester.ingest(source, source_root=knowledge)
    dense = embedder.embed_query(args.query)
    sparse_query = sparse.encode_query(args.query)
    points = store.hybrid_search(dense, sparse_query, limit=10, filters={})
    candidates = project_candidates(points)
    results = project_results(candidates[:3], store, knowledge / ".pdf-assets")
    public = []
    for result in results:
        item = dict(result)
        asset = dict(item.get("asset") or {})
        data = asset.pop("data", None)
        if data:
            asset["decoded_bytes"] = len(base64.b64decode(data))
        item["asset"] = asset
        public.append(item)
    report = {
        "date": "2026-09-09", "seconds": round(time.perf_counter() - started, 2),
        "document_id": indexed.document_id, "chunk_count": indexed.chunk_count,
        "qdrant_points": client.count(config.collection, exact=True).count,
        "query": args.query, "results": public,
    }
    args.work.mkdir(parents=True, exist_ok=True)
    (args.work / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps(report, ensure_ascii=False))


if __name__ == "__main__":
    main()
