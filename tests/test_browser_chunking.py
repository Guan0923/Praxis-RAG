"""Opt-in browser acceptance against a real running RAG service."""

import os
from pathlib import Path
from uuid import uuid4

import pytest


@pytest.mark.skipif(not os.getenv("RAG_BROWSER_URL"), reason="Set RAG_BROWSER_URL to a real test service")
def test_browser_upload_preflight_and_layout():
    playwright = pytest.importorskip("playwright.sync_api")
    output = Path(".mini_agent-rag/chunking-ui")
    output.mkdir(parents=True, exist_ok=True)
    namespace = "browser-" + uuid4().hex[:12]
    text = ("# 设备维护\n\n## E03\n\n温度传感器故障时，先断电，检查传感器连接，然后重新启动。\n\n" * 20).encode()
    with playwright.sync_playwright() as runtime:
        browser = runtime.chromium.launch(channel="msedge", headless=True)
        context = browser.new_context(viewport={"width": 1440, "height": 1000})
        context.add_init_script(f"localStorage.setItem('rag.namespace', '{namespace}');")
        page = context.new_page()
        requests = []
        errors = []
        page.on("request", lambda request: requests.append(request) if request.method == "POST" else None)
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.goto(os.environ["RAG_BROWSER_URL"] + "/console")
        page.get_by_role("button", name="知识文档", exact=True).click()
        playwright.expect(page.locator("#chunkStrategy")).to_be_enabled()
        playwright.expect(page.locator("#chunkStrategy")).to_have_value("recursive")

        def upload(name, expected):
            page.locator("#fileInput").set_input_files({"name": name, "mimeType": "text/markdown", "buffer": text})
            page.locator("#uploadButton").click()
            playwright.expect(page.locator("#uploadButton")).to_be_enabled(timeout=120_000)
            playwright.expect(page.locator("#uploadResult")).to_contain_text(expected)

        upload("maintenance.md", "新增 1 个，跳过 0 个，失败 0 个")
        def body_requests():
            return [request for request in requests if request.url.endswith("/v1/documents")]
        first_count = len(body_requests())
        assert first_count == 1
        def indexed_configuration():
            listing = page.request.get(os.environ["RAG_BROWSER_URL"] + "/v1/documents", headers={"X-RAG-Namespace": namespace})
            return listing.json()["documents"][0]["chunking"]
        assert indexed_configuration()["strategy"] == "recursive"
        upload("renamed.md", "新增 0 个，跳过 1 个，失败 0 个")
        assert len(body_requests()) == first_count
        playwright.expect(page.locator("#uploadResult")).to_contain_text("已存在 maintenance.md")
        page.locator("#chunkSize").fill("256")
        upload("maintenance.md", "新增 1 个，跳过 0 个，失败 0 个")
        assert len(body_requests()) == first_count + 1
        assert indexed_configuration()["chunk_size"] == 256
        for strategy in ("character", "token", "sentence", "structure"):
            page.locator("#chunkStrategy").select_option(strategy)
            playwright.expect(page.locator("#chunkSize")).to_have_value("700" if strategy == "character" else "512")
            playwright.expect(page.locator("#chunkOverlap")).to_have_value("100" if strategy == "character" else "64")
            upload("maintenance.md", "新增 1 个，跳过 0 个，失败 0 个")
            assert indexed_configuration()["strategy"] == strategy
        for width, height, name in ((1440, 1000, "desktop"), (390, 844, "mobile"), (320, 800, "narrow")):
            page.set_viewport_size({"width": width, "height": height})
            page.wait_for_function("getComputedStyle(document.querySelector('#toast')).opacity === '0'")
            page.screenshot(path=str(output / f"{name}.png"), full_page=True)
            assert page.evaluate("document.documentElement.scrollWidth <= window.innerWidth"), name
            boxes = [page.locator(selector).bounding_box() for selector in ("#fileInput", "#chunkStrategy", "#chunkSize", "#chunkOverlap", "#uploadButton")]
            for index, box in enumerate(boxes):
                assert box and box["width"] > 0 and box["x"] >= 0 and box["x"] + box["width"] <= width
                for other in boxes[index + 1:]:
                    assert box["x"] + box["width"] <= other["x"] + 1 or other["x"] + other["width"] <= box["x"] + 1 or box["y"] + box["height"] <= other["y"] + 1 or other["y"] + other["height"] <= box["y"] + 1
        assert not errors, errors
        print(f"BROWSER_ACCEPTANCE namespace={namespace} uploads={len(body_requests())} screenshots={output.resolve()}")
        browser.close()
