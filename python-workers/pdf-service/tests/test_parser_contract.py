"""Pure-unit tests for the manifest contract, formula rendering and validation.

These tests never import docling: they cover the exporters' pure helpers plus
the strict contract validator that protects the Go backend.
"""

from __future__ import annotations

import copy
import json
from pathlib import Path

import pytest

from app.parser import (
    FAKE_LATEX,
    SERVICE_PARSER_VERSION,
    FakeConverter,
    ParserConfigError,
    ParserConversionError,
    build_converter,
    clean_description,
    fake_manifest,
    parse_equation_number,
    render_formula_text,
    validate_manifest,
    _validate_table_structure,
)


def _fake_artifacts(tmp_path: Path) -> tuple[dict, Path]:
    artifacts = tmp_path / "artifacts"
    scratch = tmp_path / "scratch"
    manifest = FakeConverter().convert(tmp_path / "source.pdf", artifacts, scratch)
    return manifest, artifacts


def _valid_table() -> dict:
    return {
        "num_rows": 2,
        "num_cols": 2,
        "orientation": "rotated_270",
        "headers": ["指标", "数值"],
        "header_rows": [["指标", "数值"]],
        "rows": [["指标", "数值"], ["准确率", "0.93"]],
        "cells": [{"text": "指标", "row_span": 1, "col_span": 1}],
        "footnotes": ["单位：百分比"],
    }


def test_fake_converter_writes_a_real_png(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)

    assert manifest["version"] == SERVICE_PARSER_VERSION
    image = artifacts / "pictures" / "picture-0.png"
    assert image.is_file()
    assert image.read_bytes().startswith(b"\x89PNG\r\n\x1a\n")
    # A JSON round-trip must work: the Go backend decodes this exact payload.
    json.loads(json.dumps(manifest))


def test_fake_manifest_passes_contract_validation(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)

    validate_manifest(manifest, artifacts)


def test_table_manifest_passes_contract_validation(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    picture = manifest["assets"][0]
    picture.update(
        kind="table",
        image_file=None,
        table=_valid_table(),
        caption="",
        caption_source="none",
        generated_description="",
    )
    manifest["blocks"][2]["kind"] = "table"

    validate_manifest(manifest, artifacts)


def test_contract_rejects_missing_keys(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    del manifest["blocks"][0]["latex"]

    with pytest.raises(ParserConversionError, match="missing required keys"):
        validate_manifest(manifest, artifacts)


def test_contract_rejects_unexpected_keys(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    manifest["assets"][0]["_vision_file"] = "pictures/picture-0.png"

    with pytest.raises(ParserConversionError, match="unexpected keys"):
        validate_manifest(manifest, artifacts)


def test_contract_rejects_wrong_version(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    manifest["version"] = "docling-formula-remote-v3"

    with pytest.raises(ParserConversionError, match="manifest version"):
        validate_manifest(manifest, artifacts)


def test_contract_rejects_empty_formula_latex(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    formula = manifest["blocks"][1]
    formula["latex"] = ""

    with pytest.raises(ParserConversionError, match="empty latex"):
        validate_manifest(manifest, artifacts)


def test_contract_rejects_formula_text_without_latex(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    manifest["blocks"][1]["text"] = "$$ $$"

    with pytest.raises(ParserConversionError, match="does not contain its latex"):
        validate_manifest(manifest, artifacts)


def test_contract_rejects_unsafe_image_path(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    manifest["assets"][0]["image_file"] = "../escape.png"

    with pytest.raises(ParserConversionError, match="safe relative path"):
        validate_manifest(manifest, artifacts)


def test_contract_rejects_missing_image_file(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    (artifacts / "pictures" / "picture-0.png").unlink()

    with pytest.raises(ParserConversionError, match="image is missing"):
        validate_manifest(manifest, artifacts)


def test_contract_rejects_table_with_image(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    manifest["assets"][0].update(kind="table", image_file="pictures/picture-0.png")
    manifest["blocks"][2]["kind"] = "table"

    with pytest.raises(ParserConversionError, match="image_file must be null"):
        validate_manifest(manifest, artifacts)


def test_contract_rejects_block_referencing_unknown_asset(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    manifest["blocks"][2]["asset_id"] = "#/pictures/9"

    with pytest.raises(ParserConversionError, match="unknown asset"):
        validate_manifest(manifest, artifacts)


def test_contract_rejects_empty_text_block(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    manifest["blocks"][0]["text"] = "   "

    with pytest.raises(ParserConversionError, match="empty text block"):
        validate_manifest(manifest, artifacts)


def test_contract_rejects_non_sequential_order(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    manifest["blocks"][1]["order"] = 7

    with pytest.raises(ParserConversionError, match="not sequential"):
        validate_manifest(manifest, artifacts)


def test_contract_accepts_low_confidence_ocr_warning(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    manifest["warnings"] = [
        {
            "page": 2,
            "kind": "low_confidence_ocr",
            "count": 3,
            "message": "Low-confidence OCR text was retained, not discarded; verify against the PDF.",
        }
    ]

    validate_manifest(manifest, artifacts)


def test_contract_rejects_malformed_warning(tmp_path: Path) -> None:
    manifest, artifacts = _fake_artifacts(tmp_path)
    manifest["warnings"] = [{"page": 1}]

    with pytest.raises(ParserConversionError, match="warning is missing required keys"):
        validate_manifest(manifest, artifacts)


def test_formula_text_without_equation_number() -> None:
    assert render_formula_text("E = mc^2", None) == "$$\nE = mc^2\n$$"


def test_formula_text_appends_equation_number() -> None:
    assert render_formula_text("E = mc^2", "5") == "$$\nE = mc^2\n$$\n\n(5)"


def test_formula_text_does_not_duplicate_number_already_in_latex() -> None:
    assert render_formula_text("E = mc^2 \\quad (5)", "5") == "$$\nE = mc^2 \\quad (5)\n$$"


def test_formula_text_rejects_empty_latex() -> None:
    with pytest.raises(ParserConversionError):
        render_formula_text("   ", None)


@pytest.mark.parametrize(
    ("source", "expected"),
    [
        ("a = b (12)", "12"),
        ("a = b (12),", "12"),
        ("a = b (3a)", "3a"),
        ("a = b (4.1)", "4.1"),
        ("a = b (7); ", "7"),
        ("a = b", None),
        ("", None),
        ("(not a number)", None),
    ],
)
def test_parse_equation_number(source: str, expected: str | None) -> None:
    assert parse_equation_number(source) == expected


@pytest.mark.parametrize(
    ("raw", "expected"),
    [
        ("A chart of sales.", "A chart of sales."),
        ("A chart of sales", "A chart of sales."),
        ("A chart<end_of_utterance>", "A chart."),
        ("图表", "图表."),  # parity with the old worker: a plain ASCII period is appended
        ("", ""),
    ],
)
def test_clean_description(raw: str, expected: str) -> None:
    assert clean_description(raw) == expected


def test_validate_table_structure_accepts_normal_table() -> None:
    _validate_table_structure(_valid_table(), "#/tables/0")


def test_validate_table_structure_rejects_unreliable_wide_table() -> None:
    wide = _valid_table()
    wide.update(num_rows=2, num_cols=10)
    wide["rows"] = [["" for _ in range(10)] for _ in range(2)]
    wide["headers"] = ["" for _ in range(10)]
    wide["header_rows"] = []

    with pytest.raises(ParserConversionError, match="unreliable"):
        _validate_table_structure(wide, "#/tables/7")


def test_build_converter_selects_modes() -> None:
    assert build_converter("fake", models_path=None, vlm=None).name == "fake"
    assert build_converter("docling", models_path=None, vlm=None).name == "docling"
    with pytest.raises(ParserConfigError):
        build_converter("onnx", models_path=None, vlm=None)


def test_fake_manifest_is_independent_between_calls() -> None:
    first = fake_manifest()
    second = fake_manifest()
    first["blocks"][0]["text"] = "mutated"
    assert second["blocks"][0]["text"] != "mutated"
    assert copy.deepcopy(first)["version"] == SERVICE_PARSER_VERSION
