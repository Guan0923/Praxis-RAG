"""Browser acceptance against a real, separately configured RAG service."""

import json
import os
from pathlib import Path
from uuid import uuid4

import pytest


pytestmark = pytest.mark.skipif(not os.getenv("RAG_BROWSER_URL"), reason="Set RAG_BROWSER_URL to an isolated real test service")


@pytest.fixture()
def console_browser():
    playwright = pytest.importorskip("playwright.sync_api")
    namespace = "ui-" + uuid4().hex[:12]
    url = os.environ["RAG_BROWSER_URL"].rstrip("/")
    output = Path(".test-tmp/console-browser")
    output.mkdir(parents=True, exist_ok=True)
    with playwright.sync_playwright() as runtime:
        browser = runtime.chromium.launch(channel="msedge", headless=True)
        context = browser.new_context(viewport={"width": 1440, "height": 1000})
        context.add_init_script(f"if (!localStorage.getItem('rag.namespace')) localStorage.setItem('rag.namespace', '{namespace}');")
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.goto(url + "/console")
        playwright.expect(page.get_by_role("heading", name="检索调试", exact=True)).to_be_visible()
        yield page, playwright.expect, namespace, url, output
        # Only this fixture's namespace is eligible for cleanup.
        listing = context.request.get(url + "/v1/documents", headers={"X-RAG-Namespace": namespace})
        if listing.ok:
            from urllib.parse import quote
            for document in listing.json()["documents"]:
                context.request.delete(url + "/v1/documents/" + quote(document["name"], safe=""), headers={"X-RAG-Namespace": namespace})
        browser.close()
        assert not errors, errors


def select_option(page, label, option):
    page.get_by_label(label, exact=True).click()
    page.get_by_role("option", name=option, exact=True).click()


def navigate(page, name):
    page.get_by_role("navigation", name="主导航").get_by_role("button", name=name, exact=True).click()


def test_browser_real_upload_search_and_themes(console_browser):
    page, expect, namespace, url, output = console_browser
    post_requests = []
    page.on("request", lambda request: post_requests.append(request) if request.method == "POST" else None)
    text = ("# 设备维护\n\n## E03\n\n星河 X7 出现 E03 表示温度传感器故障，请先断电，检查传感器连接，然后重新启动。\n\n" * 3).encode()
    navigate(page, "知识文档")
    page.get_by_role("button", name="上传文档", exact=True).click()
    dialog = page.get_by_role("dialog", name="上传知识文档")
    expect(dialog).to_have_css("opacity", "1")
    expect(dialog.get_by_label("切分策略", exact=True)).to_be_enabled()

    def upload(name, expected):
        dialog.locator('input[type="file"]').set_input_files({"name": name, "mimeType": "text/markdown", "buffer": text})
        dialog.get_by_role("button", name="上传并索引", exact=True).click()
        expect(dialog.get_by_text(expected, exact=True)).to_be_visible(timeout=180_000)
        expect(dialog.get_by_role("button", name="上传并索引", exact=True)).to_be_enabled()

    upload("maintenance.md", "新增 1 个，跳过 0 个，失败 0 个")
    body_requests = lambda: [request for request in post_requests if request.url.endswith("/v1/documents")]
    assert len(body_requests()) == 1
    upload("renamed.md", "新增 0 个，跳过 1 个，失败 0 个")
    assert len(body_requests()) == 1
    expect(dialog.get_by_text("renamed.md：已存在 maintenance.md", exact=True)).to_be_visible()
    dialog.get_by_label("块长度（token）", exact=True).fill("256")
    upload("maintenance.md", "新增 1 个，跳过 0 个，失败 0 个")
    for strategy, label, unit in (("character", "按字符", "字符"), ("token", "按 token", "token"), ("sentence", "按句子", "token"), ("structure", "按文档结构", "token")):
        select_option(page, "切分策略", label)
        expect(dialog.get_by_label(f"块长度（{unit}）", exact=True)).to_have_value("700" if strategy == "character" else "512")
        upload("maintenance.md", "新增 1 个，跳过 0 个，失败 0 个")
        listing = page.request.get(url + "/v1/documents", headers={"X-RAG-Namespace": namespace}).json()
        assert listing["documents"][0]["chunking"]["strategy"] == strategy
    dialog.get_by_role("button", name="关闭", exact=True).last.click()
    navigate(page, "检索调试")
    page.get_by_label("查询内容", exact=True).fill("星河 X7 出现 E03 应该怎么处理？")
    page.get_by_role("button", name="运行检索", exact=True).click()
    expect(page.locator(".search-result").first).to_be_visible(timeout=180_000)
    expect(page.locator(".metrics")).to_contain_text("Cross Encoder")
    expect(page.locator(".scores").first).to_contain_text("召回")
    expect(page.locator(".result-text").first).to_contain_text("传感器")
    page.get_by_role("button", name="三模式对比", exact=True).click()
    expect(page.locator(".comparison > section")).to_have_count(3, timeout=180_000)
    expect(page.locator(".metrics")).to_contain_text("3/3 成功")
    calls = [json.loads(request.post_data) for request in post_requests if request.url.endswith("/v1/search")]
    assert [call["retrieval"] for call in calls[-3:]] == ["bm25", "embedding", "hybrid"]
    assert all(call["rerank"] == "none" for call in calls[-3:])

    for appearance in ("light", "dark"):
        if appearance == "dark":
            page.get_by_role("button", name="切换深色主题").click()
            page.reload()
            expect(page.get_by_role("button", name="切换浅色主题")).to_be_visible()
            navigate(page, "检索调试")
            page.get_by_label("查询内容", exact=True).fill("星河 X7 E03 温度传感器故障")
            page.get_by_role("button", name="运行检索", exact=True).click()
            expect(page.locator(".search-result").first).to_be_visible(timeout=180_000)
        for width in (1440, 390, 320):
            page.set_viewport_size({"width": width, "height": 1000 if width == 1440 else 844})
            for name, slug in (("检索调试", "search"), ("知识文档", "documents"), ("接入配置", "connection")):
                navigate(page, name)
                page.screenshot(path=str(output / f"{appearance}-{width}-{slug}.png"), full_page=True, animations="disabled")
                assert page.evaluate("document.documentElement.scrollWidth <= innerWidth"), (appearance, width, name)
            navigate(page, "知识文档")
            page.get_by_role("button", name="上传文档", exact=True).click()
            expect(dialog).to_be_visible()
            expect(dialog).to_have_css("opacity", "1")
            page.screenshot(path=str(output / f"{appearance}-{width}-upload.png"), full_page=True, animations="disabled")
            box = dialog.bounding_box()
            assert box and box["x"] >= 0 and box["x"] + box["width"] <= width + 1
            assert box["y"] >= 0 and box["y"] < 844
            dialog.get_by_label("切分策略", exact=True).click()
            expect(page.get_by_role("option", name="按字符", exact=True)).to_be_visible()
            expect(page.locator(".ant-select-dropdown:visible")).to_have_css("opacity", "1")
            page.screenshot(path=str(output / f"{appearance}-{width}-select.png"), full_page=True, animations="disabled")
            page.keyboard.press("Escape")
            dialog.get_by_role("button", name="关闭", exact=True).last.click()

    page.set_viewport_size({"width": 1440, "height": 1000})
    navigate(page, "接入配置")
    page.get_by_label("API Key", exact=True).fill("browser-test-placeholder")
    page.get_by_role("button", name="保存配置", exact=True).click()
    expect(page.get_by_test_id("mcp-config")).to_contain_text("<API_KEY>")
    expect(page.get_by_test_id("mcp-config")).not_to_contain_text("browser-test-placeholder")
    assert page.evaluate("sessionStorage.getItem('rag.token')") == "browser-test-placeholder"
    assert page.evaluate("localStorage.getItem('rag.token')") is None
    page.get_by_label("API Key", exact=True).fill("")
    page.get_by_role("button", name="保存配置", exact=True).click()
    navigate(page, "知识文档")
    page.get_by_role("button", name="删除 maintenance.md", exact=True).click()
    confirm = page.get_by_role("dialog").filter(has=page.get_by_text("确定删除", exact=False))
    confirm.get_by_role("button", name="取消", exact=True).click()
    expect(page.get_by_role("button", name="删除 maintenance.md", exact=True)).to_be_visible()
    page.get_by_role("button", name="删除 maintenance.md", exact=True).click()
    confirm.get_by_role("button", name="删除", exact=True).click()
    expect(page.get_by_role("button", name="删除 maintenance.md", exact=True)).to_have_count(0)
    print(f"REAL_BROWSER namespace={namespace} screenshots={output.resolve()} backend={page.request.get(url + '/health').json()['backend']}")


def test_browser_failure_and_namespace_race(console_browser):
    """Controlled transport faults supplement, not replace, the real-service test."""
    page, expect, namespace, url, _output = console_browser
    payload = {"retrieval": "bm25", "rerank": "none", "evidence_status": "found", "debug": {}, "results": [{"rank": 1, "title": "OLD_WORKSPACE_RESULT", "source": "old.md", "chunk_id": "old-1", "document_id": "old", "score": 0.8, "text": "old content"}]}
    page.route("**/v1/search", lambda route: route.fulfill(json=payload))
    page.get_by_label("查询内容", exact=True).fill("test")
    page.get_by_role("button", name="运行检索", exact=True).click()
    expect(page.get_by_role("heading", name="OLD_WORKSPACE_RESULT")).to_be_visible()
    page.unroute("**/v1/search")
    page.route("**/v1/search", lambda route: route.fulfill(status=503, json={"detail": "test service unavailable"}))
    page.get_by_role("button", name="运行检索", exact=True).click()
    expect(page.get_by_text("test service unavailable", exact=False)).to_be_visible()
    expect(page.get_by_role("heading", name="OLD_WORKSPACE_RESULT")).to_have_count(0)
    page.unroute("**/v1/search")
    pending = []
    page.route("**/v1/search", lambda route: pending.append(route))
    page.get_by_role("button", name="运行检索", exact=True).click()
    expect(page.get_by_role("button", name="三模式对比", exact=True)).to_be_disabled()
    page.get_by_label("查询内容", exact=True).press("Control+Enter")
    navigate(page, "接入配置")
    page.get_by_label("Namespace", exact=True).fill(namespace + "-other")
    page.get_by_role("button", name="保存配置", exact=True).click()
    expect(page.locator(".workspace-name")).to_have_text(namespace + "-other")
    assert len(pending) == 1
    pending[0].fulfill(json=payload)
    navigate(page, "检索调试")
    expect(page.get_by_role("heading", name="OLD_WORKSPACE_RESULT")).to_have_count(0)
    expect(page.get_by_role("button", name="运行检索", exact=True)).to_be_enabled()


def test_browser_real_batch_failure_and_upload_lock(console_browser):
    page, expect, _namespace, _url, output = console_browser
    pending = []
    page.route("**/v1/documents/check", lambda route: pending.append(route))
    navigate(page, "知识文档")
    page.get_by_role("button", name="上传文档", exact=True).click()
    dialog = page.get_by_role("dialog", name="上传知识文档")
    expect(dialog).to_have_css("opacity", "1")
    dialog.locator('input[type="file"]').set_input_files([
        {"name": "batch-notes.txt", "mimeType": "text/plain", "buffer": "独立批量上传验证：维护设备前必须关闭电源。".encode()},
        {"name": "broken.docx", "mimeType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "buffer": b"invalid docx test fixture"},
    ])
    with page.expect_request("**/v1/documents/check"):
        dialog.get_by_role("button", name="上传并索引", exact=True).click()
    expect(dialog.get_by_role("button", name="上传并索引", exact=True)).to_be_disabled()
    expect(dialog.get_by_label("切分策略", exact=True)).to_be_disabled()
    expect(dialog.get_by_role("button", name="关闭", exact=True).last).to_be_disabled()
    expect(page.get_by_label("Namespace", exact=True)).to_be_disabled()
    expect(page.get_by_role("button", name="切换工作区", exact=True)).to_be_disabled()
    expect(page.get_by_label("API Key", exact=True)).to_be_disabled()
    page.keyboard.press("Escape")
    expect(dialog).to_be_visible()
    assert len(pending) == 1
    pending[0].continue_()
    expect(dialog.get_by_text("新增 1 个，跳过 0 个，失败 1 个", exact=True)).to_be_visible(timeout=180_000)
    expect(dialog.get_by_text("batch-notes.txt：已索引", exact=True)).to_be_visible()
    expect(dialog.get_by_role("button", name="上传并索引", exact=True)).to_be_enabled()
    expect(page.get_by_label("Namespace", exact=True)).to_be_enabled()
    page.screenshot(path=str(output / "batch-partial-failure.png"), full_page=True, animations="disabled")


def test_browser_document_response_isolation(console_browser):
    page, expect, namespace, _url, _output = console_browser
    pending = []
    page.route("**/v1/documents", lambda route: pending.append(route))
    with page.expect_request("**/v1/documents"):
        navigate(page, "知识文档")
    navigate(page, "接入配置")
    page.get_by_label("Namespace", exact=True).fill(namespace + "-new")
    page.get_by_role("button", name="保存配置", exact=True).click()
    expect(page.locator(".workspace-name")).to_have_text(namespace + "-new")
    pending[0].fulfill(json={"documents": [{"name": "STALE_DOCUMENT.md", "size": 100, "chunk_count": 1, "collection": "old"}]})
    page.unroute("**/v1/documents")
    navigate(page, "知识文档")
    expect(page.get_by_text("当前工作区暂无知识文档", exact=True)).to_be_visible()
    expect(page.get_by_text("STALE_DOCUMENT.md", exact=True)).to_have_count(0)


def test_browser_validation_and_partial_comparison(console_browser):
    page, expect, _namespace, _url, _output = console_browser
    requests = []
    def respond(route):
        body = route.request.post_data_json
        requests.append(body)
        if body["retrieval"] == "embedding":
            route.fulfill(status=503, json={"detail": "embedding unavailable"})
        else:
            route.fulfill(json={"retrieval": body["retrieval"], "rerank": "none", "results": [], "evidence_status": "no_evidence"})
    page.route("**/v1/search", respond)
    page.get_by_role("button", name="运行检索", exact=True).click()
    expect(page.get_by_text("请输入查询内容", exact=True)).to_be_visible()
    page.get_by_label("查询内容", exact=True).fill("test")
    page.get_by_label("候选数", exact=True).fill("1")
    page.get_by_role("button", name="运行检索", exact=True).click()
    expect(page.get_by_text("不能大于候选数", exact=True)).to_be_visible()
    assert not requests
    page.get_by_label("候选数", exact=True).fill("10")
    page.get_by_role("button", name="三模式对比", exact=True).click()
    expect(page.locator(".metrics")).to_contain_text("2/3 成功")
    expect(page.get_by_text("HTTP 503：embedding unavailable", exact=True)).to_be_visible()
    assert len(requests) == 3


def test_browser_document_name_navigation(console_browser):
    page, expect, namespace, url, output = console_browser
    names = ["设备维护 & 检查 #1.txt", "设备维护记录_" + "long-document-name-" * 6 + ".txt"]
    for index, name in enumerate(names):
        response = page.request.post(
            url + "/v1/documents",
            headers={"X-RAG-Namespace": namespace},
            multipart={"files": {"name": name, "mimeType": "text/plain",
                                 "buffer": f"文档导航验证 {index}：设备维护前必须关闭电源。".encode()}},
            timeout=180_000,
        )
        assert response.ok and len(response.json()["imported"]) == 1, response.text()

    navigate(page, "知识文档")
    breadcrumb = page.get_by_role("navigation", name="当前位置")
    detail = page.get_by_role("region", name="文档详情页面")
    for name in names:
        page.get_by_role("button", name=name, exact=True).click()
        expect(detail.get_by_role("heading", name=name, exact=True)).to_be_visible()
        expect(page.get_by_role("region", name="知识文档页面")).not_to_be_visible()
        expect(breadcrumb.locator(":scope > *")).to_have_text(
            ["工作区", "/", namespace, "/", "知识文档", "/", name]
        )
        expect(breadcrumb.locator('[aria-current="page"]')).to_have_text(name)
        breadcrumb.get_by_role("button", name="知识文档", exact=True).click()
        expect(page.get_by_role("heading", name="知识文档", exact=True)).to_be_visible()

    name = names[-1]
    document_button = page.get_by_role("button", name=name, exact=True)
    document_button.focus()
    page.keyboard.press("Enter")
    expect(detail).to_be_visible()
    for dark in (False, True):
        if dark:
            page.get_by_role("button", name="切换深色主题", exact=True).click()
        for width in (1440, 390, 320):
            page.set_viewport_size({"width": width, "height": 900})
            expect(detail.get_by_role("heading", name=name, exact=True)).to_be_visible()
            page.screenshot(path=str(output / f"document-detail-{'dark' if dark else 'light'}-{width}.png"),
                            full_page=True, animations="disabled")
            assert page.evaluate("document.documentElement.scrollWidth <= innerWidth")

    page.get_by_role("button", name="返回知识文档", exact=True).click()
    expect(document_button).to_be_visible()
    document_button.click()
    navigate(page, "检索调试")
    expect(breadcrumb.locator('[aria-current="page"]')).to_have_text("检索调试")
    navigate(page, "知识文档")
    expect(document_button).to_be_visible()
    document_button.click()
    navigate(page, "知识文档")
    expect(document_button).to_be_visible()
    document_button.click()
    page.get_by_role("button", name="切换工作区", exact=True).click()
    page.get_by_role("menuitem", name="其他工作区…", exact=True).click()
    dialog = page.get_by_role("dialog", name="切换工作区", exact=True)
    other = namespace + "-empty"
    dialog.get_by_label("工作区名称", exact=True).fill(other)
    dialog.get_by_role("button", name="切换", exact=True).click()
    expect(detail).not_to_be_visible()
    expect(breadcrumb.locator(":scope > *")).to_have_text(["工作区", "/", other, "/", "知识文档"])
    expect(page.get_by_text("当前工作区暂无知识文档", exact=True)).to_be_visible()
    page.get_by_role("button", name="切换工作区", exact=True).click()
    page.get_by_role("menuitem", name=namespace, exact=True).click()
    expect(document_button).to_be_visible()


def test_browser_workspace_switch_real_documents(console_browser):
    page, expect, namespace, url, output = console_browser
    other = namespace + "-" + "x" * (63 - len(namespace))
    headers = {"X-RAG-Namespace": namespace}
    response = page.request.post(
        url + "/v1/documents",
        headers=headers,
        multipart={"files": {"name": "workspace-switch.txt", "mimeType": "text/plain",
                             "buffer": "工作区切换验证：星河设备维护前必须关闭电源。".encode()}},
        timeout=180_000,
    )
    assert response.ok, response.text()
    assert len(response.json()["imported"]) == 1, response.text()
    navigate(page, "知识文档")
    expect(page.get_by_text("workspace-switch.txt", exact=True)).to_be_visible()
    navigate(page, "检索调试")
    page.get_by_label("查询内容", exact=True).fill("星河设备维护")
    select_option(page, "检索方式", "BM25")
    select_option(page, "重排方式", "不重排")
    page.get_by_role("button", name="运行检索", exact=True).click()
    expect(page.locator(".search-result").first).to_be_visible(timeout=180_000)

    page.get_by_role("button", name="切换工作区", exact=True).click()
    page.get_by_role("menuitem", name="其他工作区…", exact=True).click()
    dialog = page.get_by_role("dialog", name="切换工作区", exact=True)
    dialog.get_by_label("工作区名称", exact=True).fill("../invalid")
    dialog.get_by_role("button", name="切换", exact=True).click()
    expect(dialog.get_by_text("使用 1–64 个字母、数字、点、下划线或横线，首位为字母或数字", exact=True)).to_be_visible()
    dialog.get_by_label("工作区名称", exact=True).fill(other)
    dialog.get_by_role("button", name="切换", exact=True).click()
    expect(dialog).not_to_be_visible()
    expect(page.locator(".workspace-name")).to_have_text(other)
    expect(page.locator(".search-result")).to_have_count(0)
    navigate(page, "知识文档")
    expect(page.get_by_text("当前工作区暂无知识文档", exact=True)).to_be_visible()
    expect(page.get_by_text("workspace-switch.txt", exact=True)).to_have_count(0)
    navigate(page, "接入配置")
    expect(page.get_by_label("Namespace", exact=True)).to_have_value(other)
    expect(page.get_by_test_id("mcp-config")).to_contain_text(other)
    page.reload()
    expect(page.locator(".workspace-name")).to_have_text(other)
    expect(page.get_by_role("heading", name="接入配置", exact=True)).to_be_visible()

    for width in (1440, 390, 320):
        page.set_viewport_size({"width": width, "height": 900})
        page.get_by_role("button", name="切换工作区", exact=True).click()
        expect(page.get_by_role("menuitem", name=namespace, exact=True)).to_be_visible()
        page.screenshot(path=str(output / f"workspace-switch-{width}.png"), full_page=True, animations="disabled")
        assert page.evaluate("document.documentElement.scrollWidth <= innerWidth")
        page.keyboard.press("Escape")
        page.get_by_role("heading", name="接入配置", exact=True).click()

    page.get_by_role("button", name="切换工作区", exact=True).click()
    page.get_by_role("menuitem", name=namespace, exact=True).click()
    expect(page.locator(".workspace-name")).to_have_text(namespace)
    navigate(page, "知识文档")
    expect(page.get_by_text("workspace-switch.txt", exact=True)).to_be_visible()
    navigate(page, "接入配置")
    expect(page.get_by_label("Namespace", exact=True)).to_have_value(namespace)
    page.get_by_role("button", name="保存配置", exact=True).click()
    expect(page.locator(".workspace-name")).to_have_text(namespace)
