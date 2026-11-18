import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { DocumentsPage } from "./DocumentsPage";
import type { Connection } from "./api";

// jsonResponse builds the minimal Response surface the client touches.
function jsonResponse(payload: unknown, status = 200) {
  const text = JSON.stringify(payload);
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "test",
    json: async () => JSON.parse(text),
    text: async () => text,
  } as unknown as Response;
}

const busyBody = jsonResponse({ detail: "服务繁忙，请稍后重试" }, 503);

const alphaDocuments = [
  { name: "alpha-1.md", size: 128, modified_at: 1.0, indexed_at: 1.0, document_id: "d1" },
  { name: "alpha-2.md", size: 256, modified_at: 2.0, indexed_at: 2.0, document_id: "d2" },
];
const betaDocuments = [
  { name: "beta-1.md", size: 64, modified_at: 3.0, indexed_at: 3.0, document_id: "d3" },
];

const fetchMock = vi.fn();

// installDocumentsFetch serves every endpoint the page mounts with. The
// documents endpoint answers from a scriptable queue of Response values so a
// refresh can fail with 503 after a successful first load.
function installDocumentsFetch(documentsScript: Response[]) {
  fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
    const method = (init.method ?? "GET").toUpperCase();
    if (path === "/v1/documents" && method === "GET") {
      return documentsScript.length > 0 ? documentsScript.shift()! : busyBody;
    }
    if (path === "/v1/knowledge-base" && method === "GET")
      return jsonResponse({ namespace: "alpha", strategies: [] });
    if (path.startsWith("/v1/workspaces/") && path.endsWith("/entitlements"))
      return jsonResponse({ detail: "not found" }, 404);
    if (path === "/v1/document-batches" || path.startsWith("/v1/document-batches?"))
      return jsonResponse({ batches: [] });
    return jsonResponse({ detail: "not found" }, 404);
  });
}

function renderPage(connection: Connection) {
  return render(
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <App>
        <DocumentsPage
          connection={connection}
          active={true}
          authEnabled={false}
          onOpenDocument={() => {}}
          uploading={false}
          onUploading={() => {}}
          onHealth={() => Promise.resolve()}
        />
      </App>
    </ConfigProvider>,
  );
}

function wrapPage(connection: Connection, active: boolean) {
  return (
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <App>
        <DocumentsPage
          connection={connection}
          active={active}
          authEnabled={false}
          onOpenDocument={() => {}}
          uploading={false}
          onUploading={() => {}}
          onHealth={() => Promise.resolve()}
        />
      </App>
    </ConfigProvider>
  );
}

beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
  vi.stubGlobal("ResizeObserver", class {
    observe() { /* jsdom has no layout */ }
    unobserve() { /* jsdom has no layout */ }
    disconnect() { /* jsdom has no layout */ }
  });
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("DocumentsPage 降级展示", () => {
  it("首次加载失败时显示错误与重试入口，不显示“暂无文档”", async () => {
    installDocumentsFetch([busyBody]);
    renderPage({ namespace: "alpha", token: "t" });

    // 首次加载收到 503：页面显示错误提示（含服务端 detail），保留重试按钮。
    expect(await screen.findByText("文档读取失败")).toBeTruthy();
    const alert = document.querySelector(".page-alert")?.textContent ?? "";
    expect(alert).toContain("服务繁忙，请稍后重试");
    expect(screen.getByRole("button", { name: /重试/ })).toBeTruthy();
    // 首次失败绝不能伪装成“暂无文档”。
    expect(screen.queryByText("当前工作区暂无知识文档")).toBeNull();
  });

  it("同一知识库刷新失败时保留上一次成功加载的列表", async () => {
    installDocumentsFetch([
      jsonResponse({ documents: alphaDocuments }),
      busyBody,
      jsonResponse({ documents: alphaDocuments }),
    ]);
    renderPage({ namespace: "alpha", token: "t" });

    // 第一次加载成功：两个文档可见。
    expect(await screen.findByText("alpha-1.md")).toBeTruthy();
    expect(screen.getByText("alpha-2.md")).toBeTruthy();
    expect(screen.getByText("2 个文档")).toBeTruthy();

    // 刷新收到 503：列表保留，并明确提示当前是上次加载的数据。
    fireEvent.click(screen.getByRole("button", { name: "刷新文档" }));
    await waitFor(() => {
      expect(screen.getByText("刷新失败，当前显示上次加载的数据")).toBeTruthy();
    });
    expect(screen.getByText("alpha-1.md")).toBeTruthy();
    expect(screen.getByText("alpha-2.md")).toBeTruthy();
    expect(screen.getByText("2 个文档")).toBeTruthy();

    // 重试入口恢复成功后，提示消失、列表仍完整。
    fireEvent.click(screen.getByRole("button", { name: /重试/ }));
    await waitFor(() => {
      expect(screen.queryByText("刷新失败，当前显示上次加载的数据")).toBeNull();
    });
    expect(screen.getByText("alpha-1.md")).toBeTruthy();
    expect(screen.getByText("2 个文档")).toBeTruthy();
  });

  it("切换知识库后不再显示上一个知识库的旧列表", async () => {
    installDocumentsFetch([
      jsonResponse({ documents: alphaDocuments }),
      jsonResponse({ documents: betaDocuments }),
    ]);
    const view = render(wrapPage({ namespace: "alpha", token: "t" }, true));
    expect(await screen.findByText("alpha-1.md")).toBeTruthy();

    // 离开页面（active=false）并切到另一个知识库：旧列表立即清空。
    view.rerender(wrapPage({ namespace: "beta", token: "t" }, false));
    await waitFor(() => {
      expect(screen.queryByText("alpha-1.md")).toBeNull();
      expect(screen.queryByText("alpha-2.md")).toBeNull();
    });

    // 进入新知识库后加载的是它自己的列表，而不是 alpha 的旧数据。
    view.rerender(wrapPage({ namespace: "beta", token: "t" }, true));
    expect(await screen.findByText("beta-1.md")).toBeTruthy();
    expect(screen.queryByText("alpha-1.md")).toBeNull();
  });
});
