import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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

function ocrProfile(overrides: Partial<ModelProfile> = {}): ModelProfile {
  return {
    id: "mp-ocr",
    kind: "ocr",
    name: "本地 OCR",
    base_url: "",
    model: "easyocr",
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
    ocr_model: "",
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
// the OCR path must never request them.
function mockApi(profiles: ModelProfile[], saved?: KnowledgeBase) {
  fetchMock.mockImplementation(async (path: string, options: RequestInit = {}) => {
    if (path === "/v1/admin/knowledge-bases/defaults") return jsonResponse(defaults);
    if (path === "/v1/admin/model-profiles") return jsonResponse({ model_profiles: profiles });
    const method = options.method || "GET";
    if (path === "/v1/admin/knowledge-bases" && method === "POST")
      return jsonResponse({ ...editInitial(), namespace: "ocr-kb" }, 201);
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

describe("OCR settings in the knowledge-base form", () => {
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

  it("submits the selected OCR engine as ocr_model", async () => {
    mockApi([profile({ api_key_set: false }), ocrProfile()]);
    const { onSaved } = renderForm();
    await waitForAdminDefaults();

    fireEvent.change(screen.getByLabelText("知识库标识（Namespace）"), {
      target: { value: "ocr-kb" },
    });
    await selectProfile("Embedding 模型", /默认 Embedding · bge-m3/);
    fireEvent.click(screen.getByRole("switch", { name: "启用 OCR" }));
    await selectProfile("OCR 模型", /本地 OCR · easyocr/);
    fireEvent.click(screen.getByRole("button", { name: /创建知识库/ }));

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    expect(requestBody("/v1/admin/knowledge-bases", "POST")).toMatchObject({
      namespace: "ocr-kb",
      embedding: { base_url: "https://api.example.com/v1" },
      embedding_model: "bge-m3",
      vlm_model: "",
      ocr_model: "easyocr",
    });
    // OCR 配置没有 Key 可读取，也没有远端模型列表可探测。
    expect(fetchMock.mock.calls.some(([path]) => String(path).endsWith("/key"))).toBe(false);
    expect(fetchMock.mock.calls.some(([path]) => String(path).endsWith("/models"))).toBe(false);
  });

  it("submits an empty ocr_model while OCR stays disabled", async () => {
    mockApi([profile({ api_key_set: false })]);
    const { onSaved } = renderForm();
    await waitForAdminDefaults();

    fireEvent.change(screen.getByLabelText("知识库标识（Namespace）"), {
      target: { value: "plain-kb" },
    });
    await selectProfile("Embedding 模型", /默认 Embedding · bge-m3/);
    fireEvent.click(screen.getByRole("button", { name: /创建知识库/ }));

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    const body = requestBody("/v1/admin/knowledge-bases", "POST");
    expect(body.ocr_model).toBe("");
    expect(body.vlm_model).toBe("");
  });

  it("guides to the model configuration page when no OCR profile exists", async () => {
    mockApi([profile({ api_key_set: false })]);
    const { onOpenModelProfiles } = renderForm();
    await waitForAdminDefaults();

    fireEvent.click(screen.getByRole("switch", { name: "启用 OCR" }));
    expect(await screen.findByText("暂无 OCR 模型")).toBeTruthy();
    expect(screen.getByText(/创建知识库时只能选择已保存的 OCR 模型/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "前往模型配置" }));
    expect(onOpenModelProfiles).toHaveBeenCalledTimes(1);
  });

  it("backfills initial.ocr_model on the edit form and PATCHes ocr_model", async () => {
    const initialKb = editInitial({ ocr_model: "tesseract" });
    mockApi([profile({ api_key_set: false }), ocrProfile({ model: "tesseract" })], initialKb);
    const { onSaved } = renderForm({ initial: initialKb });
    await waitForAdminDefaults();

    expect(screen.getByRole("switch", { name: "启用 OCR" }).getAttribute("aria-checked"))
      .toBe("true");
    expect(await screen.findByText(/本地 OCR · tesseract/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /保存配置/ }));

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    expect(requestBody("/v1/admin/knowledge-bases/team-kb", "PATCH")).toMatchObject({
      ocr_model: "tesseract",
      reembed: false,
    });
    expect(fetchMock.mock.calls.some(([path]) => String(path).endsWith("/key"))).toBe(false);
  });

  it("keeps the current OCR engine when no matching profile is listed", async () => {
    const initialKb = editInitial({ ocr_model: "easyocr" });
    mockApi([profile({ api_key_set: false })], initialKb);
    const { onSaved } = renderForm({ initial: initialKb });
    await waitForAdminDefaults();

    expect(await screen.findByText(/当前配置 · easyocr/)).toBeTruthy();
    expect(screen.getByText("暂无 OCR 模型")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /保存配置/ }));

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    expect(requestBody("/v1/admin/knowledge-bases/team-kb", "PATCH")).toMatchObject({
      ocr_model: "easyocr",
    });
  });
});
