from __future__ import annotations

import io
import json
import zipfile


def opening_zip(response) -> zipfile.ZipFile:
    assert response.status_code == 200, response.text
    assert response.headers["content-type"].startswith("application/zip")
    return zipfile.ZipFile(io.BytesIO(response.content))


def manifest_of(archive: zipfile.ZipFile) -> dict:
    return json.loads(archive.read("manifest.json"))


def test_docx_manifest_keeps_table_spans_and_picture(app_client, docx_bytes):
    response = app_client.post(
        "/v1/parse", files={"file": ("manual.docx", docx_bytes)}
    )
    with opening_zip(response) as archive:
        manifest = manifest_of(archive)
        assert manifest["version"] == "docling-word-v1"
        assert manifest["parser"]["input_format"] == "docx"
        assert manifest["parser"]["contract"] == "docling-service-v1"
        assert manifest["parser"]["docling_version"] == "2.150.1"
        kinds = [block["kind"] for block in manifest["blocks"]]
        assert kinds.count("heading") >= 3
        assert "code" not in kinds  # the sample has no code paragraph
        assert "table" in kinds and "picture" in kinds
        table = next(asset for asset in manifest["assets"] if asset["kind"] == "table")
        cells = {(cell["row"], cell["col"]): cell for cell in table["table"]["cells"]}
        assert cells[(0, 0)]["text"] == "部件"
        # The merged cell keeps its row span and folds both values into the anchor.
        assert cells[(1, 2)]["row_span"] == 2
        assert cells[(1, 2)]["text"].strip() == "1\n2".strip()
        assert table["table"]["header_rows"][0][:2] == ["部件", "规格"]
        picture = next(asset for asset in manifest["assets"] if asset["kind"] == "picture")
        assert picture["image_file"].startswith("images/picture-1.")
        assert picture["mime_type"] == "image/png"
        assert archive.read(picture["image_file"]).startswith(b"\x89PNG")
        document = json.loads(archive.read("document.json"))
        assert document["schema_name"] == "DoclingDocument"


def test_html_manifest_keeps_spans_captions_and_missing_images(app_client, html_bytes):
    response = app_client.post("/v1/parse", files={"file": ("manual.html", html_bytes)})
    with opening_zip(response) as archive:
        manifest = manifest_of(archive)
        assert manifest["version"] == "docling-html-v1"
        assert manifest["parser"]["input_format"] == "html"
        table = next(asset for asset in manifest["assets"] if asset["kind"] == "table")
        cells = {(cell["row"], cell["col"]): cell for cell in table["table"]["cells"]}
        assert cells[(0, 1)]["col_span"] == 2
        assert cells[(2, 0)]["row_span"] == 2
        assert cells[(2, 0)]["text"] == "内存"
        assert table["caption"] == "表 1 硬件参数表"
        assert cells[(3, 1)]["text"] == "64 GB"
        # The <figcaption> is an explicit caption: the asset carries the text
        # and must not be indexed a second time as an ordinary paragraph. The
        # table <caption> is walked as an associated caption block.
        picture = next(asset for asset in manifest["assets"] if asset["kind"] == "picture")
        assert picture["caption"] == "图 1 系统架构图"
        assert picture["image_file"] is not None
        assert archive.read(picture["image_file"]).startswith(b"\x89PNG")
        caption_blocks = [
            block
            for block in manifest["blocks"]
            if block["kind"] == "caption" and block.get("associated_assets")
        ]
        assert any(block["associated_assets"] == ["table-1"] for block in caption_blocks)
        assert not any(
            block["text"] == "图 1 系统架构图" and not block.get("associated_assets")
            for block in manifest["blocks"]
        )
        external = next(
            asset for asset in manifest["assets"] if asset["asset_id"] == "picture-2"
        )
        assert external["image_file"] is None
        assert external["caption"] == "远程图片"
        assert any(
            diagnostic["kind"] == "external_picture"
            for diagnostic in manifest["diagnostics"]
        )


def test_manifest_block_order_is_reading_order(app_client, docx_bytes):
    response = app_client.post("/v1/parse", files={"file": ("manual.docx", docx_bytes)})
    with opening_zip(response) as archive:
        manifest = manifest_of(archive)
        assert [block["order"] for block in manifest["blocks"]] == list(
            range(len(manifest["blocks"]))
        )
        texts = [block["text"] for block in manifest["blocks"] if block["kind"] == "paragraph"]
        assert texts[0].startswith("本文档描述")
        assert "图 1 展示了检索与生成之间的证据链路。" in texts
