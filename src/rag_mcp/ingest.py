"""Command-line document ingestion for the Qdrant hybrid knowledge base."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
from pathlib import Path

from .chunking import chunk_document, discover_documents
from .config import RagConfig, RagConfigError
from .orchestrator import build_qdrant_dependencies
from .service_knowledge import DenseDocumentIngester


@dataclass(frozen=True)
class IngestStats:
    """Bounded ingestion summary suitable for terminal debugging."""

    discovered_files: int
    imported_files: int
    imported_chunks: int
    failed_files: int


def ingest_path(path: Path, config: RagConfig) -> IngestStats:
    """Chunk, embed, and idempotently replace every supported document."""

    if config.backend != "qdrant":
        raise RagConfigError("Document ingestion requires RAG_BACKEND=qdrant")
    resolved_path = path.expanduser().resolve()
    documents = discover_documents(resolved_path)
    source_root = resolved_path if resolved_path.is_dir() else resolved_path.parent
    embedder, sparse_encoder, store = build_qdrant_dependencies(config)
    ingester = DenseDocumentIngester(config, embedder, sparse_encoder, store)
    imported_files = 0
    imported_chunks = 0
    failed_files = 0
    for document in documents:
        try:
            if document.suffix.casefold() in {".pdf", ".doc", ".docx"}:
                indexed = ingester.ingest(document, source_root=source_root)
                imported_chunks += indexed.chunk_count
                imported_files += 1
                print(f"OK   {document} ({indexed.chunk_count} chunks)")
                continue
            chunks = chunk_document(document, source_root=source_root, tokenizer_model=config.embedding_model)
            if not chunks:
                print(f"SKIP {document}: empty document")
                continue
            vectors = embedder.embed_documents([chunk.text for chunk in chunks])
            sparse_vectors = sparse_encoder.encode_documents([chunk.text for chunk in chunks])
            imported_chunks += store.replace_document(chunks, vectors, sparse_vectors)
            imported_files += 1
            print(f"OK   {document} ({len(chunks)} chunks)")
        except Exception as exc:
            failed_files += 1
            print(f"FAIL {document}: {exc}")
    return IngestStats(
        discovered_files=len(documents),
        imported_files=imported_files,
        imported_chunks=imported_chunks,
        failed_files=failed_files,
    )


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Import Markdown/TXT/PDF/DOCX/DOC documents into Qdrant with BGE-M3 and BM25 sparse vectors."
    )
    parser.add_argument("path", type=Path, help="Document file or directory")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    """Run one import and return nonzero when any document failed."""

    args = parse_args(argv)
    try:
        stats = ingest_path(args.path, RagConfig.from_env())
    except (FileNotFoundError, RagConfigError, ValueError) as exc:
        raise SystemExit(str(exc)) from exc
    print(
        "SUMMARY "
        f"discovered={stats.discovered_files} imported={stats.imported_files} "
        f"chunks={stats.imported_chunks} failed={stats.failed_files}"
    )
    return 1 if stats.failed_files else 0


if __name__ == "__main__":
    raise SystemExit(main())
