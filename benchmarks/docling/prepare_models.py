"""Download only selected local models and record immutable HF revisions."""

from __future__ import annotations

import argparse
import importlib.metadata
import json
from pathlib import Path

from huggingface_hub import snapshot_download

from prepare import DEFAULT_OUTPUT


def prepare_models(output: Path) -> None:
    from docling.models.stages.ocr.easyocr_model import EasyOcrModel

    models = output / "models"
    models.mkdir(parents=True, exist_ok=True)
    lock_file = output / "environment" / "model-revisions.json"
    lock_file.parent.mkdir(parents=True, exist_ok=True)
    locked = json.loads(lock_file.read_text(encoding="utf-8")) if lock_file.exists() else {}
    revisions = json.loads(Path(__file__).with_name("models.json").read_text(encoding="utf-8"))
    for repo in ("docling-project/docling-layout-heron", "docling-project/docling-models", "docling-project/CodeFormulaV2"):
        if repo not in locked:
            locked[repo] = {"revision": revisions[repo]}
            lock_file.write_text(json.dumps(locked, indent=2), encoding="utf-8")
        if locked[repo]["revision"] != revisions[repo]:
            raise ValueError(f"Existing model lock differs from benchmark: {repo}")
        print(f"Downloading {repo} at {locked[repo]['revision']}", flush=True)
        snapshot_download(
            repo, revision=locked[repo]["revision"], local_dir=models / repo.replace("/", "--"),
            token=False, max_workers=2,
        )
    ocr = models / "EasyOcr"
    if not (ocr / "craft_mlt_25k.pth").exists() or not (ocr / "zh_sim_g2.pth").exists():
        EasyOcrModel.download_models(
            recognition_models=["zh_sim_g2"], local_dir=ocr, progress=False,
        )
    locked["EasyOCR"] = {"package": importlib.metadata.version("easyocr"), "languages": ["ch_sim", "en"]}
    for repo in ("BAAI/bge-m3", "BAAI/bge-reranker-base"):
        try:
            location = Path(snapshot_download(repo, revision=revisions[repo], local_files_only=True))
        except Exception:
            location = Path(snapshot_download(repo, revision=revisions[repo], local_dir=models / repo.replace("/", "--"), token=False))
        locked[repo] = {"path": str(location), "revision": revisions[repo]}
    lock_file.write_text(json.dumps(locked, indent=2), encoding="utf-8")
    packages = sorted((d.metadata["Name"], d.version) for d in importlib.metadata.distributions() if d.metadata["Name"])
    (lock_file.parent / "model-packages.txt").write_text("\n".join(f"{name}=={version}" for name, version in packages) + "\n", encoding="utf-8")
    print(f"Model preparation complete: {lock_file}", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    prepare_models(parser.parse_args().output.resolve())
