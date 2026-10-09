"""URL navigation against an isolated HTTP service with real document storage."""

import io
import threading
from pathlib import Path
from urllib.parse import quote

import pytest
from playwright.sync_api import expect, sync_playwright
from rag_mcp.chunk_strategies import ChunkingConfig

from test_mcp_http import ProtocolRuntime, running_service


DOCUMENT = "维护 #100% & A.txt"
LABELS = {"search": "检索调试", "documents": "知识文档", "connection": "接入配置"}


class NavigationRuntime(ProtocolRuntime):
    def __init__(self, root):
        super().__init__(root)
        self.pause_check = False
        self.check_started = threading.Event()
        self.check_release = threading.Event()

    def check_documents(self, namespace, items):
        if self.pause_check:
            self.check_started.set()
            if not self.check_release.wait(20):
                raise RuntimeError("Upload test timed out")
        return super().check_documents(namespace, items)


@pytest.fixture()
def navigation_browser(tmp_path):
    runtime = NavigationRuntime(tmp_path / "service")
    for namespace in ("alpha", "beta"):
        runtime.create_knowledge_base(namespace, ChunkingConfig("character", 700, 100), runtime.config.embedding_model)
        result = runtime.upload(namespace, [(DOCUMENT, io.BytesIO(f"{namespace} 的文档内容".encode()))])
        assert len(result["imported"]) == 1, result
    with running_service(runtime.data_root, runtime=runtime) as (url, _app, _runtime):
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch(channel="msedge", headless=True)
            context = browser.new_context(viewport={"width": 1440, "height": 1000})
            context.add_init_script("""
                if (location.protocol === 'http:') {
                if (!localStorage.getItem('rag.namespace')) localStorage.setItem('rag.namespace', 'alpha');
                if (!sessionStorage.getItem('rag.token')) sessionStorage.setItem('rag.token', 'test-token');
                window.initialHistoryLength = history.length;
                }
            """)
            page = context.new_page()
            errors = []
            context.on("page", lambda tab: tab.on("pageerror", lambda error: errors.append(str(error))))
            page.on("pageerror", lambda error: errors.append(str(error)))
            try:
                yield page, context, url, runtime
            finally:
                runtime.check_release.set()
                browser.close()
            assert not errors, errors


def address(url, namespace="alpha", view="documents", document=None):
    value = f"{url}/console#/workspaces/{namespace}/{view}"
    return value if document is None else value + "/" + quote(document, safe="")


def navigate(page, view):
    page.get_by_role("navigation", name="主导航").get_by_role("button", name=LABELS[view], exact=True).click()


def switch_workspace(page, namespace):
    page.get_by_role("button", name="切换工作区", exact=True).click()
    page.get_by_role("menuitem", name="其他工作区…", exact=True).click()
    dialog = page.get_by_role("dialog", name="切换工作区", exact=True)
    dialog.get_by_label("工作区名称", exact=True).fill(namespace)
    dialog.get_by_role("button", name="切换", exact=True).click()
    expect(dialog).not_to_be_visible()


def test_refresh_restores_each_page_without_duplicate_history(navigation_browser):
    page, _context, url, _runtime = navigation_browser
    page.goto(url + "/console")
    expect(page).to_have_url(address(url, view="search"))
    assert page.evaluate("history.length === window.initialHistoryLength")
    for view, label in LABELS.items():
        navigate(page, view)
        expect(page).to_have_url(address(url, view=view))
        length = page.evaluate("history.length")
        navigate(page, view)
        assert page.evaluate("history.length") == length
        if view == "search":
            page.get_by_label("查询内容", exact=True).fill("不保存的输入")
        page.reload()
        expect(page).to_have_url(address(url, view=view))
        expect(page.get_by_role("heading", name=label, exact=True)).to_be_visible()
        if view == "search":
            expect(page.get_by_label("查询内容", exact=True)).to_have_value("")


def test_back_forward_documents_and_workspace_switch(navigation_browser):
    page, _context, url, _runtime = navigation_browser
    page.goto(address(url))
    page.evaluate("window.navigationMarker = 'not-reloaded'")
    page.get_by_role("button", name=DOCUMENT, exact=True).click()
    expect(page).to_have_url(address(url, document=DOCUMENT))
    expect(page.locator(".document-text")).to_have_text("alpha 的文档内容")
    page.get_by_role("navigation", name="当前位置").get_by_role("button", name="知识文档", exact=True).click()
    expect(page).to_have_url(address(url))
    page.go_back()
    expect(page.locator(".document-text")).to_have_text("alpha 的文档内容")
    page.go_back()
    expect(page.get_by_role("heading", name="知识文档", exact=True)).to_be_visible()
    page.go_forward()
    expect(page).to_have_url(address(url, document=DOCUMENT))
    switch_workspace(page, "beta")
    expect(page).to_have_url(address(url, "beta"))
    expect(page.get_by_role("region", name="文档详情页面")).to_have_count(0)
    page.get_by_role("button", name=DOCUMENT, exact=True).click()
    expect(page.locator(".document-text")).to_have_text("beta 的文档内容")
    page.go_back()
    page.go_back()
    expect(page).to_have_url(address(url, document=DOCUMENT))
    expect(page.locator(".document-text")).to_have_text("alpha 的文档内容")
    expect(page.locator(".workspace-name")).to_have_text("alpha")
    assert page.evaluate("window.navigationMarker") == "not-reloaded"
    navigate(page, "connection")
    expect(page.get_by_label("Namespace", exact=True)).to_have_value("alpha")
    page.get_by_role("button", name="切换工作区", exact=True).click()
    expect(page.get_by_role("menuitem", name="beta", exact=True)).to_be_visible()


def test_direct_link_overrides_storage_and_supports_new_tabs(navigation_browser):
    page, context, url, _runtime = navigation_browser
    page.goto(address(url))
    linked = context.new_page()
    requests = []
    linked.on("request", lambda request: requests.append(request) if "/v1/" in request.url else None)
    linked.goto(address(url, "beta", document=DOCUMENT))
    expect(linked.locator(".document-text")).to_have_text("beta 的文档内容")
    expect(linked.locator(".workspace-name")).to_have_text("beta")
    assert requests and all(request.headers.get("x-rag-namespace") == "beta" for request in requests)
    assert "test-token" not in linked.url
    assert linked.evaluate("localStorage.getItem('rag.namespace')") == "beta"
    expect(page.locator(".workspace-name")).to_have_text("alpha")
    page.reload()
    expect(page.locator(".workspace-name")).to_have_text("alpha")
    linked.reload()
    expect(linked.locator(".document-text")).to_have_text("beta 的文档内容")
    expect(linked.get_by_role("navigation", name="当前位置").locator('[aria-current="page"]')).to_have_text(DOCUMENT)


def test_invalid_addresses_replace_history_and_fall_back(navigation_browser):
    page, _context, url, _runtime = navigation_browser
    page.goto(address(url, document=DOCUMENT))
    expect(page.locator(".document-text")).to_have_text("alpha 的文档内容")
    for value in (
        "#/unknown", "#/workspaces/%ZZ/documents", "#/workspaces/alpha/unknown",
        "#/workspaces/alpha/documents/%E0%A4%A", "#/workspaces/alpha/documents/%2Fprivate.txt",
        "#/workspaces/alpha/search/extra", "#/workspaces/alpha/documents/", "",
    ):
        length = page.evaluate("history.length")
        page.evaluate("value => { location.hash = value; }", value)
        expect(page).to_have_url(address(url, view="search"))
        expect(page.get_by_role("heading", name="检索调试", exact=True)).to_be_visible()
        assert page.evaluate("history.length") == length + 1
    page.evaluate("localStorage.setItem('rag.namespace', '../invalid')")
    page.goto(url + "/console")
    expect(page).to_have_url(address(url, "default", "search"))


def test_missing_document_and_auth_errors_keep_document_address(navigation_browser):
    page, _context, url, _runtime = navigation_browser
    missing = address(url, document="已经删除.txt")
    page.goto(missing)
    expect(page.get_by_text("文档内容加载失败", exact=True)).to_be_visible()
    page.reload()
    expect(page.get_by_text("文档内容加载失败", exact=True)).to_be_visible()
    expect(page).to_have_url(missing)
    page.get_by_role("button", name="返回知识文档", exact=True).click()
    expect(page).to_have_url(address(url))
    page.get_by_role("button", name=DOCUMENT, exact=True).click()
    page.evaluate("sessionStorage.setItem('rag.token', 'wrong-test-credential')")
    page.reload()
    expect(page.get_by_text("文档内容加载失败", exact=True)).to_be_visible()
    expect(page).to_have_url(address(url, document=DOCUMENT))
    page.evaluate("sessionStorage.setItem('rag.token', 'test-token')")
    page.reload()
    expect(page.locator(".document-text")).to_have_text("alpha 的文档内容")


def test_connection_workspace_updates_url_and_history(navigation_browser):
    page, _context, url, _runtime = navigation_browser
    page.goto(address(url, view="connection"))
    switch_workspace(page, "beta")
    expect(page).to_have_url(address(url, "beta", "connection"))
    page.go_back()
    expect(page.locator(".workspace-name")).to_have_text("alpha")
    expect(page.get_by_test_id("mcp-config")).to_contain_text('"X-RAG-Namespace": "alpha"')
    page.go_forward()
    expect(page.locator(".workspace-name")).to_have_text("beta")
    expect(page.get_by_role("heading", name="接入配置", exact=True)).to_be_visible()


@pytest.mark.parametrize("action", ["hash", "back", "same-workspace"])
def test_upload_survives_url_navigation(navigation_browser, action):
    page, _context, url, runtime = navigation_browser
    page.goto(address(url, "beta"))
    switch_workspace(page, "alpha")
    runtime.pause_check = True
    page.get_by_role("button", name="上传文档", exact=True).click()
    dialog = page.get_by_role("dialog", name="上传知识文档")
    expect(dialog.get_by_role("combobox")).to_have_count(0)
    dialog.locator('input[type="file"]').set_input_files({
        "name": "upload-navigation.txt", "mimeType": "text/plain", "buffer": b"Real upload during navigation",
    })
    dialog.get_by_role("button", name="上传并索引", exact=True).click()
    assert runtime.check_started.wait(5)
    requests = []
    page.on("request", lambda request: requests.append(request) if "/v1/" in request.url else None)
    if action == "back":
        page.go_back()
    else:
        target = "#/workspaces/alpha/connection" if action == "same-workspace" else "#/workspaces/beta/documents"
        page.evaluate("value => { location.hash = value; }", target)
    if action != "same-workspace":
        expect(page.get_by_text("文档正在上传，请完成后再切换工作区", exact=True)).to_be_visible()
    expect(page).to_have_url(address(url, view="connection" if action == "same-workspace" else "documents"))
    expect(page.locator(".workspace-name")).to_have_text("alpha")
    expect(dialog).to_be_visible()
    expect(dialog.get_by_role("button", name="上传并索引", exact=True)).to_be_disabled()
    runtime.check_release.set()
    expect(dialog.get_by_text("新增 1 个，跳过 0 个，失败 0 个", exact=True)).to_be_visible(timeout=30000)
    assert all(request.headers.get("x-rag-namespace") == "alpha" for request in requests)
    assert runtime.read_document("alpha", "upload-navigation.txt")[0] == "Real upload during navigation"
    assert not any(item["name"] == "upload-navigation.txt" for item in runtime.list_documents("beta"))
    dialog.get_by_role("button", name="关闭", exact=True).last.click()
    page.evaluate("location.hash = '#/workspaces/beta/documents'")
    expect(page).to_have_url(address(url, "beta"))
    expect(page.locator(".workspace-name")).to_have_text("beta")


def test_mobile_navigation_layout(navigation_browser):
    page, _context, url, _runtime = navigation_browser
    page.goto(address(url, document=DOCUMENT))
    expect(page.locator(".document-text")).to_have_text("alpha 的文档内容")
    output = Path(".test-tmp/console-browser")
    output.mkdir(parents=True, exist_ok=True)
    for width in (1440, 390, 320):
        page.set_viewport_size({"width": width, "height": 900})
        page.screenshot(path=str(output / f"url-navigation-{width}.png"), full_page=True)
        assert page.evaluate("document.documentElement.scrollWidth <= innerWidth")
