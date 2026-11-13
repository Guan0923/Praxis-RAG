import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { createElement } from "react";
import { KnowledgeBaseForm, matchingProfile, profileNeedsReembed } from "./CreateKnowledgeBase";
import type { KnowledgeBase, ModelProfile } from "./api";

function profile(overrides: Partial<ModelProfile> = {}): ModelProfile {
  return {
    id: "mp-embed",
    kind: "embedding",
    name: "默认 Embedding",
    base_url: "https://api.example.com/v1",
    model: "bge-m3",
    api_key_set: true,
    vector_size: 1024,
    ...overrides,
  };
}

function vlmProfile(overrides: Partial<ModelProfile> = {}): ModelProfile {
  return {
    id: "mp-vlm",
    kind: "vlm",
    name: "视觉模型",
    base_url: "https://vlm.example.com/v1",
    model: "qwen2.5-vl-72b",
    api_key_set: false,
    vector_size: null,
    ...overrides,
  };
}

const defaults = {
  default: "recursive",
  strategies: [
    { id: "recursive", label: "递归切分", unit: "tokens", chunk_size: 512, chunk_overlap: 64 },
  ],
};

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

// Only the fields the matching / reembed helpers read are needed.
const initial = {
  embedding: { base_url: "https://api.example.com/v1", api_key_set: true },
  embedding_model: "bge-m3",
} as KnowledgeBase;

// editInitial is a complete knowledge base used by the edit-form cases.
function editInitial(overrides: Partial<KnowledgeBase> = {}): KnowledgeBase {
  return {
    namespace: "team-kb",
    strategy: "recursive",
    chunk_size: 512,
    chunk_overlap: 64,
    embedding: { base_url: "https://api.example.com/v1", api_key_set: false },
    reranker: { base_url: "", api_key_set: false },
    vlm: { base_url: "", api_key_set: false },
    embedding_model: "bge-m3",
    reranker_model: "",
    vlm_model: "",
    vector_size: 1024,
    collection: "team_kb",
    tokenizer: "bge",
    created_at: "2026-11-09T10:00:00Z",
    ...overrides,
  };
}


describe("matchingProfile", () => {
  it("finds the profile with the same kind, model and base URL", () => {
    expect(matchingProfile([profile()], "embedding", "https://api.example.com/v1", "bge-m3"))
      .toMatchObject({ id: "mp-embed" });
  });

  it("ignores a trailing slash in the stored base URL", () => {
    expect(matchingProfile([profile({ base_url: "https://api.example.com/v1/" })],
      "embedding", "https://api.example.com/v1", "bge-m3")).toMatchObject({ id: "mp-embed" });
    expect(matchingProfile([profile()], "embedding", "https://api.example.com/v1/", "bge-m3"))
      .toMatchObject({ id: "mp-embed" });
  });

  it("never matches a different kind, model or host", () => {
    expect(matchingProfile([profile({ kind: "reranker" })], "embedding",
      "https://api.example.com/v1", "bge-m3")).toBeUndefined();
    expect(matchingProfile([profile({ model: "bge-large" })], "embedding",
      "https://api.example.com/v1", "bge-m3")).toBeUndefined();
    expect(matchingProfile([profile()], "embedding",
      "https://other.example.com/v1", "bge-m3")).toBeUndefined();
  });
});

describe("profileNeedsReembed", () => {
  it("does not require a reembed without an edit target or selection", () => {
    expect(profileNeedsReembed(undefined, profile())).toBe(false);
    expect(profileNeedsReembed(initial, undefined)).toBe(false);
  });

  it("keeps the current connection when the selected profile matches it", () => {
    expect(profileNeedsReembed(initial, profile())).toBe(false);
    expect(profileNeedsReembed(initial, profile({ base_url: "https://api.example.com/v1/" })))
      .toBe(false);
  });

  it("requires a reembed for a different embedding connection or model", () => {
    expect(profileNeedsReembed(initial, profile({ base_url: "https://other.example.com/v1" })))
      .toBe(true);
    expect(profileNeedsReembed(initial, profile({ model: "bge-large" }))).toBe(true);
  });

  it("ignores non-embedding profiles", () => {
    expect(profileNeedsReembed(initial, profile({ kind: "reranker" }))).toBe(false);
  });
});

const fetchMock = vi.fn();

// antd needs these browser APIs that jsdom does not implement.
function installAntdBrowserStubs() {
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
}

// mockApi answers the defaults, profile list and the create/update endpoints the
// form talks to. Saved keys and remote model lists are deliberately unsupported:
// the VLM path must never request them.
function mockApi(profiles: ModelProfile[], saved?: KnowledgeBase) {
  fetchMock.mockImplementation(async (path: string, options: RequestInit = {}) => {
    if (path === "/v1/admin/knowledge-bases/defaults") return jsonResponse(defaults);
    if (path === "/v1/admin/model-profiles") return jsonResponse({ model_profiles: profiles });
    const method = options.method || "GET";
    if (path === "/v1/admin/knowledge-bases" && method === "POST")
      return jsonResponse({ ...editInitial(), namespace: "vision-kb" }, 201);
    if (path.startsWith("/v1/admin/knowledge-bases/") && method === "PATCH")
      return jsonResponse({ ...editInitial(), ...saved, ...JSON.parse(String(options.body || "{}")) });
    return jsonResponse({});
  });
}

function requestBody(path: string, method: string) {
  const call = fetchMock.mock.calls.find(([callPath, options]) =>
    callPath === path && (((options as RequestInit | undefined)?.method) || "GET") === method);
  return call ? JSON.parse(String((call[1] as RequestInit).body)) : undefined;
}

function renderForm(options: { initial?: KnowledgeBase; onSaved?: () => void } = {}) {
  const onSaved = options.onSaved ?? vi.fn();
  const onOpenModelProfiles = vi.fn();
  render(createElement(ConfigProvider, null,
    createElement(App, null,
      createElement(KnowledgeBaseForm, {
        initial: options.initial,
        disabled: false,
        active: true,
        onSaved,
        onOpenModelProfiles,
      }))));
  return { onSaved, onOpenModelProfiles };
}

async function waitForAdminDefaults() {
  await waitFor(() => {
    const submit = screen.getByRole("button", { name: /创建知识库|保存配置/ }) as HTMLButtonElement;
    expect(submit.disabled).toBe(false);
  });
}

async function selectProfile(label: string, optionText: RegExp) {
  fireEvent.mouseDown(screen.getByLabelText(label));
  const option = (await screen.findByText(optionText)).closest('[role="option"]');
  expect(option).toBeTruthy();
  fireEvent.click(option!);
}

describe("optional model settings in the knowledge-base form", () => {
  beforeEach(() => {
    fetchMock.mockReset();
    vi.stubGlobal("fetch", fetchMock);
    installAntdBrowserStubs();
    sessionStorage.clear();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    sessionStorage.clear();
  });

  it("keeps unconfigured optional models disabled on the edit form", async () => {
    const initialKb = editInitial();
    mockApi([profile({ api_key_set: false })], initialKb);
    renderForm({ initial: initialKb });
    await waitForAdminDefaults();

    for (const name of ["启用 Reranker", "启用 VLM"]) {
      expect(screen.getByRole("switch", { name }).getAttribute("aria-checked")).toBe("false");
    }
  });

  it("submits the selected VLM connection and model", async () => {
    mockApi([profile({ api_key_set: false }), vlmProfile()]);
    const { onSaved } = renderForm();
    await waitForAdminDefaults();

    fireEvent.change(screen.getByLabelText("知识库标识（Namespace）"), {
      target: { value: "vision-kb" },
    });
    await selectProfile("Embedding 模型", /默认 Embedding · bge-m3/);
    // Reranker 在创建页默认开启，本用例只验证 VLM，先关闭 Reranker。
    fireEvent.click(screen.getByRole("switch", { name: "启用 Reranker" }));
    await selectProfile("VLM 模型", /视觉模型 · qwen2.5-vl-72b/);
    fireEvent.click(screen.getByRole("button", { name: /创建知识库/ }));

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    const body = requestBody("/v1/admin/knowledge-bases", "POST");
    expect(body).toMatchObject({
      namespace: "vision-kb",
      embedding: { base_url: "https://api.example.com/v1" },
      embedding_model: "bge-m3",
      vlm: { base_url: "https://vlm.example.com/v1" },
      vlm_model: "qwen2.5-vl-72b",
    });
    // 无 Key 的视觉模型配置不会读取已保存 Key，也没有远端模型列表可探测。
    expect(fetchMock.mock.calls.some(([path]) => String(path).endsWith("/key"))).toBe(false);
    expect(fetchMock.mock.calls.some(([path]) => String(path).endsWith("/models"))).toBe(false);
  });

  it("enables reranker and VLM by default on the create form", async () => {
    mockApi([profile({ api_key_set: false }), vlmProfile()]);
    renderForm();
    await waitForAdminDefaults();

    for (const name of ["启用 Reranker", "启用 VLM"]) {
      expect(screen.getByRole("switch", { name }).getAttribute("aria-checked")).toBe("true");
    }
  });

  it("restores explicitly disabled models from the create draft", async () => {
    sessionStorage.setItem("rag.knowledge-base-draft", JSON.stringify({
      draft_version: 2,
      reranker_enabled: false,
      vlm_enabled: false,
    }));
    mockApi([profile({ api_key_set: false }), vlmProfile()]);
    renderForm();
    await waitForAdminDefaults();

    for (const name of ["启用 Reranker", "启用 VLM"]) {
      expect(screen.getByRole("switch", { name }).getAttribute("aria-checked")).toBe("false");
    }
  });

  it("keeps the new default-on switches for a draft saved before the default change", async () => {
    // 旧草稿没有 draft_version，其开关状态来自「默认关闭」时代，不能覆盖新默认值。
    sessionStorage.setItem("rag.knowledge-base-draft", JSON.stringify({
      namespace: "legacy-draft",
      reranker_enabled: false,
      vlm_enabled: false,
    }));
    mockApi([profile({ api_key_set: false }), vlmProfile()]);
    renderForm();
    await waitForAdminDefaults();

    expect((screen.getByLabelText("知识库标识（Namespace）") as HTMLInputElement).value)
      .toBe("legacy-draft");
    for (const name of ["启用 Reranker", "启用 VLM"]) {
      expect(screen.getByRole("switch", { name }).getAttribute("aria-checked")).toBe("true");
    }
  });

  it("submits empty optional models while they stay disabled", async () => {
    mockApi([profile({ api_key_set: false })]);
    const { onSaved } = renderForm();
    await waitForAdminDefaults();

    fireEvent.change(screen.getByLabelText("知识库标识（Namespace）"), {
      target: { value: "plain-kb" },
    });
    await selectProfile("Embedding 模型", /默认 Embedding · bge-m3/);
    // 两个可选模型默认开启；本用例验证关闭后提交空值。
    fireEvent.click(screen.getByRole("switch", { name: "启用 Reranker" }));
    fireEvent.click(screen.getByRole("switch", { name: "启用 VLM" }));
    fireEvent.click(screen.getByRole("button", { name: /创建知识库/ }));

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    const body = requestBody("/v1/admin/knowledge-bases", "POST");
    expect(body.vlm_model).toBe("");
    expect(body).not.toHaveProperty("ocr_model");
  });

  it("guides to the model configuration page when no VLM profile exists", async () => {
    mockApi([profile({ api_key_set: false })]);
    const { onOpenModelProfiles } = renderForm();
    await waitForAdminDefaults();

    // VLM 默认开启；没有 VLM 模型时表单直接给出提示。
    expect(screen.getByRole("switch", { name: "启用 VLM" }).getAttribute("aria-checked")).toBe("true");
    expect(await screen.findByText("暂无 VLM 模型")).toBeTruthy();
    expect(screen.getByText(/创建知识库时只能选择已保存的 VLM 模型/)).toBeTruthy();
    // Reranker / VLM 默认开启且都没有模型时，各自都有「前往模型配置」入口。
    const vlmSection = screen.getByRole("region", { name: "VLM 配置" });
    fireEvent.click(within(vlmSection).getByRole("button", { name: "前往模型配置" }));
    expect(onOpenModelProfiles).toHaveBeenCalledTimes(1);
  });

  it("backfills initial.vlm_model on the edit form and PATCHes vlm_model", async () => {
    const initialKb = editInitial({
      vlm: { base_url: "https://vlm.example.com/v1", api_key_set: false },
      vlm_model: "qwen2.5-vl-72b",
    });
    mockApi([profile({ api_key_set: false }), vlmProfile()], initialKb);
    const { onSaved } = renderForm({ initial: initialKb });
    await waitForAdminDefaults();

    expect(screen.getByRole("switch", { name: "启用 VLM" }).getAttribute("aria-checked"))
      .toBe("true");
    expect(await screen.findByText(/视觉模型 · qwen2.5-vl-72b/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /保存配置/ }));

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    const body = requestBody("/v1/admin/knowledge-bases/team-kb", "PATCH");
    expect(body).toMatchObject({
      vlm_model: "qwen2.5-vl-72b",
      reembed: false,
    });
    expect(body).not.toHaveProperty("ocr_model");
  });

  it("keeps the current VLM when no matching profile is listed", async () => {
    const initialKb = editInitial({
      vlm: { base_url: "https://vlm.example.com/v1", api_key_set: false },
      vlm_model: "qwen2.5-vl-72b",
    });
    mockApi([profile({ api_key_set: false })], initialKb);
    const { onSaved } = renderForm({ initial: initialKb });
    await waitForAdminDefaults();

    expect(await screen.findByText(/当前配置 · qwen2.5-vl-72b/)).toBeTruthy();
    expect(screen.getByText("暂无 VLM 模型")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /保存配置/ }));

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    expect(requestBody("/v1/admin/knowledge-bases/team-kb", "PATCH")).toMatchObject({
      vlm_model: "qwen2.5-vl-72b",
      reembed: false,
    });
  });
});
