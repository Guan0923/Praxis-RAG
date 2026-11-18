import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { KnowledgeBasesPage } from "./KnowledgeBasesPage";

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

function textResponse(body: string, status: number) {
  return {
    ok: false,
    status,
    statusText: "test",
    json: async () => JSON.parse(body),
    text: async () => body,
  } as unknown as Response;
}

// KnowledgeBase mirror of the admin list row the page renders.
type KnowledgeBase = {
  namespace: string;
  configured: boolean;
  document_count: number;
  embedding_model?: string;
  embedding?: { base_url: string; api_key_set: boolean };
  reranker_model?: string;
  reranker?: { base_url: string; api_key_set: boolean };
  // 配置抽屉里知识库表单还会读取切分参数、VLM / 知识图谱的可选配置。
  vlm?: { base_url: string; api_key_set: boolean };
  vlm_model?: string;
  graph?: { base_url: string; api_key_set: boolean };
  graph_model?: string;
  graph_enabled?: boolean;
  strategy?: string;
  chunk_size?: number;
  chunk_overlap?: number;
  collection?: string;
  tokenizer?: string;
  created_at?: string;
  vector_size?: number;
};

// BatchReply is the raw knowledge-base batch-delete transport shape; the client
// normalizes both `failed[].namespace` and `failed[].id` internally.
type BatchReply = {
  deleted?: string[];
  failed?: { namespace?: string; error?: string }[];
};

function knowledgeBase(namespace: string, extra: Partial<KnowledgeBase> = {}): KnowledgeBase {
  return {
    namespace,
    configured: true,
    document_count: 2,
    embedding_model: "bge-m3",
    embedding: { base_url: "https://embed.example.com/v1", api_key_set: true },
    reranker_model: "",
    vector_size: 1024,
    ...extra,
  };
}

const baseKnowledgeBases: KnowledgeBase[] = [
  knowledgeBase("kb-a"),
  knowledgeBase("kb-b"),
  knowledgeBase("kb-c"),
];

const fetchMock = vi.fn();

// installFetch serves GET /v1/admin/knowledge-bases from an in-memory list and
// applies the batch-delete reply so the reload after a delete shows the result.
function installFetch(
  options: { initial?: KnowledgeBase[]; batch?: (namespaces: string[], current: KnowledgeBase[]) => BatchReply } = {},
) {
  let current = (options.initial ?? baseKnowledgeBases).map((item) => ({ ...item }));
  fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
    const method = (init.method ?? "GET").toUpperCase();
    if (path === "/v1/admin/knowledge-bases" && method === "GET")
      return jsonResponse({ knowledge_bases: current });
    if (path === "/v1/admin/knowledge-bases/batch-delete" && method === "POST") {
      const body = JSON.parse(String(init.body)) as { namespaces: string[] };
      const present = current.filter((item) => body.namespaces.includes(item.namespace));
      const reply: BatchReply = options.batch ? options.batch(body.namespaces, current) : {
        deleted: present.map((item) => item.namespace),
        failed: [],
      };
      const gone = reply.deleted ?? [];
      current = current.filter((item) => !gone.includes(item.namespace));
      return jsonResponse(reply);
    }
    return jsonResponse({ detail: "not found" }, 404);
  });
  return { current: () => current };
}

function renderPage() {
  const onChanged = vi.fn();
  const onCreate = vi.fn();
  return {
    onChanged,
    onCreate,
    ...render(
      <ConfigProvider button={{ autoInsertSpace: false }}>
        <App>
          <KnowledgeBasesPage disabled={false} onCreate={onCreate} onChanged={onChanged} />
        </App>
      </ConfigProvider>,
    ),
  };
}

function batchButton() {
  return screen.getByRole("button", { name: /批量删除/ }) as HTMLButtonElement;
}

function checkbox(namespace: string) {
  return screen.getByRole("checkbox", { name: "选择 " + namespace }) as HTMLInputElement;
}

function listCalls() {
  return fetchMock.mock.calls.filter(
    ([path, init]) =>
      path === "/v1/admin/knowledge-bases"
      && (((init as RequestInit | undefined)?.method ?? "GET").toUpperCase() === "GET"),
  ).length;
}

// stubRows gives every rendered table row a deterministic client rect so the
// marquee hook can hit-test them (jsdom performs no layout).
function stubRows(container: HTMLElement) {
  const rect = (top: number, bottom: number): DOMRect => ({
    x: 20, y: top, top, left: 20, right: 500, bottom,
    width: 480, height: bottom - top, toJSON: () => ({}),
  } as DOMRect);
  const rows = Array.from(container.querySelectorAll<HTMLElement>("tbody tr[data-row-key]"));
  expect(rows.length).toBe(3);
  rows.forEach((row, index) => {
    Object.defineProperty(row, "getBoundingClientRect", { value: () => rect(40 + index * 40, 80 + index * 40) });
  });
  const table = container.querySelector<HTMLElement>(".knowledge-bases-table");
  expect(table).toBeTruthy();
  Object.defineProperty(table!, "getBoundingClientRect", {
    value: () => ({ x: 0, y: 0, top: 0, left: 0, right: 900, bottom: 400, width: 900, height: 400, toJSON: () => ({}) } as DOMRect),
  });
  return table!;
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

describe("KnowledgeBasesPage batch delete", () => {
  it("renders the knowledge-base rows with an inactive batch button", async () => {
    installFetch();
    renderPage();

    expect(await screen.findByText("kb-a")).toBeTruthy();
    expect(screen.getByText("kb-b")).toBeTruthy();
    expect(screen.getByText("kb-c")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "知识库" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "刷新知识库" })).toBeTruthy();
    expect(screen.getByRole("button", { name: /创建知识库/ })).toBeTruthy();
    expect(batchButton().disabled).toBe(true);
    expect(batchButton().textContent).toBe("批量删除");
    expect(checkbox("kb-a").disabled).toBe(false);
    expect(fetchMock.mock.calls[0][0]).toBe("/v1/admin/knowledge-bases");
  });

  it("keeps the batch delete button disabled until a checkbox row is selected", async () => {
    installFetch();
    renderPage();
    await screen.findByText("kb-a");

    expect(batchButton().disabled).toBe(true);
    fireEvent.click(checkbox("kb-a"));
    await waitFor(() => expect(batchButton().disabled).toBe(false));
    expect(batchButton().textContent).toContain("1");

    fireEvent.click(checkbox("kb-a"));
    await waitFor(() => expect(batchButton().disabled).toBe(true));
  });

  it("confirms, posts the selected namespaces and reloads after a full success", async () => {
    const { current } = installFetch();
    const { onChanged } = renderPage();
    await screen.findByText("kb-a");

    fireEvent.click(checkbox("kb-a"));
    fireEvent.click(checkbox("kb-b"));
    fireEvent.click(batchButton());

    const dialog = await screen.findByRole("dialog");
    // antd 6 renders the title in both the header and the confirm body.
    expect(within(dialog).getByText("删除知识库", { selector: ".ant-modal-confirm-title" })).toBeTruthy();
    expect(within(dialog).getByText(/删除所选 2 个知识库及其全部文档（不可恢复）/)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    await waitFor(() => expect(current().map((item) => item.namespace)).toEqual(["kb-c"]));
    const post = fetchMock.mock.calls.find(
      ([path, init]) =>
        path === "/v1/admin/knowledge-bases/batch-delete"
        && (init as RequestInit | undefined)?.method === "POST",
    );
    expect(post).toBeTruthy();
    expect(JSON.parse(String((post![1] as RequestInit).body))).toEqual({ namespaces: ["kb-a", "kb-b"] });

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(listCalls()).toBeGreaterThanOrEqual(2));
    await waitFor(() => expect(screen.queryByText("kb-a")).toBeNull());
    await waitFor(() => expect(batchButton().textContent).toBe("批量删除"));
    expect(batchButton().disabled).toBe(true);
    expect(await screen.findByText("已删除 2 个知识库")).toBeTruthy();
  });

  it("keeps failed namespaces selected and lists them in a warning alert until the next refresh", async () => {
    installFetch({
      batch: () => ({
        deleted: ["kb-a"],
        failed: [{ namespace: "kb-b", error: "文档删除失败，请先停止上传任务" }],
      }),
    });
    const { onChanged } = renderPage();
    await screen.findByText("kb-a");

    fireEvent.click(checkbox("kb-a"));
    fireEvent.click(checkbox("kb-b"));
    fireEvent.click(batchButton());
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    const alertTitle = await screen.findByText("部分知识库删除失败");
    const alert = alertTitle.closest(".ant-alert") as HTMLElement;
    expect(alert).toBeTruthy();
    expect(alert.textContent).toContain("kb-b");
    expect(alert.textContent).toContain("文档删除失败，请先停止上传任务");

    await waitFor(() => expect(checkbox("kb-b").checked).toBe(true));
    expect(screen.queryByRole("checkbox", { name: "选择 kb-a" })).toBeNull();
    expect(batchButton().textContent).toContain("1");
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));

    // A manual refresh is a reload, so the stale failure alert goes away.
    fireEvent.click(screen.getByRole("button", { name: "刷新知识库" }));
    await waitFor(() => expect(screen.queryByText("部分知识库删除失败")).toBeNull());
    // The failed namespace stays selected so the operator can retry it.
    expect(batchButton().textContent).toContain("1");
  });

  it("reports a request-level failure and keeps the selection for retry", async () => {
    fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
      const method = (init.method ?? "GET").toUpperCase();
      if (path === "/v1/admin/knowledge-bases" && method === "GET")
        return jsonResponse({ knowledge_bases: baseKnowledgeBases });
      if (path === "/v1/admin/knowledge-bases/batch-delete" && method === "POST")
        return textResponse('{"detail":"数据库繁忙"}', 500);
      return jsonResponse({ detail: "not found" }, 404);
    });
    const { onChanged } = renderPage();
    await screen.findByText("kb-a");

    fireEvent.click(checkbox("kb-a"));
    fireEvent.click(batchButton());
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    expect(await screen.findByText(/HTTP 500/)).toBeTruthy();
    await waitFor(() => expect(checkbox("kb-a").checked).toBe(true));
    expect(batchButton().textContent).toContain("1");
    expect(onChanged).not.toHaveBeenCalled();
    expect(listCalls()).toBe(1);
  });

  it("keeps checkbox-selected rows when a later marquee drag covers other rows", async () => {
    installFetch();
    const { container } = renderPage();
    await screen.findByText("kb-a");
    const table = stubRows(container);

    fireEvent.click(checkbox("kb-c"));
    await waitFor(() => expect(checkbox("kb-c").checked).toBe(true));

    // Band covers kb-a only; kb-c stays selected because a drag may only add.
    fireEvent.mouseDown(table, { button: 0, clientX: 30, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 400, clientY: 79 });
    fireEvent.mouseUp(window, { clientX: 400, clientY: 79 });

    await waitFor(() => expect(checkbox("kb-a").checked).toBe(true));
    expect(checkbox("kb-c").checked).toBe(true);
    expect(checkbox("kb-b").checked).toBe(false);
    await waitFor(() => expect(batchButton().textContent).toContain("2"));
  });

  it("selects rows covered by a left-mouse marquee drag", async () => {
    installFetch();
    const { container } = renderPage();
    await screen.findByText("kb-a");
    const table = stubRows(container);

    fireEvent.mouseDown(table, { button: 0, clientX: 30, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 400, clientY: 100 });
    await waitFor(() => expect(document.querySelector(".drag-band")).toBeTruthy());
    fireEvent.mouseUp(window, { clientX: 400, clientY: 100 });

    await waitFor(() => expect(checkbox("kb-a").checked).toBe(true));
    expect(checkbox("kb-b").checked).toBe(true);
    expect(checkbox("kb-c").checked).toBe(false);
    await waitFor(() => expect(batchButton().textContent).toContain("2"));
    expect(batchButton().disabled).toBe(false);
    await waitFor(() => expect(document.querySelector(".drag-band")).toBeNull());
  });
});

// 配置抽屉复用知识库表单：这里只验证「启用知识图谱」开关与保存请求体。
describe("KnowledgeBasesPage configuration drawer", () => {
  it("saves graph_enabled=false without requiring a graph model while the switch is off", async () => {
    const detail: KnowledgeBase = {
      ...knowledgeBase("kb-a"),
      embedding: { base_url: "https://embed.example.com/v1", api_key_set: false },
      reranker: { base_url: "", api_key_set: false },
      strategy: "recursive",
      chunk_size: 512,
      chunk_overlap: 64,
      collection: "kb_a",
      tokenizer: "bge",
      created_at: "2026-11-09T10:00:00Z",
    };
    fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
      const method = (init.method ?? "GET").toUpperCase();
      if (path === "/v1/admin/knowledge-bases" && method === "GET")
        return jsonResponse({ knowledge_bases: [detail] });
      if (path === "/v1/admin/knowledge-bases/defaults")
        return jsonResponse({
          default: "recursive",
          strategies: [
            { id: "recursive", label: "递归切分", unit: "tokens", chunk_size: 512, chunk_overlap: 64 },
          ],
        });
      if (path === "/v1/admin/knowledge-bases/kb-a" && method === "GET") return jsonResponse(detail);
      if (path === "/v1/admin/knowledge-bases/kb-a" && method === "PATCH") return jsonResponse(detail);
      if (path === "/v1/model-profiles") return jsonResponse({ model_profiles: [] });
      return jsonResponse({ detail: "not found" }, 404);
    });
    const { onChanged } = renderPage();
    await screen.findByText("kb-a");

    fireEvent.click(screen.getByRole("button", { name: "配置 kb-a" }));
    const dialog = await screen.findByRole("dialog");
    // 编辑抽屉里新增「启用知识图谱」开关：默认关闭，且不展示图谱模型选择。
    const toggle = await within(dialog).findByRole("switch", { name: "启用知识图谱" });
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    expect(within(dialog).queryByLabelText("知识图谱 模型")).toBeNull();

    fireEvent.click(within(dialog).getByRole("button", { name: /保存配置/ }));
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    const patch = fetchMock.mock.calls.find(
      ([path, options]) =>
        path === "/v1/admin/knowledge-bases/kb-a"
        && (((options as RequestInit | undefined)?.method) ?? "GET") === "PATCH",
    );
    expect(patch).toBeTruthy();
    expect(JSON.parse(String((patch![1] as RequestInit).body))).toMatchObject({
      graph_enabled: false,
      graph_model: "",
      graph: { base_url: "", apikey: "" },
    });
  });
});
