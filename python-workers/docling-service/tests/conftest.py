"""Shared fixtures for the Docling service tests.

The tests run the real Docling converter against generated DOCX/HTML samples:
that is the point of this service (the demo samples validate table spans,
captions and image extraction), so no Docling double is used here.
"""

from __future__ import annotations

import base64
import io
import sys
from pathlib import Path

import pytest

SERVICE_ROOT = Path(__file__).resolve().parents[1]
if str(SERVICE_ROOT) not in sys.path:
    sys.path.insert(0, str(SERVICE_ROOT))

from app import main as main_module  # noqa: E402
from app.main import Settings, create_app  # noqa: E402


def _png_bytes(color: tuple[int, int, int]) -> bytes:
    from PIL import Image, ImageDraw

    image = Image.new("RGB", (64, 48), color)
    draw = ImageDraw.Draw(image)
    draw.rectangle((2, 2, 60, 44), outline="white", width=2)
    buffer = io.BytesIO()
    image.save(buffer, format="PNG")
    return buffer.getvalue()


@pytest.fixture(scope="session")
def app_client():
    from fastapi.testclient import TestClient

    app = create_app(
        Settings(concurrency=2, parse_timeout=60.0, timeout=120.0, max_upload_bytes=5 * 1024 * 1024)
    )
    with TestClient(app) as client:
        yield client


@pytest.fixture()
def docx_bytes(tmp_path: Path) -> bytes:
    from docx import Document
    from docx.shared import Inches

    image_path = tmp_path / "figure.png"
    image_path.write_bytes(_png_bytes((30, 90, 160)))
    document = Document()
    document.add_heading("X7 系统手册", level=1)
    document.add_paragraph("本文档描述 X7 的安装、配置与故障处理流程。")
    document.add_heading("安装步骤", level=2)
    document.add_paragraph("1. 连接电源。")
    document.add_paragraph("2. 按下启动按钮。")
    document.add_heading("硬件参数", level=2)
    document.add_paragraph("表 1 硬件参数表")
    table = document.add_table(rows=3, cols=3)
    table.style = "Table Grid"
    for row, values in enumerate(
        (("部件", "规格", "数量"), ("处理器", "8 核 3.2GHz", "1"), ("内存", "32 GB", "2"))
    ):
        for column, value in enumerate(values):
            table.cell(row, column).text = value
    table.cell(1, 2).merge(table.cell(2, 2))
    document.add_heading("架构图", level=2)
    document.add_paragraph("图 1 系统架构图")
    document.add_picture(str(image_path), width=Inches(1.5))
    document.add_paragraph("图 1 展示了检索与生成之间的证据链路。")
    buffer = io.BytesIO()
    document.save(buffer)
    return buffer.getvalue()


@pytest.fixture()
def html_bytes(tmp_path: Path) -> bytes:
    png = base64.b64encode(_png_bytes((120, 60, 20))).decode()
    html = f"""<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>X7 网页手册</title></head>
<body>
<h1>X7 网页手册</h1>
<p>本页描述网页版的安装说明与硬件参数。</p>
<h2>硬件参数</h2>
<table border="1">
  <caption>表 1 硬件参数表</caption>
  <tr><th>部件</th><th colspan="2">规格</th></tr>
  <tr><td>处理器</td><td>8 核</td><td>3.2GHz</td></tr>
  <tr><td rowspan="2">内存</td><td>32 GB</td><td>2</td></tr>
  <tr><td>64 GB</td><td>1</td></tr>
</table>
<h2>架构图</h2>
<figure>
  <img src="data:image/png;base64,{png}" alt="架构图" />
  <figcaption>图 1 系统架构图</figcaption>
</figure>
<p>图 1 展示了证据链路。</p>
<h2>外部资源</h2>
<p>外部图片：<img src="https://example.invalid/remote.png" alt="远程图片" /></p>
</body></html>
"""
    return html.encode("utf-8")


__all__ = ["main_module"]
