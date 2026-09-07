"""Download bounded, pinned public samples without accessing existing RAG data."""

from __future__ import annotations

import argparse
import json
import time
from pathlib import Path
from urllib.parse import urlparse
from zipfile import is_zipfile

import httpx

MAX_BYTES = 10 * 1024 * 1024
ROOT = Path(__file__).resolve().parents[2]
DEFAULT_OUTPUT = ROOT / ".mini_agent-rag" / "docling-benchmark"


def prepare(output: Path) -> None:
    corpus = json.loads(Path(__file__).with_name("corpus.json").read_text(encoding="utf-8"))
    destination = output / "sources"
    destination.mkdir(parents=True, exist_ok=True)
    records = []
    with httpx.Client(follow_redirects=True, timeout=120) as client:
        for item in corpus["documents"]:
            if urlparse(item["url"]).hostname not in {"raw.githubusercontent.com", "arxiv.org"}:
                raise ValueError("Unapproved corpus host")
            target = destination / item["file"]
            if target.parent != destination or target.suffix not in {".pdf", ".docx"}:
                raise ValueError("Invalid corpus filename")
            if not target.exists():
                temporary = target.with_suffix(target.suffix + ".partial")
                try:
                    with client.stream("GET", item["url"]) as response:
                        response.raise_for_status()
                        size = 0
                        with temporary.open("wb") as handle:
                            for chunk in response.iter_bytes():
                                size += len(chunk)
                                if size > MAX_BYTES:
                                    raise ValueError(f"Sample exceeds 10 MiB: {item['id']}")
                                handle.write(chunk)
                    temporary.replace(target)
                finally:
                    temporary.unlink(missing_ok=True)
            if not 0 < target.stat().st_size <= MAX_BYTES:
                raise ValueError(f"Invalid sample size: {target}")
            if target.suffix == ".pdf":
                with target.open("rb") as handle:
                    if not handle.read(1024).lstrip().startswith(b"%PDF"):
                        raise ValueError(f"Not a PDF: {target}")
            elif not is_zipfile(target):
                raise ValueError(f"Not a DOCX archive: {target}")
            records.append({**item, "bytes": target.stat().st_size})
            print(f"{item['id']}: {target.stat().st_size} bytes", flush=True)
    (output / "sources.json").write_text(
        json.dumps({"verified_at_unix": time.time(), "documents": records}, indent=2), encoding="utf-8"
    )


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    prepare(parser.parse_args().output.resolve())
