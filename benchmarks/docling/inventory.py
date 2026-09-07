"""Record active package locations and hardware without changing either environment."""

from __future__ import annotations

import argparse
import importlib.metadata
import json
from pathlib import Path
import platform
import subprocess
import sys

from prepare import DEFAULT_OUTPUT


def inventory(output: Path):
    names = ("docling", "docling-core", "docling-ibm-models", "docling-parse", "torch", "torchvision",
             "transformers", "FlagEmbedding", "easyocr", "qdrant-client", "numpy", "psutil",
             "pypdf", "pypdfium2", "python-docx", "Pillow", "pydantic", "tokenizers", "huggingface-hub")
    packages = {}
    for name in names:
        package = importlib.metadata.distribution(name)
        packages[name] = {"version": package.version, "location": str(package.locate_file(""))}
    gpu = subprocess.run(["nvidia-smi", "--query-gpu=name,driver_version,memory.total", "--format=csv,noheader"],
                         capture_output=True, text=True, check=True)
    record = {"python": sys.version, "prefix": sys.prefix, "base_prefix": sys.base_prefix,
              "platform": platform.platform(), "gpu": gpu.stdout.strip(), "packages": packages}
    destination = output / "environment" / "active-runtime.json"
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(record, indent=2), encoding="utf-8")
    print(json.dumps(record, indent=2))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    inventory(parser.parse_args().output.resolve())
