import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { FeishuImportModal } from "./FeishuImportModal";
import type { KnowledgeBase } from "./api";

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

const knowledgeBase = {
  namespace: "alpha",
  collection: "kb_alpha",
  vector_size: 1024,
  chunk_size: 512,
  chunk_overlap: 64,
  strategy: "character",
  embedding_model: "bge-m3",
  reranker_model: "",
  tokenizer: "cl100k_base",
  created_at: "2026-01-01T00:00:00Z",
  embedding: { base_url: "https://embed.example.com", api_key_set: true },
  reranker: { base_url: "", api_key_set: false },
  vlm: { base_url: "", api_key_set: false },
} as KnowledgeBase;

const fetchMock = vi.fn();

function renderModal(options: { canWrite?: boolean } = {}) {
  const onClose = vi.fn();
  const onImported = vi.fn();
  render(
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <App>
        <FeishuImportModal
          open
          connection={{ namespace: "alpha", token: "" }}
          knowledgeBase={knowledgeBase}
          canWrite={options.canWrite ?? true}
          onClose={onClose}
          onImported={onImported}
        />
      </App>
    </ConfigProvider>,
  );
  return { onClose, onImported };
}

function connectedStatus() {
  return {
    enabled: true,
    connected: true,
    needs_reauth: false,
    account: { open_id: "ou_1", name: "张三" },
    scopes: ["drive:drive:readonly", "docx:document:readonly", "offline_access"],
    missing_scopes: [],
    required_scopes: ["drive:drive:readonly", "docx:document:readonly", "offline_access"],
  };
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

describe("FeishuImportModal", () => {
  it("prompts for authorization and shows the target knowledge base first", async () => {
    fetchMock.mockImplementation(async (path: string) => {
      if (path === "/v1/feishu/status")
        return jsonResponse({
          enabled: true,
          connected: false,
          needs_reauth: false,
          scopes: [],
          missing_scopes: ["drive:drive:readonly", "docx:document:readonly", "offline_access"],
          required_scopes: ["drive:drive:readonly", "docx:document:readonly", "offline_access"],
        });
      return jsonResponse({ detail: "not found" }, 404);
    });
    renderModal();
    expect(await screen.findByText("先连接你的飞书账号")).toBeTruthy();
    expect(screen.getByText(/导入目标：alpha/)).toBeTruthy();
    expect(
      screen.getByText(/谁可以检索这份副本由 Praxis-RAG 的知识库权限决定/),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: /连接飞书/ })).toBeTruthy();
  });

  it("imports selected documents and reports per-document outcomes", async () => {
    let importBody: unknown;
    fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
      const method = (init.method ?? "GET").toUpperCase();
      if (path === "/v1/feishu/status") return jsonResponse(connectedStatus());
      if (path.startsWith("/v1/feishu/files")) {
        return jsonResponse({
          folder_token: "",
          files: [
            { token: "doxcn1", name: "周报", type: "docx", importable: true, navigable: false },
            { token: "fld1", name: "子目录", type: "folder", importable: false, navigable: true },
            {
              token: "doccn1",
              name: "旧版",
              type: "doc",
              importable: false,
              navigable: false,
              unsupported_reason: "旧版飞书文档（doc）暂不支持",
            },
          ],
          has_more: false,
        });
      }
      if (path === "/v1/feishu/import" && method === "POST") {
        importBody = JSON.parse(String(init.body));
        return jsonResponse({
          namespace: "alpha",
          total: 1,
          accepted: 1,
          skipped: 0,
          failed: 0,
          items: [
            {
              document_id: "doxcn1",
              title: "周报",
              name: "周报.md",
              status: "accepted",
              artifact_id: "artifact-1",
              job: { status: "queued", stage: "queued", error: null },
            },
          ],
        });
      }
      return jsonResponse({ detail: "not found" }, 404);
    });
    const { onImported } = renderModal();
    const checkbox = (await screen.findByRole("checkbox", {
      name: "选择 周报",
    })) as HTMLInputElement;
    // An unsupported old document cannot be selected.
    expect(
      (screen.getByRole("checkbox", { name: "选择 旧版" }) as HTMLInputElement).disabled,
    ).toBe(true);
    fireEvent.click(checkbox);
    expect(screen.getByText(/已选择/)).toBeTruthy();
    const importButton = screen.getByRole("button", { name: /导入 1 篇/ });
    fireEvent.click(importButton);
    expect(
      await screen.findByText("已受理 1 篇，跳过 0 篇，失败 0 篇"),
    ).toBeTruthy();
    expect(screen.getByText("已受理")).toBeTruthy();
    expect(screen.getByText("周报.md")).toBeTruthy();
    expect(importBody).toEqual({
      documents: [{ document_id: "doxcn1", title: "周报" }],
    });
    expect(onImported).toHaveBeenCalledTimes(1);
    // The accepted item is explicitly not searchable until indexing succeeds.
    expect(screen.getByText(/只有索引状态为「已索引」后/)).toBeTruthy();
  });

  it("blocks imports without whole-knowledge-base write access", async () => {
    fetchMock.mockImplementation(async (path: string) => {
      if (path === "/v1/feishu/status") return jsonResponse(connectedStatus());
      if (path.startsWith("/v1/feishu/files"))
        return jsonResponse({
          folder_token: "",
          files: [{ token: "doxcn1", name: "周报", type: "docx", importable: true }],
          has_more: false,
        });
      return jsonResponse({ detail: "not found" }, 404);
    });
    renderModal({ canWrite: false });
    expect(await screen.findByText("没有写入权限")).toBeTruthy();
    const checkbox = (await screen.findByRole("checkbox", {
      name: "选择 周报",
    })) as HTMLInputElement;
    fireEvent.click(checkbox);
    const importButton = screen.getByRole("button", { name: /导入 1 篇/ }) as HTMLButtonElement;
    expect(importButton.disabled).toBe(true);
  });

  it("searches the account's own Feishu documents", async () => {
    let searchURL = "";
    fetchMock.mockImplementation(async (path: string) => {
      if (path === "/v1/feishu/status") return jsonResponse(connectedStatus());
      if (path.startsWith("/v1/feishu/files"))
        return jsonResponse({ folder_token: "", files: [], has_more: false });
      if (path.startsWith("/v1/feishu/search")) {
        searchURL = path;
        return jsonResponse({
          query: "周报",
          items: [{ token: "doxcn9", title: "季度周报", type: "doc", importable: true }],
          has_more: false,
          total: 1,
          offset: 0,
          count: 20,
        });
      }
      return jsonResponse({ detail: "not found" }, 404);
    });
    renderModal();
    fireEvent.click(await screen.findByRole("tab", { name: "搜索文档" }));
    const input = await screen.findByLabelText("搜索飞书文档");
    fireEvent.change(input, { target: { value: "周报" } });
    fireEvent.keyDown(input, { key: "Enter", code: "Enter", keyCode: 13 });
    expect(await screen.findByText("季度周报")).toBeTruthy();
    expect(searchURL).toContain("q=%E5%91%A8%E6%8A%A5");
  });

  it("explains when the deployment has not configured Feishu", async () => {
    fetchMock.mockImplementation(async (path: string) => {
      if (path === "/v1/feishu/status")
        return jsonResponse({
          enabled: false,
          connected: false,
          needs_reauth: false,
          scopes: [],
          missing_scopes: [],
          required_scopes: [],
        });
      return jsonResponse({ detail: "not found" }, 404);
    });
    renderModal();
    expect(await screen.findByText("飞书导入未启用")).toBeTruthy();
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(([path]) => path === "/v1/feishu/status")).toBe(true),
    );
  });
});
