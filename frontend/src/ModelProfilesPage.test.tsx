import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { ModelProfilesPage } from "./ModelProfilesPage";
import type { ModelProfile, Role } from "./api";

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

const fetchMock = vi.fn();

const embeddingProfile: ModelProfile = {
  id: "mp-embed",
  kind: "embedding",
  name: "默认 Embedding",
  base_url: "https://embed.example.com/v1",
  model: "bge-m3",
  api_key_set: true,
  vector_size: 1024,
  owner_user_id: "user-self",
  owner_username: "alice",
  mine: true,
  updated_at: "2026-11-09T10:00:00Z",
};

const rerankerProfile: ModelProfile = {
  id: "mp-rerank",
  kind: "reranker",
  name: "默认 Reranker",
  base_url: "https://rerank.example.com/v1",
  model: "bge-reranker",
  api_key_set: false,
  vector_size: null,
  owner_user_id: "user-self",
  owner_username: "alice",
  mine: true,
  updated_at: "2026-11-09T10:00:00Z",
};

const vlmProfile: ModelProfile = {
  id: "mp-vlm",
  kind: "vlm",
  name: "视觉模型",
  base_url: "https://vlm.example.com/v1",
  model: "qwen2.5-vl-72b",
  api_key_set: false,
  vector_size: null,
  owner_user_id: "user-self",
  owner_username: "alice",
  mine: true,
  updated_at: "2026-11-09T10:00:00Z",
};

// otherAccountProfile 模拟管理员在只读总览里看到的、别人拥有的配置。
const otherAccountProfile: ModelProfile = {
  id: "mp-bob",
  kind: "embedding",
  name: "Bob 的 Embedding",
  base_url: "https://bob.example.com/v1",
  model: "bge-large",
  api_key_set: true,
  vector_size: 1024,
  owner_user_id: "user-bob",
  owner_username: "bob",
  mine: false,
  updated_at: "2026-11-09T10:00:00Z",
};

function renderPage(options: { onChanged?: () => void; role?: Role } = {}) {
  const onChanged = options.onChanged ?? vi.fn();
  return {
    onChanged,
    ...render(
      <ConfigProvider button={{ autoInsertSpace: false }}>
        <App>
          <ModelProfilesPage disabled={false} onChanged={onChanged} role={options.role ?? "admin"} />
        </App>
      </ConfigProvider>,
    ),
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

describe("ModelProfilesPage", () => {
  it("renders the per-kind counts, rows and saved-key state", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ model_profiles: [embeddingProfile, rerankerProfile] }));
    renderPage();

    expect(await screen.findByText("默认 Embedding")).toBeTruthy();
    expect(screen.getByText("bge-m3")).toBeTruthy();
    expect(screen.getByText("https://embed.example.com/v1")).toBeTruthy();
    expect(screen.getByText("已保存")).toBeTruthy();
    expect(screen.getByText("1024")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "模型配置" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Embedding（1）" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Reranker（1）" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "VLM（0）" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "JEV（0）" })).toBeTruthy();
    expect(screen.queryByRole("tab", { name: /OCR/ })).toBeNull();
    // Only the active kind's rows are rendered.
    expect(screen.queryByText("默认 Reranker")).toBeNull();
    expect(fetchMock.mock.calls[0][0]).toBe("/v1/model-profiles");
  });

  it("filters rows by kind and shows a per-kind empty hint", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ model_profiles: [embeddingProfile, rerankerProfile] }));
    renderPage();
    await screen.findByText("默认 Embedding");

    fireEvent.click(screen.getByRole("tab", { name: "VLM（0）" }));
    expect(await screen.findByText("还没有 VLM 模型，点击「添加模型」创建")).toBeTruthy();

    fireEvent.click(screen.getByRole("tab", { name: "Reranker（1）" }));
    expect(await screen.findByText("默认 Reranker")).toBeTruthy();
  });

  it("shows the informative empty state when no profile exists", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ model_profiles: [] }));
    renderPage();

    expect(
      await screen.findByText("还没有模型配置，请先添加 Embedding / Reranker / VLM / JEV 模型；创建知识库时只能从其中的 Embedding / Reranker / VLM 选择"),
    ).toBeTruthy();
    expect(screen.getAllByRole("button", { name: /添加模型/ })[0]).toBeTruthy();
  });

  it("opens the add drawer and validates the required fields", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ model_profiles: [] }));
    renderPage();
    await screen.findByText(/还没有模型配置/);

    fireEvent.click(screen.getAllByRole("button", { name: /添加模型/ })[0]);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("添加模型")).toBeTruthy();

    fireEvent.click(within(dialog).getByRole("button", { name: "保存" }));
    expect(await within(dialog).findByText("请输入配置名称")).toBeTruthy();
    expect(within(dialog).getByText("请输入服务地址")).toBeTruthy();
    expect(within(dialog).getByText("请输入或获取模型名称")).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("creates a profile with the default kind and notifies the parent", async () => {
    fetchMock.mockImplementation(async (path: string, options: RequestInit = {}) => {
      if (path === "/v1/model-profiles" && options.method === "POST")
        return jsonResponse({ profile: { ...embeddingProfile, id: "mp-new", name: "生产 Embedding" } }, 201);
      return jsonResponse({ model_profiles: [embeddingProfile] });
    });
    const { onChanged } = renderPage();
    await screen.findByText("默认 Embedding");

    fireEvent.click(screen.getAllByRole("button", { name: /添加模型/ })[0]);
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("配置名称"), { target: { value: "生产 Embedding" } });
    fireEvent.change(within(dialog).getByLabelText("服务地址"), {
      target: { value: "https://api.example.com/v1/" },
    });
    fireEvent.change(within(dialog).getByLabelText("模型名称"), { target: { value: "bge-m3" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "保存" }));

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    const post = fetchMock.mock.calls.find(
      ([path, options]) => path === "/v1/model-profiles" && (options as RequestInit | undefined)?.method === "POST",
    );
    expect(post).toBeTruthy();
    expect(JSON.parse(String((post![1] as RequestInit).body))).toEqual({
      kind: "embedding",
      name: "生产 Embedding",
      base_url: "https://api.example.com/v1",
      model: "bge-m3",
      apikey: "",
    });
  });

  it("creates a configuration-only JEV profile without probing a model list", async () => {
    fetchMock.mockImplementation(async (path: string, options: RequestInit = {}) => {
      if (path === "/v1/model-profiles" && options.method === "POST")
        return jsonResponse({
          profile: {
            ...embeddingProfile,
            id: "mp-jev",
            kind: "jev",
            name: "JEV 默认",
            base_url: "https://api.typesafe.ai/v1",
            model: "jev-latest",
            api_key_set: false,
            vector_size: null,
          },
        }, 201);
      return jsonResponse({ model_profiles: [] });
    });
    const { onChanged } = renderPage();
    await screen.findByText(/还没有模型配置/);

    fireEvent.click(screen.getByRole("tab", { name: "JEV（0）" }));
    fireEvent.click(screen.getAllByRole("button", { name: /添加模型/ })[0]);
    const dialog = await screen.findByRole("dialog");
    // JEV 只保存连接配置：有服务地址与 Key，但没有 /models 探测按钮。
    expect(within(dialog).getByText("服务地址")).toBeTruthy();
    expect(within(dialog).getByText("API Key")).toBeTruthy();
    expect(within(dialog).queryByText("获取模型列表")).toBeNull();

    fireEvent.change(within(dialog).getByLabelText("配置名称"), { target: { value: "JEV 默认" } });
    fireEvent.change(within(dialog).getByLabelText("服务地址"), {
      target: { value: "https://api.typesafe.ai/v1" },
    });
    fireEvent.change(within(dialog).getByLabelText("模型名称"), { target: { value: "jev-latest" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "保存" }));

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    const post = fetchMock.mock.calls.find(
      ([path, options]) => path === "/v1/model-profiles" && (options as RequestInit | undefined)?.method === "POST",
    );
    expect(post).toBeTruthy();
    expect(JSON.parse(String((post![1] as RequestInit).body))).toEqual({
      kind: "jev",
      name: "JEV 默认",
      base_url: "https://api.typesafe.ai/v1",
      model: "jev-latest",
      apikey: "",
    });
    // JEV 没有 OpenAI /models 路由：页面绝不发起模型列表探测。
    expect(fetchMock.mock.calls.some(([path]) => String(path).endsWith("/models"))).toBe(false);
  });

  it("clears a saved key with an explicit empty apikey on PATCH", async () => {
    fetchMock.mockImplementation(async (path: string, options: RequestInit = {}) => {
      if (path === "/v1/model-profiles/mp-embed" && options.method === "PATCH")
        return jsonResponse({ profile: { ...embeddingProfile, api_key_set: false } });
      return jsonResponse({ model_profiles: [embeddingProfile] });
    });
    const { onChanged } = renderPage();
    await screen.findByText("默认 Embedding");

    fireEvent.click(screen.getByRole("button", { name: "编辑 默认 Embedding" }));
    const dialog = await screen.findByRole("dialog");
    // The saved key marker and the removal action only exist in edit mode.
    expect(within(dialog).getByText("已保存 Key，留空保持不变")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "清除已保存的 Key" }));
    expect(within(dialog).getByText("保存后将清除已保存的 Key")).toBeTruthy();

    fireEvent.click(within(dialog).getByRole("button", { name: "保存" }));
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    const patch = fetchMock.mock.calls.find(
      ([path, options]) =>
        path === "/v1/model-profiles/mp-embed" && (options as RequestInit | undefined)?.method === "PATCH",
    );
    expect(patch).toBeTruthy();
    expect(JSON.parse(String((patch![1] as RequestInit).body))).toEqual({
      name: "默认 Embedding",
      base_url: "https://embed.example.com/v1",
      model: "bge-m3",
      apikey: "",
    });
  });

  it("omits apikey on PATCH while the key field is left untouched", async () => {
    fetchMock.mockImplementation(async (path: string, options: RequestInit = {}) => {
      if (path === "/v1/model-profiles/mp-embed" && options.method === "PATCH")
        return jsonResponse({ profile: embeddingProfile });
      return jsonResponse({ model_profiles: [embeddingProfile] });
    });
    const { onChanged } = renderPage();
    await screen.findByText("默认 Embedding");

    fireEvent.click(screen.getByRole("button", { name: "编辑 默认 Embedding" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "保存" }));

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    const patch = fetchMock.mock.calls.find(
      ([path, options]) =>
        path === "/v1/model-profiles/mp-embed" && (options as RequestInit | undefined)?.method === "PATCH",
    );
    expect(patch).toBeTruthy();
    expect(JSON.parse(String((patch![1] as RequestInit).body))).toEqual({
      name: "默认 Embedding",
      base_url: "https://embed.example.com/v1",
      model: "bge-m3",
    });
  });

  it("offers only the four remote model kinds in the model-type picker", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ model_profiles: [embeddingProfile] }));
    renderPage();
    await screen.findByText("默认 Embedding");

    fireEvent.click(screen.getAllByRole("button", { name: /添加模型/ })[0]);
    const dialog = await screen.findByRole("dialog");
    // 默认是 Embedding：保留服务地址 / Key / 模型名称。
    expect(within(dialog).getByText("服务地址")).toBeTruthy();
    expect(within(dialog).getByText("API Key")).toBeTruthy();

    fireEvent.mouseDown(within(dialog).getByLabelText("模型类型"));
    for (const kind of ["Embedding", "Reranker", "VLM", "JEV"]) {
      expect((await screen.findAllByTitle(kind)).length).toBeGreaterThan(0);
    }
    // OCR 已并入 VLM：模型类型里不再提供 OCR 引擎。
    expect(screen.queryAllByTitle("OCR")).toHaveLength(0);
  });

  it("marks the owner column and keeps other accounts' rows read-only for an administrator", async () => {
    fetchMock.mockResolvedValue(jsonResponse({
      model_profiles: [embeddingProfile, otherAccountProfile],
    }));
    renderPage({ role: "admin" });

    expect(await screen.findByText("默认 Embedding")).toBeTruthy();
    // 管理员视图带「所有者」列：自己的行标记为「自己」，别人的行显示账号名。
    expect(screen.getByRole("columnheader", { name: "所有者" })).toBeTruthy();
    expect(screen.getByText("自己")).toBeTruthy();
    expect(screen.getByText("bob")).toBeTruthy();
    expect(screen.getByText("管理员只读视图")).toBeTruthy();

    const ownEdit = screen.getByRole("button", { name: "编辑 默认 Embedding" }) as HTMLButtonElement;
    const otherEdit = screen.getByRole("button", { name: "编辑 Bob 的 Embedding" }) as HTMLButtonElement;
    const otherDelete = screen.getByRole("button", { name: "删除 Bob 的 Embedding" }) as HTMLButtonElement;
    expect(ownEdit.disabled).toBe(false);
    // 别人的配置只读：不能编辑、删除，也不会去读取别人的 Key。
    expect(otherEdit.disabled).toBe(true);
    expect(otherDelete.disabled).toBe(true);
    expect(fetchMock.mock.calls.some(([path]) => String(path).endsWith("/key"))).toBe(false);
    // 只读行不提供编辑抽屉入口。
    fireEvent.click(otherEdit);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("hides the owner column for a normal account", async () => {
    // 普通账号的列表由服务端限定为自己的配置，页面不需要所有者列。
    fetchMock.mockResolvedValue(jsonResponse({ model_profiles: [embeddingProfile] }));
    renderPage({ role: "user" });

    expect(await screen.findByText("默认 Embedding")).toBeTruthy();
    expect(screen.queryByRole("columnheader", { name: "所有者" })).toBeNull();
    expect((screen.getByRole("button", { name: "编辑 默认 Embedding" }) as HTMLButtonElement).disabled)
      .toBe(false);
    expect(fetchMock.mock.calls[0][0]).toBe("/v1/model-profiles");
  });

  it("does not probe the administrator model-list route for a normal account", async () => {
    fetchMock.mockImplementation(async (path: string) => {
      if (path === "/v1/model-profiles") return jsonResponse({ model_profiles: [] });
      return jsonResponse({});
    });
    renderPage({ role: "user" });
    await screen.findByText(/还没有模型配置/);

    fireEvent.click(screen.getAllByRole("button", { name: /添加模型/ })[0]);
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("服务地址"), {
      target: { value: "https://api.example.com/v1" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: /获取模型列表/ }));

    // 未保存的连接只有管理员能探测；普通账号保存后可用自己的模型列表接口。
    expect(await within(dialog).findByText("保存配置后可从该地址获取模型列表，也可直接输入模型名称"))
      .toBeTruthy();
    expect(fetchMock.mock.calls.some(
      ([path]) => String(path).startsWith("/v1/admin/knowledge-bases/models/"),
    )).toBe(false);
  });

  it("creates a VLM profile with a remote vision connection and probes the model list", async () => {
    fetchMock.mockImplementation(async (path: string, options: RequestInit = {}) => {
      if (path === "/v1/model-profiles" && options.method === "POST")
        return jsonResponse({
          profile: {
            ...embeddingProfile,
            id: "mp-vlm",
            kind: "vlm",
            name: "视觉模型",
            base_url: "https://vlm.example.com/v1",
            model: "qwen2.5-vl-72b",
            vector_size: null,
          },
        }, 201);
      if (path === "/v1/admin/knowledge-bases/models/vlm")
        return jsonResponse({ models: ["qwen2.5-vl-72b"] });
      return jsonResponse({ model_profiles: [embeddingProfile, vlmProfile] });
    });
    const { onChanged } = renderPage();
    await screen.findByText("默认 Embedding");
    fireEvent.click(screen.getByRole("tab", { name: "VLM（1）" }));

    expect(await screen.findByText("视觉模型")).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "模型" })).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "服务地址" })).toBeTruthy();
    expect(screen.getByText("https://vlm.example.com/v1")).toBeTruthy();

    fireEvent.click(screen.getAllByRole("button", { name: /添加模型/ })[0]);
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("配置名称"), { target: { value: "视觉模型" } });
    fireEvent.change(within(dialog).getByLabelText("服务地址"), {
      target: { value: "https://vlm.example.com/v1" },
    });
    fireEvent.change(within(dialog).getByLabelText("模型名称"), {
      target: { value: "qwen2.5-vl-72b" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: /获取模型列表/ }));
    // VLM 是远端 OpenAI 兼容模型：管理员允许按服务地址探测模型列表。
    await waitFor(() => expect(fetchMock.mock.calls.some(
      ([path]) => path === "/v1/admin/knowledge-bases/models/vlm",
    )).toBe(true));
    fireEvent.click(within(dialog).getByRole("button", { name: "保存" }));

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    const post = fetchMock.mock.calls.find(
      ([path, options]) => path === "/v1/model-profiles" && (options as RequestInit | undefined)?.method === "POST",
    );
    expect(post).toBeTruthy();
    expect(JSON.parse(String((post![1] as RequestInit).body))).toEqual({
      kind: "vlm",
      name: "视觉模型",
      base_url: "https://vlm.example.com/v1",
      model: "qwen2.5-vl-72b",
      apikey: "",
    });
  });
});
