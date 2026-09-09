"""Original document delivery and PDF rendering in a real browser."""

import os
import shutil
from pathlib import Path
from urllib.parse import quote

import pytest
from fastapi.testclient import TestClient

from rag_mcp.http_service import RagHttpSettings, create_service_app
from rag_mcp.service_knowledge import KnowledgeValidationError
from test_document_readers import _write_text_pdf
from test_mcp_http import AUTH, ProtocolRuntime, running_service


def seed_document(runtime, source, namespace="manuals"):
    store = runtime._tenant(namespace).documents
    shutil.copyfile(source, store.paths.knowledge_dir / source.name)
    manifest = store._read_manifest()
    manifest[source.name] = {
        "name": source.name, "size": source.stat().st_size, "chunk_count": 0,
        "collection": "preview", "indexed_at": "preview-test",
    }
    store._write_manifest(manifest)


def test_document_content_authentication_and_original_bytes(tmp_path):
    runtime = ProtocolRuntime(tmp_path / "service")
    pdf = tmp_path / "维护 & 检查 #1.pdf"
    _write_text_pdf(pdf, "Original PDF preview")
    seed_document(runtime, pdf)
    text = tmp_path / "notes.txt"
    text.write_text("维护前关闭电源。", encoding="utf-8")
    seed_document(runtime, text)
    settings = RagHttpSettings(data_root=runtime.data_root, token="test-token")
    with TestClient(create_service_app(runtime, settings=settings)) as client:
        url = "/v1/documents/" + quote(pdf.name, safe="") + "/content"
        response = client.get(url, headers=AUTH)
        assert response.status_code == 200
        assert response.content == pdf.read_bytes()
        assert response.headers["content-type"] == "application/pdf"
        assert response.headers["cache-control"] == "no-store"
        assert client.get(url).status_code == 401
        assert client.get(url, headers={"Authorization": "Bearer test-token"}).status_code == 400
        assert client.get(url, headers={**AUTH, "X-RAG-Namespace": "other"}).status_code == 404
        assert client.get("/v1/documents/missing.pdf/content", headers=AUTH).status_code == 404
        assert client.get("/v1/documents/..%5Cprivate.pdf/content", headers=AUTH).status_code == 404
        response = client.get("/v1/documents/notes.txt/content", headers=AUTH)
        assert response.text == "维护前关闭电源。"
        assert response.headers["content-type"].startswith("text/plain")
        (runtime.data_root / "manuals/knowledge" / pdf.name).unlink()
        assert client.get(url, headers=AUTH).status_code == 404


def test_document_file_rejects_symbolic_link(tmp_path):
    runtime = ProtocolRuntime(tmp_path / "service")
    source = tmp_path / "private.pdf"
    _write_text_pdf(source, "Private source")
    seed_document(runtime, source)
    target = runtime.data_root / "manuals/knowledge" / source.name
    target.unlink()
    try:
        target.symlink_to(source)
    except OSError:
        pytest.skip("Symbolic links are unavailable")
    with pytest.raises(KnowledgeValidationError):
        runtime.read_document_file("manuals", source.name)


@pytest.mark.skipif(not os.getenv("RAG_PREVIEW_PDF_DIR"), reason="Set RAG_PREVIEW_PDF_DIR to real source PDFs")
def test_browser_existing_pdf_content(tmp_path):
    from playwright.sync_api import expect, sync_playwright
    from pypdf import PdfReader

    sources = sorted(Path(os.environ["RAG_PREVIEW_PDF_DIR"]).glob("*.pdf"))
    assert sources, "No source PDFs found"
    output = Path(".test-tmp/console-browser")
    output.mkdir(parents=True, exist_ok=True)
    with running_service(tmp_path / "service") as (url, _app, runtime):
        for source in sources:
            seed_document(runtime, source)
        notes = tmp_path / "notes.txt"
        notes.write_text("维护前关闭电源。", encoding="utf-8")
        seed_document(runtime, notes)
        broken = tmp_path / "broken.pdf"
        broken.write_bytes(b"Not a valid PDF")
        seed_document(runtime, broken)
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch(channel="msedge", headless=True)
            context = browser.new_context(viewport={"width": 1440, "height": 1000}, accept_downloads=True)
            context.add_init_script("localStorage.setItem('rag.namespace', 'manuals'); sessionStorage.setItem('rag.token', 'test-token');")
            page = context.new_page()
            errors = []
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.on("response", lambda response: errors.append(f"HTTP {response.status}: {response.url}")
                    if response.status >= 400 and "/console/assets/" in response.url else None)
            page.goto(url + "/console")
            page.get_by_role("navigation", name="主导航").get_by_role("button", name="知识文档", exact=True).click()

            def rendered(page_number):
                canvas = page.locator(".pdf-viewport canvas")
                expect(canvas).to_have_attribute("aria-label", f"{source.name}，第 {page_number} 页", timeout=30000)
                assert canvas.evaluate("""canvas => {
                    const context = canvas.getContext('2d');
                    const {data} = context.getImageData(0, 0, canvas.width, canvas.height);
                    let ink = 0;
                    for (let i = 0; i < data.length; i += 16) {
                        if (data[i + 3] && Math.min(data[i], data[i + 1], data[i + 2]) < 200) ink++;
                    }
                    return ink > 100;
                }"""), "PDF canvas is blank"

            for index, source in enumerate(sources):
                page.get_by_role("button", name=source.name, exact=True).click()
                rendered(1)
                document_url = url + "/console#/workspaces/manuals/documents/" + quote(source.name, safe="")
                expect(page).to_have_url(document_url)
                count = len(PdfReader(source).pages)
                expect(page.locator(".pdf-page-count")).to_have_text(f"/ {count}")
                page.screenshot(path=str(output / f"pdf-preview-{index}-desktop.png"), full_page=True)
                if count > 1:
                    page.get_by_role("button", name="下一页", exact=True).click()
                    rendered(2)
                    page.get_by_role("button", name="上一页", exact=True).click()
                    rendered(1)
                page.get_by_label("PDF 页码", exact=True).fill(str(count))
                page.get_by_label("PDF 页码", exact=True).press("Enter")
                rendered(count)
                page.get_by_label("PDF 页码", exact=True).fill("1")
                page.get_by_label("PDF 页码", exact=True).press("Enter")
                rendered(1)
                with page.expect_download() as download:
                    page.get_by_role("link", name="下载 PDF", exact=True).click()
                assert Path(download.value.path()).read_bytes() == source.read_bytes()
                for width in (390, 320):
                    page.set_viewport_size({"width": width, "height": 900})
                    page.wait_for_function("""() => {
                        const host = document.querySelector('.pdf-viewport');
                        const canvas = host?.querySelector('canvas');
                        return canvas && Math.abs(canvas.getBoundingClientRect().width - host.clientWidth) < 2;
                    }""")
                    rendered(1)
                    page.screenshot(path=str(output / f"pdf-preview-{index}-{width}.png"), full_page=True)
                    assert page.evaluate("document.documentElement.scrollWidth <= innerWidth")
                page.get_by_label("PDF 缩放", exact=True).click()
                page.get_by_role("option", name="150%", exact=True).click()
                page.wait_for_function("""() => {
                    const host = document.querySelector('.pdf-viewport');
                    return host && host.scrollWidth > host.clientWidth * 1.4;
                }""")
                rendered(1)
                assert page.evaluate("document.documentElement.scrollWidth <= innerWidth")
                if count > 1:
                    page.get_by_role("button", name="下一页", exact=True).click()
                    rendered(2)
                expect(page).to_have_url(document_url)
                page.reload()
                rendered(1)
                expect(page.locator(".pdf-toolbar .ant-select")).to_contain_text("适合宽度")
                expect(page).to_have_url(document_url)
                with browser.new_context(viewport={"width": 1440, "height": 1000}) as linked_context:
                    linked_context.add_init_script("if (location.protocol === 'http:') { localStorage.setItem('rag.namespace', 'other'); sessionStorage.setItem('rag.token', 'test-token'); }")
                    linked = linked_context.new_page()
                    requests = []
                    linked.on("request", lambda request: requests.append(request) if "/v1/" in request.url else None)
                    linked.goto(document_url)
                    expect(linked.locator(".workspace-name")).to_have_text("manuals")
                    expect(linked.locator(".pdf-viewport canvas")).to_have_attribute("aria-label", f"{source.name}，第 1 页", timeout=30000)
                    assert requests and all(request.headers.get("x-rag-namespace") == "manuals" for request in requests)
                page.get_by_role("button", name="切换深色主题", exact=True).click()
                page.screenshot(path=str(output / f"pdf-preview-{index}-dark.png"), full_page=True)
                page.get_by_role("button", name="切换浅色主题", exact=True).click()
                page.get_by_role("button", name="返回知识文档", exact=True).click()
                page.set_viewport_size({"width": 1440, "height": 1000})
            page.get_by_role("button", name="notes.txt", exact=True).click()
            expect(page.locator(".document-text")).to_have_text("维护前关闭电源。")
            page.get_by_role("button", name="返回知识文档", exact=True).click()
            page.get_by_role("button", name="broken.pdf", exact=True).click()
            expect(page.get_by_text("PDF 预览失败", exact=True)).to_be_visible()
            expect(page.locator(".pdf-viewport canvas")).to_have_count(0)
            page.get_by_role("button", name="返回知识文档", exact=True).click()
            source = sources[0]
            stored = runtime.data_root / "manuals/knowledge" / source.name
            stored.unlink()
            page.get_by_role("button", name=source.name, exact=True).click()
            expect(page.get_by_text("文档内容加载失败", exact=True)).to_be_visible()
            shutil.copyfile(source, stored)
            page.get_by_role("button", name="重试", exact=True).click()
            rendered(1)
            browser.close()
            assert not errors, errors
