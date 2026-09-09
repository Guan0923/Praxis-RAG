"""Describe uncaptioned Word images using the existing isolated Docling model."""

import argparse
import json
import re
from pathlib import Path


def validate_description(text):
    if not text.strip() or re.search(
        r"i\s+(?:cannot|can't|do not|don't)|unable to|cannot (?:view|see|access|analy[sz]e)|"
        r"i(?:'m| am) sorry|as an ai|抱歉|无法(?:查看|识别|分析|描述)|不能(?:查看|识别|分析)", text, re.I
    ):
        raise ValueError("Vision did not describe the image; refusing to index a refusal or generic response")
    return text


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", required=True, type=Path)
    parser.add_argument("--models", type=Path)
    args = parser.parse_args()
    if args.models is None:
        raise ValueError("RAG_PDF_MODELS_PATH is required for uncaptioned Word images")
    from .pdf_worker import _describe_missing

    raw = json.loads(args.manifest.read_text(encoding="utf-8"))
    missing = [a for a in raw["assets"] if a["kind"] == "picture" and not a["caption"]]
    for asset in missing:
        asset["_vision_file"] = asset["image_file"]
    _describe_missing(missing, args.manifest.parent, args.models, compile_model=False, max_image_edge=384)
    for asset in missing:
        validate_description(asset["generated_description"])
    args.manifest.write_text(json.dumps(raw, ensure_ascii=False), encoding="utf-8")


if __name__ == "__main__":
    main()
