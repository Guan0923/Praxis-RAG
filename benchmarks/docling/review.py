"""Prepare independent source-versus-output evidence, never self-certify accuracy."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import textwrap
import unicodedata

from PIL import Image, ImageDraw, ImageFont
from docling_core.types.doc import DoclingDocument

from prepare import DEFAULT_OUTPUT


def normal(text):
    return "".join(c for c in unicodedata.normalize("NFKC", text).casefold() if c.isalnum())


def font(size):
    return ImageFont.truetype("C:/Windows/Fonts/msyh.ttc", size)


def item_text(item):
    if "data" in item and "table_cells" in item["data"]:
        rows = {}
        for cell in item["data"]["table_cells"]:
            rows.setdefault(cell["start_row_offset_idx"], []).append(cell)
        return "\n".join(" | ".join(c["text"] for c in sorted(cells, key=lambda c: c["start_col_offset_idx"])) for _, cells in sorted(rows.items()))
    return item.get("text", "")


def review(output, conversion_run):
    checks = json.loads(Path(__file__).with_name("checks.json").read_text(encoding="utf-8"))
    destination = output / "quality-review"
    destination.mkdir(parents=True, exist_ok=True)
    sources = {s["file"]: s for s in json.loads((output / "source-review/source-content.json").read_text(encoding="utf-8"))}
    corpus = {s["id"]: s for s in json.loads(Path(__file__).with_name("corpus.json").read_text(encoding="utf-8"))["documents"]}
    results = []
    for check in checks:
        record = dict(check, human_status="not_reviewed")
        source = corpus[check["document"]]
        converted = output / conversion_run / check["document"] / "document.json"
        if not converted.exists():
            record["availability"] = "not_converted"
            results.append(record)
            continue
        doc = json.loads(converted.read_text(encoding="utf-8"))
        native = DoclingDocument.model_validate(doc)
        all_items = [item.model_dump(mode="json") for item, _ in native.iterate_items()]
        page = check.get("page")
        items = [i for i in all_items if page is None or any(p["page_no"] == page for p in i["prov"])]
        if check["kind"] == "table":
            candidates = [i for i in items if i["label"] == "table"]
        elif check["kind"] == "formula":
            candidates = [i for i in items if i["label"] == "formula"]
        else:
            candidates = items
        extracted = "\n\n".join(f"{i['self_ref']} [{i['label']}]\n{item_text(i)}" for i in candidates)
        record["term_check"] = all(normal(t) in normal(extracted) for t in check["expected"]) if "expected" in check else None
        record["term_check_scope"] = "Same page/document only; does not validate row association, reading order or semantic accuracy."
        record["candidates"] = candidates
        (destination / f"check-{check['id']:02d}.txt").write_text(extracted, encoding="utf-8")
        if page:
            image = Image.open(output / "source-review" / f"{Path(source['file']).stem}-page-{page}.png").convert("RGB")
            draw = ImageDraw.Draw(image)
            size = doc["pages"][str(page)]["size"]
            sx, sy = image.width / size["width"], image.height / size["height"]
            for number, item in enumerate(items, 1):
                for prov in item["prov"]:
                    if prov["page_no"] != page:
                        continue
                    box = prov["bbox"]
                    top, bottom = box["t"], box["b"]
                    if box["coord_origin"] == "BOTTOMLEFT":
                        top, bottom = size["height"] - top, size["height"] - bottom
                    rect = (box["l"] * sx, min(top, bottom) * sy, box["r"] * sx, max(top, bottom) * sy)
                    color = "#b50027" if item["label"] in {"table", "picture", "formula"} else "#167c3a"
                    draw.rectangle(rect, outline=color, width=2)
                    draw.text((rect[0], rect[1]), str(number), fill=color, font=font(11))
            image.save(destination / f"check-{check['id']:02d}-regions.png")
            record["source_screenshot"] = str(output / "source-review" / f"{Path(source['file']).stem}-page-{page}.png")
        else:
            original = sources[source["file"]]
            original_text = "\n".join(original["paragraphs"]) + "\n" + json.dumps(original["tables"], ensure_ascii=False, indent=2)
            (destination / f"check-{check['id']:02d}-source-ooxml.txt").write_text(original_text, encoding="utf-8")
        lines = []
        for line in extracted.splitlines():
            lines.extend(textwrap.wrap(line, width=80) or [""])
        # Paginate all extracted text, rather than silently cutting long tables.
        for start in range(0, len(lines), 55):
            panel = Image.new("RGB", (1500, 1450), "white")
            draw = ImageDraw.Draw(panel)
            draw.text((20, 10), f"Check {check['id']:02d}: parsed output, panel {start // 55 + 1}", fill="black", font=font(20))
            for n, line in enumerate(lines[start:start + 55]):
                draw.text((20, 55 + n * 24), line, fill="black", font=font(17))
            panel.save(destination / f"check-{check['id']:02d}-parsed-{start // 55 + 1}.png")
        results.append(record)
    (destination / "checks.json").write_text(json.dumps(results, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"Prepared {len(results)} checks; manual decisions remain separate: {destination}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument("--conversion-run", default="conversion-cached")
    args = parser.parse_args()
    review(args.output.resolve(), args.conversion_run)
