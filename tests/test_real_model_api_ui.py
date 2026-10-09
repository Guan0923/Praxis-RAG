"""Opt-in browser acceptance against an isolated RAG and real inference provider."""

import os
from pathlib import Path
from urllib.parse import quote
from uuid import uuid4

import pytest

pytestmark = pytest.mark.skipif(
    not os.getenv("RAG_MODEL_API_TEST_URL") or not os.getenv("RAG_MODEL_PROVIDER_URL"),
    reason="Set separate RAG_MODEL_API_TEST_URL and RAG_MODEL_PROVIDER_URL for real API acceptance",
)


def test_model_settings_real_browser_flow():
    from playwright.sync_api import expect, sync_playwright

    url = os.environ["RAG_MODEL_API_TEST_URL"].rstrip("/")
    provider = os.environ["RAG_MODEL_PROVIDER_URL"].rstrip("/")
    namespace = "api-ui-" + uuid4().hex[:12]
    output = Path(".test-tmp/model-api-browser")
    output.mkdir(parents=True, exist_ok=True)
    with sync_playwright() as driver:
        browser = driver.chromium.launch(channel="msedge", headless=True)
        context = browser.new_context(viewport={"width": 1440, "height": 1050})
        context.add_init_script(f"localStorage.setItem('rag.namespace', '{namespace}');")
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))

        def navigate(name):
            page.get_by_role("navigation", name="主导航").get_by_role("button", name=name, exact=True).click()

        try:
            page.goto(url + "/console")
            navigate("创建知识库")
            creation = page.locator(".knowledge-base-form")
            creation.get_by_label("知识库标识（Namespace）", exact=True).fill(namespace)
            creation.get_by_role("switch", name="启用 Reranker").click()
            for kind, model in [("Embedding", os.getenv("RAG_TEST_EMBEDDING_MODEL", "served-embedding")), ("Reranker", os.getenv("RAG_TEST_RERANKER_MODEL", "served-reranker"))]:
                creation.get_by_label(kind + " URL", exact=True).fill(provider)
                creation.get_by_label(kind + " 模型", exact=True).click()
                page.get_by_role("option", name=model, exact=True).click()
            for width in (1440, 390, 320):
                page.set_viewport_size({"width": width, "height": 1000})
                page.screenshot(path=str(output / f"creation-{width}.png"), full_page=True)
            page.set_viewport_size({"width": 1440, "height": 1050})
            creation.get_by_role("button", name="创建知识库", exact=True).click()
            expect(page.get_by_role("button", name="上传文档", exact=True)).to_be_visible(timeout=180_000)
            page.get_by_role("button", name="上传文档", exact=True).click()
            dialog = page.get_by_role("dialog", name="上传知识文档")
            text = ("# 星河设备维护\n\nE03 表示温度传感器故障。先关闭电源，检查传感器连接，再重新启动设备。\n" * 3).encode()
            dialog.locator('input[type="file"]').set_input_files({"name": "api-maintenance.md", "mimeType": "text/markdown", "buffer": text})
            dialog.get_by_role("button", name="上传并索引", exact=True).click()
            expect(dialog.get_by_text("新增 1 个，跳过 0 个，失败 0 个", exact=True)).to_be_visible(timeout=180_000)
            dialog.get_by_role("button", name="关闭", exact=True).last.click()
            navigate("检索调试")
            page.get_by_label("查询内容", exact=True).fill("E03 温度传感器故障怎么处理")
            page.get_by_label("检索方式", exact=True).click()
            page.get_by_role("option", name="Embedding", exact=True).click()
            page.get_by_label("重排方式", exact=True).click()
            page.get_by_role("option", name="Cross Encoder", exact=True).click()
            page.get_by_role("button", name="运行检索", exact=True).click()
            expect(page.locator(".search-result").first).to_be_visible(timeout=180_000)
            expect(page.locator(".result-text").first).to_contain_text("E03")
            expect(page.locator(".scores").first).to_contain_text("召回")
            page.screenshot(path=str(output / "real-embedding-rerank.png"), full_page=True, animations="disabled")
            navigate("知识库")
            page.get_by_role("button", name="配置 " + namespace, exact=True).click()
            editor = page.locator(".ant-drawer .knowledge-base-form")
            expect(editor.get_by_label("Embedding URL", exact=True)).to_have_value(provider)
            editor.get_by_role("button", name="保存配置", exact=True).click()
            expect(page.locator(".ant-drawer")).to_have_count(0, timeout=180_000)
            assert not errors, errors
        finally:
            listing = context.request.get(url + "/v1/documents", headers={"X-RAG-Namespace": namespace})
            if listing.ok:
                for item in listing.json()["documents"]:
                    context.request.delete(url + "/v1/documents/" + quote(item["name"], safe=""), headers={"X-RAG-Namespace": namespace})
            browser.close()
