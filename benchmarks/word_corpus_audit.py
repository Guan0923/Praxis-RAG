"""Compare existing Word samples with the reader, recording explicit rejections."""

import argparse
import json
from pathlib import Path
from zipfile import ZipFile

from lxml import etree

from rag_mcp.word_reader import NS, WORD_PARSER_VERSION, parse_docx, render_word


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--sources", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    results = []
    for path in sorted(args.sources.glob("*.docx")):
        with ZipFile(path) as archive:
            source = etree.fromstring(archive.read("word/document.xml"))
        entry = {"name": path.name, "source_tables": len(source.findall(".//w:tbl", NS)),
                 "source_formulas": len(source.findall(".//m:oMath", NS))}
        try:
            result = parse_docx(path)
            entry.update(status="accepted", saved_tables=sum(a["kind"] == "table" for a in result["assets"]),
                         saved_pictures=sum(a["kind"] == "picture" for a in result["assets"]),
                         characters=len(render_word(result)), skipped_object_count=result["skipped_object_count"],
                         diagnostics=result["diagnostics"])
            if path.name == "rich_tables.docx":
                entry["expected_table_labels_present"] = all(value in render_word(result) for value in
                    ("Action Needed", "Cross-Reference Normalization", "Document Clause Ordering"))
        except ValueError as exc:
            entry.update(status="rejected", error=str(exc))
        results.append(entry)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps({"version": WORD_PARSER_VERSION, "documents": results}, ensure_ascii=False, indent=2), encoding="utf-8")
    print(args.output.read_text(encoding="utf-8"))


if __name__ == "__main__":
    main()
