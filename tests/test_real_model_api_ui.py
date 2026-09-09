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
            page.get_by_role("navigation").get_by_role("button", name=name, exact=True).click()

        try:
            page.goto(url + "/console")
            navigate("连接配置")
            for kind, model in [("Embedding", "served-embedding"), ("Reranker", "served-reranker")]:
                section = page.get_by_role("region", name=kind + " 模型配置", exact=True)
                section.get_by_label("base_url", exact=True).fill(provider + "/")
                section.get_by_role("button", name="获取模型列表", exact=True).click()
                expect(section.get_by_role("button", name="获取模型列表", exact=True)).to_be_enabled(timeout=10_000)
                section.get_by_label("model", exact=True).fill("served")
                page.get_by_role("option", name=model, exact=True).click()
                section.get_by_role("button", name="测试连接", exact=True).click()
                expect(section.get_by_text("连接成功", exact=True)).to_be_visible(timeout=180_000)
                section.get_by_role("button", name="保存并应用", exact=True).click()
                expect(section.get_by_role("button", name="保存并应用", exact=True)).to_be_enabled(timeout=180_000)
                expect(section.get_by_label("base_url", exact=True)).to_have_value(provider)
                expect(section.get_by_label("apikey", exact=True)).to_have_value("")

            page.reload()
            navigate("连接配置")
            expect(page.get_by_role("region", name="Embedding 模型配置").get_by_label("model", exact=True)).to_have_value("served-embedding")
            expect(page.get_by_text("device:", exact=False)).to_have_count(0)
            for theme in ("light", "dark"):
                if theme == "dark":
                    page.get_by_role("button", name="切换深色主题", exact=True).click()
                for width in (1440, 390, 320):
                    page.set_viewport_size({"width": width, "height": 1000})
                    page.screenshot(path=str(output / f"model-settings-{theme}-{width}.png"), full_page=True, animations="disabled")
                    assert page.evaluate("document.documentElement.scrollWidth <= innerWidth")
            page.set_viewport_size({"width": 1440, "height": 1050})
            navigate("知识文档")
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
            page.get_by_role("button", name="运行检索", exact=True).click()
            expect(page.locator(".search-result").first).to_be_visible(timeout=180_000)
            expect(page.locator(".result-text").first).to_contain_text("E03")
            expect(page.locator(".scores").first).to_contain_text("召回")
            page.screenshot(path=str(output / "real-embedding-rerank.png"), full_page=True, animations="disabled")
            navigate("连接配置")
            embedding = page.get_by_role("region", name="Embedding 模型配置")
            embedding.get_by_label("model", exact=True).fill("BAAI/bge-m3")
            embedding.get_by_role("button", name="保存并应用", exact=True).click()
            expect(embedding.get_by_text("已有知识文档", exact=False)).to_be_visible(timeout=180_000)
            saved = context.request.get(url + "/v1/model-settings").json()
            assert saved["models"]["embedding"]["model"] == "served-embedding"

            # A missing list endpoint never prevents manual model entry.
            reranker = page.get_by_role("region", name="Reranker 模型配置")
            reranker.get_by_label("base_url", exact=True).fill(provider + "/missing")
            reranker.get_by_role("button", name="获取模型列表", exact=True).click()
            expect(reranker.get_by_text("HTTP 404", exact=False)).to_be_visible()
            reranker.get_by_label("base_url", exact=True).fill(provider)
            reranker.get_by_label("model", exact=True).fill("served-reranker")
            reranker.get_by_role("button", name="保存并应用", exact=True).click()
            expect(reranker.get_by_text("连接成功", exact=True)).to_be_visible(timeout=180_000)
            reranker.get_by_label("apikey", exact=True).fill("ui-test-only-key")
            reranker.get_by_role("button", name="保存并应用", exact=True).click()
            expect(reranker.get_by_label("apikey", exact=True)).to_have_value("", timeout=180_000)
            expect(reranker.get_by_label("apikey", exact=True)).to_have_attribute("placeholder", "已保存")
            assert "ui-test-only-key" not in page.evaluate("JSON.stringify({...localStorage,...sessionStorage})")
            with page.expect_request(lambda request: request.method == "PATCH" and request.url.endswith("/reranker")) as request:
                reranker.get_by_role("button", name="保存并应用", exact=True).click()
            assert "apikey" not in request.value.post_data_json
            expect(reranker.get_by_role("button", name="保存并应用", exact=True)).to_be_enabled(timeout=180_000)
            reranker.get_by_role("button", name="清除 Reranker 已保存的 apikey", exact=True).click()
            reranker.get_by_role("button", name="保存并应用", exact=True).click()
            expect(reranker.get_by_role("button", name="清除 Reranker 已保存的 apikey", exact=True)).to_have_count(0, timeout=180_000)

            readonly_context = browser.new_context(extra_http_headers={"X-Forwarded-For": "192.0.2.1"})
            readonly = readonly_context.new_page()
            readonly.goto(url + "/console")
            readonly.get_by_role("navigation").get_by_role("button", name="连接配置", exact=True).click()
            expect(readonly.get_by_text("模型设置只读", exact=True)).to_be_visible()
            for button in readonly.get_by_role("button", name="保存并应用", exact=True).all():
                expect(button).to_be_disabled()
            readonly_context.close()
            assert not errors, errors
        finally:
            listing = context.request.get(url + "/v1/documents", headers={"X-RAG-Namespace": namespace})
            if listing.ok:
                for item in listing.json()["documents"]:
                    context.request.delete(url + "/v1/documents/" + quote(item["name"], safe=""), headers={"X-RAG-Namespace": namespace})
            browser.close()
