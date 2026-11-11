import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { ApiKeysPage } from "./ApiKeysPage";
import type { Role } from "./api";

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

type Key = {
  id: string;
  name: string;
  prefix: string;
  namespaces: string[];
  enabled: boolean;
  exportable: boolean;
  created_at: string;
  owner_username?: string;
};

type BatchReply = {
  deleted: string[];
  missing: string[];
  failed: { id: string; error: string }[];
};

function key(id: string, name: string, prefix: string, namespaces: string[], extra: Partial<Key> = {}): Key {
  return {
    id,
    name,
    prefix,
    namespaces,
    enabled: true,
    exportable: true,
    created_at: "2026-11-01T10:00:00Z",
    owner_username: "alice",
    ...extra,
  };
}

const baseKeys: Key[] = [
  key("key_1", "生产密钥", "rk_live", ["kb-one"]),
  key("key_2", "测试密钥", "rk_test", ["kb-two"], { enabled: false, exportable: false, owner_username: "bob" }),
  key("key_3", "备份密钥", "rk_backup", ["kb-one"]),
];

const fetchMock = vi.fn();

// installFetch serves GET /v1/admin/api-keys from an in-memory list, applies
// batch-delete results to that list, and supports single-row DELETE.
function installFetch(options: { initial?: Key[]; batch?: (ids: string[], current: Key[]) => BatchReply } = {}) {
  let current = (options.initial ?? baseKeys).map((item) => ({ ...item }));
  fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
    const method = (init.method ?? "GET").toUpperCase();
    if (path === "/v1/admin/api-keys" && method === "GET") return jsonResponse({ keys: current });
    if (path === "/v1/admin/api-keys/batch-delete" && method === "POST") {
      const body = JSON.parse(String(init.body)) as { ids: string[] };
      const present = current.filter((item) => body.ids.includes(item.id));
      const reply: BatchReply = options.batch ? options.batch(body.ids, current) : {
        deleted: present.map((item) => item.id),
        missing: body.ids.filter((id) => !present.some((item) => item.id === id)),
        failed: [],
      };
      current = current.filter((item) => !reply.deleted.includes(item.id) && !reply.missing.includes(item.id));
      return jsonResponse(reply);
    }
    if (String(path).startsWith("/v1/admin/api-keys/") && method === "DELETE") {
      const id = String(path).slice("/v1/admin/api-keys/".length);
      current = current.filter((item) => item.id !== id);
      return jsonResponse({ deleted: id });
    }
    return jsonResponse({ detail: "not found" }, 404);
  });
  return { current: () => current };
}

function renderPage(role: Role = "admin") {
  return render(
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <App>
        <ApiKeysPage namespaces={["kb-one", "kb-two"]} ownedNamespaces={["kb-one"]} role={role} />
      </App>
    </ConfigProvider>,
  );
}

function batchButton() {
  return screen.getByRole("button", { name: /批量删除/ }) as HTMLButtonElement;
}

function checkbox(name: string) {
  return screen.getByRole("checkbox", { name: "选择 " + name }) as HTMLInputElement;
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
  const table = container.querySelector<HTMLElement>(".api-keys-table");
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

describe("ApiKeysPage batch delete", () => {
  it("renders the key list with owner, prefix, namespaces and per-row controls", async () => {
    installFetch();
    renderPage();

    expect(await screen.findByText("生产密钥")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "API Key" })).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "所有者" })).toBeTruthy();
    expect(screen.getAllByText("alice").length).toBe(2);
    expect(screen.getByText("rk_live…")).toBeTruthy();
    expect(screen.getAllByText("kb-one").length).toBe(2);
    expect(screen.getByRole("button", { name: /刷新/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: "删除 生产密钥" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "编辑 生产密钥" })).toBeTruthy();
    expect(screen.getByRole("switch", { name: "启用 生产密钥" })).toBeTruthy();
  });

  it("keeps the batch delete button disabled until a checkbox row is selected", async () => {
    installFetch();
    renderPage();
    await screen.findByText("生产密钥");

    expect(batchButton().disabled).toBe(true);
    fireEvent.click(checkbox("生产密钥"));
    await waitFor(() => expect(batchButton().disabled).toBe(false));
    expect(batchButton().textContent).toContain("1");

    fireEvent.click(checkbox("生产密钥"));
    await waitFor(() => expect(batchButton().disabled).toBe(true));
  });

  it("confirms, posts the selected ids to the batch endpoint and reloads the list", async () => {
    const { current } = installFetch();
    renderPage();
    await screen.findByText("生产密钥");

    fireEvent.click(checkbox("生产密钥"));
    fireEvent.click(checkbox("备份密钥"));
    fireEvent.click(batchButton());

    const dialog = await screen.findByRole("dialog");
    // antd renders the confirm title both visually and for screen readers.
    expect(within(dialog).getAllByText("删除 API Key").length).toBeGreaterThan(0);
    expect(within(dialog).getByText(/删除所选 2 个 API Key 后使用这些密钥的应用将无法继续访问/)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    await waitFor(() => expect(current().map((item) => item.id)).toEqual(["key_2"]));
    const post = fetchMock.mock.calls.find(
      ([path, init]) => path === "/v1/admin/api-keys/batch-delete" && (init as RequestInit | undefined)?.method === "POST",
    );
    expect(post).toBeTruthy();
    expect(JSON.parse(String((post![1] as RequestInit).body))).toEqual({ ids: ["key_1", "key_3"] });

    const listCalls = () => fetchMock.mock.calls.filter(
      ([path, init]) => path === "/v1/admin/api-keys" && ((init as RequestInit | undefined)?.method ?? "GET") === "GET",
    ).length;
    await waitFor(() => expect(listCalls()).toBeGreaterThanOrEqual(2));
    await waitFor(() => expect(screen.queryByText("生产密钥")).toBeNull());
    await waitFor(() => expect(batchButton().textContent).toBe("批量删除"));
    expect(await screen.findByText("已删除 2 个 API Key")).toBeTruthy();
  });

  it("reports already-missing ids as deleted and does not keep them selected", async () => {
    installFetch({
      batch: () => ({ deleted: ["key_1"], missing: ["key_3"], failed: [] }),
    });
    renderPage();
    await screen.findByText("生产密钥");

    fireEvent.click(checkbox("生产密钥"));
    fireEvent.click(checkbox("备份密钥"));
    fireEvent.click(batchButton());
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    expect(await screen.findByText("已删除 2 个 API Key（1 个已不存在）")).toBeTruthy();
    await waitFor(() => expect(screen.queryByText("部分 API Key 删除失败")).toBeNull());
    await waitFor(() => expect(batchButton().textContent).toBe("批量删除"));
    expect(batchButton().disabled).toBe(true);
  });

  it("keeps failed ids selected and lists them in a warning alert until the next reload", async () => {
    installFetch({
      batch: () => ({ deleted: ["key_1"], missing: [], failed: [{ id: "key_2", error: "数据库繁忙" }] }),
    });
    renderPage();
    await screen.findByText("生产密钥");

    fireEvent.click(checkbox("生产密钥"));
    fireEvent.click(checkbox("测试密钥"));
    fireEvent.click(batchButton());
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    expect(await screen.findByText("部分 API Key 删除失败")).toBeTruthy();
    expect(screen.getByText("数据库繁忙")).toBeTruthy();
    expect(screen.getByText("key_2")).toBeTruthy();
    await waitFor(() => expect(checkbox("测试密钥").checked).toBe(true));
    // The successfully deleted key is gone from the reloaded list.
    expect(screen.queryByText("生产密钥")).toBeNull();
    expect(batchButton().textContent).toContain("1");

    // A manual refresh is a reload, so the stale failure alert goes away.
    fireEvent.click(screen.getByRole("button", { name: /刷新/ }));
    await waitFor(() => expect(screen.queryByText("部分 API Key 删除失败")).toBeNull());
  });

  it("selects rows covered by a left-mouse marquee drag", async () => {
    installFetch();
    const { container } = renderPage();
    await screen.findByText("生产密钥");
    const table = stubRows(container);

    fireEvent.mouseDown(table, { button: 0, clientX: 30, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 400, clientY: 100 });
    await waitFor(() => expect(document.querySelector(".drag-band")).toBeTruthy());
    fireEvent.mouseUp(window, { clientX: 400, clientY: 100 });

    await waitFor(() => expect(checkbox("生产密钥").checked).toBe(true));
    expect(checkbox("测试密钥").checked).toBe(true);
    expect(checkbox("备份密钥").checked).toBe(false);
    await waitFor(() => expect(batchButton().textContent).toContain("2"));
    expect(batchButton().disabled).toBe(false);
    await waitFor(() => expect(document.querySelector(".drag-band")).toBeNull());
  });

  it("still deletes a single key from its row action", async () => {
    const { current } = installFetch();
    renderPage();
    await screen.findByText("生产密钥");

    fireEvent.click(screen.getByRole("button", { name: "删除 生产密钥" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/删除后，使用此密钥的应用将无法继续访问：生产密钥/)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    await waitFor(() => expect(current().map((item) => item.id)).toEqual(["key_2", "key_3"]));
    const del = fetchMock.mock.calls.find(
      ([path, init]) => path === "/v1/admin/api-keys/key_1" && (init as RequestInit | undefined)?.method === "DELETE",
    );
    expect(del).toBeTruthy();
    await waitFor(() => expect(screen.queryByText("生产密钥")).toBeNull());
    expect(screen.getByText("测试密钥")).toBeTruthy();
  });
});
