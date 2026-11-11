import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { ModelProfilesPage } from "./ModelProfilesPage";
import type { ModelProfile } from "./api";

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
  updated_at: "2026-11-09T10:00:00Z",
};

// OCR 配置把引擎 id 保存在 model 里，没有服务地址与 Key。
const ocrProfile: ModelProfile = {
  id: "mp-ocr",
  kind: "ocr",
  name: "本地 OCR",
  base_url: "",
  model: "easyocr",
  api_key_set: false,
  vector_size: null,
  updated_at: "2026-11-09T10:00:00Z",
};

function renderPage(onChanged = vi.fn()) {
  return {
    onChanged,
    ...render(
      <ConfigProvider button={{ autoInsertSpace: false }}>
        <App>
          <ModelProfilesPage disabled={false} onChanged={onChanged} />
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
    expect(screen.getByRole("tab", { name: "OCR（0）" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "JEV（0）" })).toBeTruthy();
    // Only the active kind's rows are rendered.
    expect(screen.queryByText("默认 Reranker")).toBeNull();
    expect(fetchMock.mock.calls[0][0]).toBe("/v1/admin/model-profiles");
  });

  it("filters rows by kind and shows a per-kind empty hint", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ model_profiles: [embeddingProfile, rerankerProfile] }));
    renderPage();
    await screen.findByText("默认 Embedding");

    fireEvent.click(screen.getByRole("tab", { name: "VLM（0）" }));
    expect(await screen.findByText("还没有 VLM 模型，点击「添加模型」创建")).toBeTruthy();

    fireEvent.click(screen.getByRole("tab", { name: "OCR（0）" }));
    expect(await screen.findByText("还没有 OCR 模型，点击「添加模型」创建")).toBeTruthy();

    fireEvent.click(screen.getByRole("tab", { name: "Reranker（1）" }));
    expect(await screen.findByText("默认 Reranker")).toBeTruthy();
  });

  it("shows the informative empty state when no profile exists", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ model_profiles: [] }));
    renderPage();

    expect(
      await screen.findByText("还没有模型配置，请先添加 Embedding / Reranker / VLM / OCR / JEV 模型；创建知识库时只能从其中的 Embedding / Reranker / VLM / OCR 选择"),
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
      if (path === "/v1/admin/model-profiles" && options.method === "POST")
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
      ([path, options]) => path === "/v1/admin/model-profiles" && (options as RequestInit | undefined)?.method === "POST",
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
      if (path === "/v1/admin/model-profiles" && options.method === "POST")
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
      ([path, options]) => path === "/v1/admin/model-profiles" && (options as RequestInit | undefined)?.method === "POST",
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
      if (path === "/v1/admin/model-profiles/mp-embed" && options.method === "PATCH")
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
        path === "/v1/admin/model-profiles/mp-embed" && (options as RequestInit | undefined)?.method === "PATCH",
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
      if (path === "/v1/admin/model-profiles/mp-embed" && options.method === "PATCH")
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
        path === "/v1/admin/model-profiles/mp-embed" && (options as RequestInit | undefined)?.method === "PATCH",
    );
    expect(patch).toBeTruthy();
    expect(JSON.parse(String((patch![1] as RequestInit).body))).toEqual({
      name: "默认 Embedding",
      base_url: "https://embed.example.com/v1",
      model: "bge-m3",
    });
  });

  it("offers OCR in the model-type picker and switches the form to the engine-only field", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ model_profiles: [embeddingProfile] }));
    renderPage();
    await screen.findByText("默认 Embedding");

    fireEvent.click(screen.getAllByRole("button", { name: /添加模型/ })[0]);
    const dialog = await screen.findByRole("dialog");
    // 默认是 Embedding：保留服务地址 / Key / 模型名称。
    expect(within(dialog).getByText("服务地址")).toBeTruthy();
    expect(within(dialog).getByText("API Key")).toBeTruthy();
    expect(within(dialog).queryByText("OCR 引擎")).toBeNull();

    fireEvent.mouseDown(within(dialog).getByLabelText("模型类型"));
    fireEvent.click(await screen.findByTitle("OCR"));

    // 切到 OCR 后只剩引擎下拉框，默认选择 EasyOCR。
    expect(await within(dialog).findByText("OCR 引擎")).toBeTruthy();
    expect(within(dialog).getByText("EasyOCR")).toBeTruthy();
    expect(within(dialog).queryByText("服务地址")).toBeNull();
    expect(within(dialog).queryByText("API Key")).toBeNull();
    expect(within(dialog).queryByText("获取模型列表")).toBeNull();
  });

  it("keeps OCR profiles to the engine column and creates them with an empty connection", async () => {
    fetchMock.mockImplementation(async (path: string, options: RequestInit = {}) => {
      if (path === "/v1/admin/model-profiles" && options.method === "POST")
        return jsonResponse({ profile: { ...ocrProfile, id: "mp-ocr-new", name: "新 OCR" } }, 201);
      return jsonResponse({ model_profiles: [embeddingProfile, ocrProfile] });
    });
    const { onChanged } = renderPage();
    await screen.findByText("默认 Embedding");
    fireEvent.click(screen.getByRole("tab", { name: "OCR（1）" }));

    expect(await screen.findByText("本地 OCR")).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "OCR 引擎" })).toBeTruthy();
    expect(screen.getByText("easyocr")).toBeTruthy();
    // OCR 不使用服务地址、Key 与向量维度列。
    expect(screen.queryByText("服务地址")).toBeNull();
    expect(screen.queryByText("Key")).toBeNull();
    expect(screen.queryByText("1024")).toBeNull();

    fireEvent.click(screen.getAllByRole("button", { name: /添加模型/ })[0]);
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText("OCR 引擎")).toBeTruthy();
    expect(within(dialog).queryByText("服务地址")).toBeNull();
    expect(within(dialog).queryByText("API Key")).toBeNull();
    expect(within(dialog).queryByText("获取模型列表")).toBeNull();

    fireEvent.change(within(dialog).getByLabelText("配置名称"), { target: { value: "新 OCR" } });
    fireEvent.mouseDown(within(dialog).getByLabelText("OCR 引擎"));
    fireEvent.click(await screen.findByTitle("Tesseract"));
    fireEvent.click(within(dialog).getByRole("button", { name: "保存" }));

    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    const post = fetchMock.mock.calls.find(
      ([path, options]) => path === "/v1/admin/model-profiles" && (options as RequestInit | undefined)?.method === "POST",
    );
    expect(post).toBeTruthy();
    expect(JSON.parse(String((post![1] as RequestInit).body))).toEqual({
      kind: "ocr",
      name: "新 OCR",
      base_url: "",
      model: "tesseract",
      apikey: "",
    });
  });

  it("edits an OCR profile without revealing a key or probing a model list", async () => {
    fetchMock.mockImplementation(async (path: string, options: RequestInit = {}) => {
      if (path === "/v1/admin/model-profiles/mp-ocr" && options.method === "PATCH")
        return jsonResponse({ profile: ocrProfile });
      return jsonResponse({ model_profiles: [ocrProfile] });
    });
    const { onChanged } = renderPage();
    fireEvent.click(await screen.findByRole("tab", { name: "OCR（1）" }));
    fireEvent.click(await screen.findByRole("button", { name: "编辑 本地 OCR" }));

    const dialog = await screen.findByRole("dialog");
    // 编辑时从 profile.model 回填引擎，只显示引擎下拉框。
    expect(within(dialog).getByText("EasyOCR")).toBeTruthy();
    expect(within(dialog).getByText("OCR 引擎")).toBeTruthy();
    expect(within(dialog).queryByText("API Key")).toBeNull();
    expect(within(dialog).queryByText("获取模型列表")).toBeNull();

    fireEvent.click(within(dialog).getByRole("button", { name: "保存" }));
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    const patch = fetchMock.mock.calls.find(
      ([path, options]) =>
        path === "/v1/admin/model-profiles/mp-ocr" && (options as RequestInit | undefined)?.method === "PATCH",
    );
    expect(patch).toBeTruthy();
    expect(JSON.parse(String((patch![1] as RequestInit).body))).toEqual({
      name: "本地 OCR",
      base_url: "",
      model: "easyocr",
      apikey: "",
    });
    // OCR 配置既没有 Key 可读取，也没有远端模型列表可探测。
    expect(fetchMock.mock.calls.some(([path]) => String(path).endsWith("/key"))).toBe(false);
    expect(fetchMock.mock.calls.some(([path]) => String(path).endsWith("/models"))).toBe(false);
  });
});
