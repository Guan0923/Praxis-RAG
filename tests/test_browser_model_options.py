"""Model options must not be hidden by a previously saved model name."""

from pathlib import Path

from playwright.sync_api import expect

from test_browser_navigation import navigation_browser, address  # noqa: F401


def test_fetched_models_visible_before_typing(navigation_browser):
    page, _context, url, _runtime = navigation_browser
    models = ["local-embedding-v2", "local-reranker-v2"]

    def respond(route):
        if route.request.method == "POST":
            route.fulfill(json={"models": models})
        else:
            route.fulfill(json={"writable": True, "models": {
                kind: {"base_url": "http://127.0.0.1:11434/v1", "model": "old-unrelated-model",
                       "api_key_set": False, "configured": True, "vector_size": None}
                for kind in ("embedding", "reranker")
            }})

    page.route("**/v1/model-settings**", respond)
    page.goto(url + "/console#/models")
    output = Path(".test-tmp/model-options")
    output.mkdir(parents=True, exist_ok=True)
    for title, width in [("Embedding", 1440), ("Reranker", 390)]:
        page.set_viewport_size({"width": width, "height": 1000})
        section = page.get_by_role("region", name=title + " 模型配置", exact=True)
        section.get_by_role("button", name="获取模型列表", exact=True).click()
        expect(section.get_by_role("button", name="获取模型列表", exact=True)).to_be_enabled()
        field = section.get_by_label("model", exact=True)
        def option(name):
            return page.get_by_role("option", name=name, exact=True).and_(
                page.locator(f'[id^="model-{title.lower()}_"]'))
        expect(field).to_have_value("old-unrelated-model")
        field.click()
        expect(option(models[0])).to_be_visible()
        expect(option(models[1])).to_be_visible()
        page.screenshot(path=str(output / f"{title}-{width}.png"), full_page=True, animations="disabled")
        page.wait_for_function("document.documentElement.scrollWidth <= innerWidth")
        field.fill("reranker")
        expect(option(models[0])).to_have_count(0)
        option(models[1]).click()
        expect(field).to_have_value(models[1])
        field.blur()
        field.click()
        expect(option(models[0])).to_be_visible()
        field.fill("custom-manual-model")
        expect(field).to_have_value("custom-manual-model")
        field.press("Escape")
        field.blur()
