"""Model selection belongs to knowledge-base creation, not global connections."""

from pathlib import Path

from playwright.sync_api import expect, sync_playwright

from test_model_api_settings import provider, runtime  # noqa: F401
from test_mcp_http import running_service


def test_models_are_selected_during_creation(runtime, provider):
    with running_service(runtime.data_root, runtime=runtime) as (url, _app, _runtime):
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch(channel="msedge", headless=True)
            page = browser.new_page(viewport={"width": 1440, "height": 1000})
            page.goto(url + "/console#/knowledge-bases")
            page.get_by_role("button", name="创建知识库", exact=True).first.click()
            form = page.locator(".knowledge-base-form")
            form.get_by_label("知识库标识（Namespace）", exact=True).fill("ui-models")
            form.get_by_label("Embedding URL", exact=True).fill(provider[0])
            form.get_by_role("switch", name="启用 Reranker").click()
            form.get_by_label("Reranker URL", exact=True).fill(provider[0])
            requests = []
            page.on("request", lambda request: requests.append(request.url))
            output = Path(".test-tmp/model-options")
            output.mkdir(parents=True, exist_ok=True)
            for label, model, width in [("Embedding 模型", "embed-a", 1440), ("Reranker 模型", "rank-a", 390)]:
                page.set_viewport_size({"width": width, "height": 1000})
                field = form.get_by_label(label, exact=True)
                field.click()
                expect(page.get_by_role("option", name=model, exact=True)).to_be_visible()
                page.screenshot(path=str(output / f"creation-{width}.png"), full_page=True)
                page.get_by_role("option", name=model, exact=True).click()
                field.click()
                expect(page.get_by_role("option", name=model, exact=True)).to_be_visible()
                field.press("Escape")
            expect(form.get_by_label("向量维度（vector len）", exact=True)).to_have_count(0)
            assert sum("/knowledge-bases/models/embedding" in path for path in requests) == 2
            assert sum("/knowledge-bases/models/reranker" in path for path in requests) == 2
            form.get_by_label("切分方式", exact=True).click()
            page.get_by_role("option", name="按字符", exact=True).click()
            form.get_by_role("button", name="创建知识库", exact=True).click()
            expect(page).to_have_url(url + "/console#/workspaces/documents")
            assert runtime.knowledge_base("ui-models")["embedding_model"] == "embed-a"
            browser.close()
