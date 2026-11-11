"""``/v1/parse`` zip/manifest contract using the fake converter."""

from __future__ import annotations

import io
import json
import zipfile

from fastapi.testclient import TestClient

from conftest import parse_files
from app.parser import (
    FAKE_EQUATION_NUMBER,
    FAKE_LATEX,
    SERVICE_PARSER_VERSION,
)

BLOCK_KEYS = {
    "ref",
    "kind",
    "page",
    "order",
    "text",
    "asset_id",
    "bbox",
    "locations",
    "latex",
    "original_text",
    "equation_number",
    "association",
    "associated_assets",
}
ASSET_KEYS = {
    "asset_id",
    "kind",
    "page",
    "bbox",
    "caption",
    "caption_source",
    "generated_description",
    "image_file",
    "table",
    "captions",
    "footnotes",
}


def _parse_zip(client: TestClient, content: bytes) -> zipfile.ZipFile:
    response = client.post("/v1/parse", files=parse_files(content))
    assert response.status_code == 200, response.text
    assert response.headers["content-type"] == "application/zip"
    assert response.headers["x-pdf-parser-version"] == SERVICE_PARSER_VERSION
    return zipfile.ZipFile(io.BytesIO(response.content))


def test_parse_returns_zip_with_manifest_and_picture(fake_client: TestClient, sample_pdf: bytes) -> None:
    archive = _parse_zip(fake_client, sample_pdf)

    assert sorted(archive.namelist()) == ["manifest.json", "pictures/picture-0.png"]

    manifest = json.loads(archive.read("manifest.json"))
    assert manifest["version"] == SERVICE_PARSER_VERSION
    assert set(manifest) == {"version", "blocks", "assets", "warnings", "parser", "metrics"}
    assert isinstance(manifest["warnings"], list)
    assert isinstance(manifest["metrics"], dict)
    assert manifest["metrics"]["pages"] == 1
    assert manifest["parser"]["version"] == SERVICE_PARSER_VERSION
    assert manifest["parser"]["converter"] == "fake"

    kinds = [block["kind"] for block in manifest["blocks"]]
    assert kinds == ["text", "formula", "picture"]
    assert [block["order"] for block in manifest["blocks"]] == [0, 1, 2]

    text_block = manifest["blocks"][0]
    assert text_block["text"].startswith("Praxis-RAG PDF service fake mode")
    assert text_block["asset_id"] == ""
    assert text_block["latex"] == ""
    assert text_block["locations"][0]["page"] == 1
    assert set(text_block) == BLOCK_KEYS

    formula_block = manifest["blocks"][1]
    assert formula_block["kind"] == "formula"
    assert formula_block["latex"] == FAKE_LATEX
    assert formula_block["text"].startswith("$$\n")
    assert FAKE_LATEX in formula_block["text"]
    assert formula_block["equation_number"] == FAKE_EQUATION_NUMBER
    assert formula_block["text"].rstrip().endswith("(" + FAKE_EQUATION_NUMBER + ")")
    assert formula_block["original_text"] == "G = X_basis^T X_basis (1)"

    picture_block = manifest["blocks"][2]
    assert picture_block["kind"] == "picture"
    assert picture_block["asset_id"] == "#/pictures/0"
    assert picture_block["text"] == ""

    assert len(manifest["assets"]) == 1
    asset = manifest["assets"][0]
    assert set(asset) == ASSET_KEYS
    assert asset["asset_id"] == "#/pictures/0"
    assert asset["kind"] == "picture"
    assert asset["image_file"] == "pictures/picture-0.png"
    assert asset["table"] is None
    assert asset["caption"]
    assert asset["caption_source"] == "document"
    assert isinstance(asset["captions"], list) and asset["captions"]
    assert isinstance(asset["footnotes"], list)

    png = archive.read(asset["image_file"])
    assert png.startswith(b"\x89PNG\r\n\x1a\n")
    assert len(png) > 50


def test_every_asset_image_is_present_in_the_zip(fake_client: TestClient, sample_pdf: bytes) -> None:
    archive = _parse_zip(fake_client, sample_pdf)
    manifest = json.loads(archive.read("manifest.json"))
    names = set(archive.namelist())

    referenced = {
        asset["image_file"]
        for asset in manifest["assets"]
        if asset["image_file"]
    }
    assert referenced
    assert referenced <= names


def test_manifest_table_asset_shape(fake_client: TestClient, sample_pdf: bytes) -> None:
    """Table assets (not produced by fake mode) must keep a null image_file."""
    manifest = json.loads(_parse_zip(fake_client, sample_pdf).read("manifest.json"))

    for asset in manifest["assets"]:
        if asset["kind"] == "table":
            assert asset["image_file"] is None
            assert asset["table"] is not None
