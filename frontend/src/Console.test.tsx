import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { Console } from "./Console";

const fetchMock = vi.fn();

// jsonResponse builds the minimal Response surface the client touches.
function jsonResponse(payload: unknown, status = 200) {
  const text = JSON.stringify(payload);
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "test",
    headers: new Headers(),
    json: async () => JSON.parse(text),
    text: async () => text,
  } as unknown as Response;
}

const user = { id: "u-1", username: "alice", role: "admin", enabled: true };

// mockApi serves the small set of endpoints the console shell needs; unknown
// paths get a 404 so unexpected page mounts show up as failed requests.
function mockApi(namespaces: string[], options: { authEnabled?: boolean; owned?: string[]; user?: typeof user } = {}) {
  const authEnabled = options.authEnabled ?? true;
  const account = options.user ?? user;
  fetchMock.mockImplementation(async (path: string) => {
    if (path === "/v1/auth/status") {
      return jsonResponse({
        auth_enabled: authEnabled,
        registration_enabled: false,
        authenticated: authEnabled,
      });
    }
    if (path === "/v1/auth/me" && authEnabled) return jsonResponse({ user: account, csrf_token: "csrf" });
    if (path === "/v1/invitations") return jsonResponse({ invitations: [] });
    if (path === "/v1/namespaces") {
      return jsonResponse({
        namespaces,
        ...(options.owned ? { owned_namespaces: options.owned } : {}),
      });
    }
    if (path === "/health") return jsonResponse({ backend: "test" });
    if (path === "/v1/knowledge-base") return jsonResponse({ namespace: "kb-b", strategies: [] });
    if (path.endsWith("/access")) {
      return jsonResponse({
        namespace: "kb-b",
        owner: true,
        can_manage: true,
        can_share: true,
        role: "admin",
        can_read: true,
        can_write: true,
        scope: { whole_read: true, whole_write: true, read_documents: [], write_documents: [] },
      });
    }
    if (path.startsWith("/v1/document-batches")) return jsonResponse({ batches: [] });
    return jsonResponse({ detail: "not found" }, 404);
  });
}

function renderConsole() {
  return render(
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <App>
        <Console dark={false} toggleTheme={vi.fn()} />
      </App>
    </ConfigProvider>,
  );
}

function requestedPaths() {
  return fetchMock.mock.calls.map(([path]) => String(path));
}

// namespaceDependentRequests lists every request that would only come from
// mounting SearchPage/DocumentsPage (or document preview) against a workspace.
function namespaceDependentRequests() {
  return requestedPaths().filter((path) =>
    path.startsWith("/v1/search")
    || path.startsWith("/v1/documents")
    || path === "/v1/knowledge-base"
    || path.startsWith("/v1/knowledge-bases/")
    || path.startsWith("/v1/document-batches"));
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
  localStorage.clear();
  sessionStorage.clear();
  window.history.replaceState(null, "", "#/workspaces/search");
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  localStorage.clear();
  sessionStorage.clear();
});

describe("Console workspace selection", () => {
  it("auto-loads namespaces and gates the pages with an empty accessible list", async () => {
    mockApi([]);
    renderConsole();

    await waitFor(() => {
      expect(screen.getAllByText("暂无可访问的知识库").length).toBeGreaterThanOrEqual(2);
    });
    // The list request is namespace-agnostic: it must not carry a remembered
    // X-RAG-Namespace, otherwise a stale selection could block the list.
    const listCall = fetchMock.mock.calls.find(([path]) => path === "/v1/namespaces");
    expect(new Headers((listCall?.[1] as RequestInit | undefined)?.headers).get("X-RAG-Namespace"))
      .toBeNull();

    // The list request is automatic: the dropdown was never opened.
    expect(requestedPaths().filter((path) => path === "/v1/namespaces")).toHaveLength(1);
    expect(namespaceDependentRequests()).toEqual([]);
  });

  it("applies the empty-list gate to legacy no-session mode as well", async () => {
    mockApi([], { authEnabled: false });
    renderConsole();

    await waitFor(() => {
      expect(screen.getAllByText("暂无可访问的知识库").length).toBeGreaterThanOrEqual(2);
    });
    // Legacy mode keeps its historical pages visible while the list is still
    // loading, but once the empty list settles neither SearchPage nor
    // DocumentsPage may stay mounted or issue further workspace requests.
    const callsAtGate = namespaceDependentRequests().length;
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(screen.queryByLabelText("检索调试页面")).toBeNull();
    expect(screen.queryByLabelText("知识文档页面")).toBeNull();
    expect(namespaceDependentRequests().length).toBe(callsAtGate);
  });

  it("recovers a stale legacy API-key namespace from the namespace-agnostic list", async () => {
    // Simulate the old backend: a list request that still carries the stale
    // remembered namespace is rejected, so recovery only works because the
    // Console leaves X-RAG-Namespace off the list request.
    fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
      const headers = new Headers(init.headers);
      if (path === "/v1/auth/status") {
        return jsonResponse({ auth_enabled: false, registration_enabled: false, authenticated: false });
      }
      if (path === "/v1/namespaces") {
        if (headers.get("X-RAG-Namespace"))
          return jsonResponse({ detail: "API Key 未获授权访问此 Namespace" }, 403);
        return jsonResponse({ namespaces: ["kb-b"] });
      }
      if (path === "/health") return jsonResponse({ backend: "test" });
      if (path === "/v1/knowledge-base") return jsonResponse({ namespace: "kb-b", strategies: [] });
      if (path.endsWith("/access")) {
        return jsonResponse({
          namespace: "kb-b",
          owner: true,
          can_manage: true,
          can_share: true,
          role: "admin",
          can_read: true,
          can_write: true,
          scope: { whole_read: true, whole_write: true, read_documents: [], write_documents: [] },
        });
      }
      return jsonResponse({ detail: "not found" }, 404);
    });
    sessionStorage.setItem("rag.token", "prk_legacy");
    localStorage.setItem("rag.namespace", "stale-kb");

    renderConsole();

    // The remembered `stale-kb` is not sent on the list request, so the key's
    // real scope arrives and the Console auto-corrects to it.
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "选择知识库" }).textContent).toContain("kb-b");
    });
    expect(localStorage.getItem("rag.namespace")).toBe("kb-b");
    const listCall = fetchMock.mock.calls.find(([path]) => path === "/v1/namespaces");
    const listHeaders = new Headers((listCall?.[1] as RequestInit | undefined)?.headers);
    expect(listHeaders.get("X-RAG-Namespace")).toBeNull();
    expect(listHeaders.get("Authorization")).toBe("Bearer prk_legacy");
    // Pages follow the recovered namespace.
    await waitFor(() => {
      const headers = fetchMock.mock.calls
        .filter(([path]) => String(path).startsWith("/v1/knowledge-base"))
        .map(([, init]) => new Headers((init as RequestInit | undefined)?.headers).get("X-RAG-Namespace"));
      expect(headers).toContain("kb-b");
    });
  });

  it("keeps the empty-list gate closed while a refresh is in flight", async () => {
    let resolveReload: ((response: Response) => void) | undefined;
    const reloadResponse = new Promise<Response>((resolve) => {
      resolveReload = resolve;
    });
    let namespaceCalls = 0;
    fetchMock.mockImplementation(async (path: string) => {
      if (path === "/v1/auth/status") {
        return jsonResponse({
          auth_enabled: true,
          registration_enabled: false,
          authenticated: true,
        });
      }
      if (path === "/v1/auth/me") return jsonResponse({ user, csrf_token: "csrf" });
      if (path === "/v1/invitations") return jsonResponse({ invitations: [] });
      if (path === "/v1/namespaces") {
        namespaceCalls += 1;
        return namespaceCalls === 1 ? jsonResponse({ namespaces: [] }) : reloadResponse;
      }
      if (path === "/health") return jsonResponse({ backend: "test" });
      return jsonResponse({ detail: "not found" }, 404);
    });

    renderConsole();
    await waitFor(() => {
      expect(screen.getAllByText("暂无可访问的知识库").length).toBeGreaterThanOrEqual(2);
    });
    const before = namespaceDependentRequests().length;

    // Opening the dropdown refreshes the list. While that request is pending the
    // pages must stay unmounted: otherwise the stale route namespace would fire
    // /v1/knowledge-base, /v1/documents, ... requests during the empty state.
    fireEvent.click(screen.getByRole("button", { name: "选择知识库" }));
    await waitFor(() => expect(namespaceCalls).toBe(2));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(namespaceDependentRequests().length).toBe(before);
    expect(screen.queryByLabelText("检索调试页面")).toBeNull();
    expect(screen.queryByLabelText("知识文档页面")).toBeNull();

    await act(async () => {
      resolveReload?.(jsonResponse({ namespaces: [] }));
    });
    await waitFor(() => {
      expect(screen.getAllByText("暂无可访问的知识库").length).toBeGreaterThanOrEqual(2);
    });
  });

  it("restores the per-user selection before mounting the pages", async () => {
    localStorage.setItem("rag.namespace.u-1", "kb-b");
    mockApi(["kb-a", "kb-b"]);
    renderConsole();

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "选择知识库" }).textContent).toContain("kb-b");
    });
    // The namespace-dependent pages mount only after the selection was
    // validated, and then use the restored namespace.
    await waitFor(() => {
      const call = fetchMock.mock.calls.find(([path]) => path === "/v1/knowledge-base");
      expect(call).toBeTruthy();
      const init = call?.[1] as RequestInit | undefined;
      expect(new Headers(init?.headers).get("X-RAG-Namespace")).toBe("kb-b");
    });
    expect(localStorage.getItem("rag.namespace.u-1")).toBe("kb-b");
  });

  it("isolates accounts across logout and login: A's selection never leaks into B's session", async () => {
    type Account = { id: string; username: string; role: string; enabled: boolean };
    const alice: Account = { id: "u-a", username: "alice", role: "admin", enabled: true };
    const bob: Account = { id: "u-b", username: "bob", role: "user", enabled: true };
    let active: Account | null = alice;
    const workspaceFor = (account: Account) =>
      account.id === alice.id ? ["kb-a", "kb-shared"] : ["kb-b"];

    fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
      const method = (init.method || "GET").toUpperCase();
      if (path === "/v1/auth/status") {
        return jsonResponse({
          auth_enabled: true,
          registration_enabled: false,
          authenticated: active !== null,
        });
      }
      if (path === "/v1/auth/me" && active) {
        return jsonResponse({ user: active, csrf_token: "csrf-a" });
      }
      if (path === "/v1/auth/logout" && method === "POST") {
        active = null;
        return jsonResponse({ ok: true });
      }
      if (path === "/v1/auth/login" && method === "POST") {
        active = bob;
        return jsonResponse({ user: bob, csrf_token: "csrf-b" });
      }
      if (path === "/v1/invitations") return jsonResponse({ invitations: [] });
      if (path === "/v1/namespaces") {
        return jsonResponse({ namespaces: active ? workspaceFor(active) : [] });
      }
      if (path === "/health") return jsonResponse({ backend: "test" });
      if (path === "/v1/knowledge-base") return jsonResponse({ namespace: "test", strategies: [] });
      if (path.endsWith("/access")) {
        return jsonResponse({
          namespace: "test",
          owner: false,
          can_manage: false,
          can_share: false,
          role: "user",
          can_read: true,
          can_write: false,
          scope: { whole_read: true, whole_write: false, read_documents: [], write_documents: [] },
        });
      }
      if (path.startsWith("/v1/document-batches")) return jsonResponse({ batches: [] });
      return jsonResponse({ detail: "not found" }, 404);
    });

    // Each account picked a different knowledge base in an earlier session; a
    // logout/login switch must restore only the new account's entry.
    localStorage.setItem("rag.namespace.u-a", "kb-a");
    localStorage.setItem("rag.namespace.u-b", "kb-b");
    renderConsole();

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "选择知识库" }).textContent).toContain("kb-a");
    });

    fireEvent.click(screen.getByRole("button", { name: "账号菜单" }));
    fireEvent.click(await screen.findByText("退出登录"));
    await screen.findByLabelText("用户名");

    // Everything requested from here on belongs to B's session.
    const switchIndex = fetchMock.mock.calls.length;
    fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "bob" } });
    fireEvent.change(screen.getByLabelText("密码"), { target: { value: "password123" } });
    fireEvent.click(screen.getByRole("button", { name: "登录" }));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "选择知识库" }).textContent).toContain("kb-b");
    });
    const workspaceRequestHeaders = () => fetchMock.mock.calls
      .slice(switchIndex)
      .filter(([path]) => {
        const target = String(path);
        return target === "/v1/knowledge-base"
          || target.startsWith("/v1/documents")
          || target.startsWith("/v1/search");
      })
      .map(([, init]) => new Headers((init as RequestInit | undefined)?.headers).get("X-RAG-Namespace"));
    await waitFor(() => expect(workspaceRequestHeaders()).toContain("kb-b"));
    expect(workspaceRequestHeaders()).not.toContain("kb-a");
    // Both per-user entries survive independently.
    expect(localStorage.getItem("rag.namespace.u-a")).toBe("kb-a");
    expect(localStorage.getItem("rag.namespace.u-b")).toBe("kb-b");
  });

  it("offers the create shortcut from the empty state without firing workspace requests", async () => {
    mockApi([]);
    renderConsole();

    const titles = await screen.findAllByText("暂无可访问的知识库");
    const alertTitle = titles.find((node) => node.closest(".ant-alert"));
    expect(alertTitle).toBeTruthy();
    const emptyAlert = alertTitle!.closest(".ant-alert") as HTMLElement;
    const before = namespaceDependentRequests().length;

    fireEvent.click(within(emptyAlert).getByRole("button", { name: "创建知识库" }));

    await waitFor(() => expect(window.location.hash).toBe("#/workspaces/create"));
    // Creating the first knowledge base needs no workspace-scoped requests.
    expect(namespaceDependentRequests().length).toBe(before);
  });

  it("shows the model configuration page to a non-admin account", async () => {
    // 模型配置按账号隔离：普通账号也能打开自己的配置页，不再是管理员专属入口。
    mockApi(["kb-b"], { user: { ...user, role: "user" } });
    window.history.replaceState(null, "", "#/model-profiles");
    renderConsole();

    await screen.findByRole("button", { name: "账号菜单" });
    // 侧边栏入口对所有登录角色可见。
    expect(screen.getByRole("button", { name: "模型配置" })).toBeTruthy();
    // 直接打开该路由时，非管理员也渲染自己的模型配置页（数据请求失败不影响页面标题）。
    expect(await screen.findByRole("heading", { name: "模型配置" })).toBeTruthy();
  });

  it("drops the previous account's document detail route when another account signs in", async () => {
    type Account = { id: string; username: string; role: string; enabled: boolean };
    const alice: Account = { id: "u-a", username: "alice", role: "admin", enabled: true };
    const bob: Account = { id: "u-b", username: "bob", role: "user", enabled: true };
    let active: Account | null = alice;
    const namespaceFor = (account: Account) => account.id === alice.id ? "kb-a" : "kb-b";

    fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
      const method = (init.method || "GET").toUpperCase();
      if (path === "/v1/auth/status") {
        return jsonResponse({
          auth_enabled: true,
          registration_enabled: false,
          authenticated: active !== null,
        });
      }
      if (path === "/v1/auth/me" && active) return jsonResponse({ user: active, csrf_token: "csrf-a" });
      if (path === "/v1/auth/logout" && method === "POST") {
        active = null;
        return jsonResponse({ ok: true });
      }
      if (path === "/v1/auth/login" && method === "POST") {
        active = bob;
        return jsonResponse({ user: bob, csrf_token: "csrf-b" });
      }
      if (path === "/v1/invitations") return jsonResponse({ invitations: [] });
      if (path === "/v1/namespaces") {
        return jsonResponse({ namespaces: active ? [namespaceFor(active)] : [] });
      }
      if (path === "/health") return jsonResponse({ backend: "test" });
      return jsonResponse({ detail: "not found" }, 404);
    });

    localStorage.setItem("rag.namespace.u-a", "kb-a");
    localStorage.setItem("rag.namespace.u-b", "kb-b");
    window.history.replaceState(null, "", "#/workspaces/documents/old-guide.md");
    renderConsole();

    // The own deep link survives the initial namespace correction...
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "选择知识库" }).textContent).toContain("kb-a");
    });
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: "old-guide.md" })).toBeTruthy();
    });

    fireEvent.click(screen.getByRole("button", { name: "账号菜单" }));
    fireEvent.click(await screen.findByText("退出登录"));
    await screen.findByLabelText("用户名");
    fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "bob" } });
    fireEvent.change(screen.getByLabelText("密码"), { target: { value: "password123" } });
    fireEvent.click(screen.getByRole("button", { name: "登录" }));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "选择知识库" }).textContent).toContain("kb-b");
    });
    // ...but a detail belonging to the signed-out account is replaced by B's
    // own document list instead of staying in the hash under B's namespace.
    await waitFor(() => expect(window.location.hash).toBe("#/workspaces/documents"));
    expect(screen.queryByRole("heading", { name: "old-guide.md" })).toBeNull();
  });

  it("drops a signed-out account's late invitation response before it reaches the next bell", async () => {
    type Account = { id: string; username: string; role: string; enabled: boolean };
    const alice: Account = { id: "u-a", username: "alice", role: "admin", enabled: true };
    const bob: Account = { id: "u-b", username: "bob", role: "user", enabled: true };
    let active: Account | null = alice;
    let resolveAliceInbox: ((response: Response) => void) | undefined;
    const aliceInbox = new Promise<Response>((resolve) => {
      resolveAliceInbox = resolve;
    });
    let inboxCalls = 0;
    const aliceInvitation = {
      id: "inv-a",
      namespace: "a-old-kb",
      inviter_user_id: "u-a",
      inviter_username: "alice",
      invitee_user_id: "u-a",
      invitee_username: "alice",
      permission: "read",
      scope: "all",
      document_names: [],
      status: "pending",
      created_at: "2026-01-01T00:00:00Z",
      expires_at: "2030-01-01T00:00:00Z",
      accepted_at: null,
    };

    fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
      const method = (init.method || "GET").toUpperCase();
      if (path === "/v1/auth/status") {
        return jsonResponse({
          auth_enabled: true,
          registration_enabled: false,
          authenticated: active !== null,
        });
      }
      if (path === "/v1/auth/me" && active) return jsonResponse({ user: active, csrf_token: "csrf-a" });
      if (path === "/v1/auth/logout" && method === "POST") {
        active = null;
        return jsonResponse({ ok: true });
      }
      if (path === "/v1/auth/login" && method === "POST") {
        active = bob;
        return jsonResponse({ user: bob, csrf_token: "csrf-b" });
      }
      if (path === "/v1/invitations") {
        inboxCalls += 1;
        return inboxCalls === 1 ? aliceInbox : jsonResponse({ invitations: [] });
      }
      if (path === "/v1/namespaces") {
        return jsonResponse({ namespaces: active?.id === alice.id ? ["kb-a"] : ["kb-b"] });
      }
      if (path === "/health") return jsonResponse({ backend: "test" });
      return jsonResponse({ detail: "not found" }, 404);
    });

    localStorage.setItem("rag.namespace.u-a", "kb-a");
    localStorage.setItem("rag.namespace.u-b", "kb-b");
    renderConsole();

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "选择知识库" }).textContent).toContain("kb-a");
    });
    await waitFor(() => expect(inboxCalls).toBe(1));

    fireEvent.click(screen.getByRole("button", { name: "账号菜单" }));
    fireEvent.click(await screen.findByText("退出登录"));
    await screen.findByLabelText("用户名");
    fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "bob" } });
    fireEvent.change(screen.getByLabelText("密码"), { target: { value: "password123" } });
    fireEvent.click(screen.getByRole("button", { name: "登录" }));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "选择知识库" }).textContent).toContain("kb-b");
    });
    await waitFor(() => expect(inboxCalls).toBeGreaterThanOrEqual(2));
    const bell = screen.getByRole("button", { name: "邀请通知" });
    expect(bell.querySelector(".ant-badge-count")).toBeNull();

    // Alice's response resolves only after Bob signed in: it must be dropped.
    await act(async () => {
      resolveAliceInbox?.(jsonResponse({ invitations: [aliceInvitation] }));
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(bell.querySelector(".ant-badge-count")).toBeNull();
    expect(screen.queryByText("a-old-kb")).toBeNull();
  });

  it("ignores a signed-out account's late namespace response after the next account signed in", async () => {
    type Account = { id: string; username: string; role: string; enabled: boolean };
    const alice: Account = { id: "u-a", username: "alice", role: "admin", enabled: true };
    const bob: Account = { id: "u-b", username: "bob", role: "user", enabled: true };
    let active: Account | null = alice;
    let resolveAliceNamespaces: ((response: Response) => void) | undefined;
    const aliceNamespaces = new Promise<Response>((resolve) => {
      resolveAliceNamespaces = resolve;
    });
    let namespaceCalls = 0;

    fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
      const method = (init.method || "GET").toUpperCase();
      if (path === "/v1/auth/status") {
        return jsonResponse({
          auth_enabled: true,
          registration_enabled: false,
          authenticated: active !== null,
        });
      }
      if (path === "/v1/auth/me" && active) return jsonResponse({ user: active, csrf_token: "csrf-a" });
      if (path === "/v1/auth/logout" && method === "POST") {
        active = null;
        return jsonResponse({ ok: true });
      }
      if (path === "/v1/auth/login" && method === "POST") {
        active = bob;
        return jsonResponse({ user: bob, csrf_token: "csrf-b" });
      }
      if (path === "/v1/invitations") return jsonResponse({ invitations: [] });
      if (path === "/v1/namespaces") {
        namespaceCalls += 1;
        if (namespaceCalls === 1) return aliceNamespaces;
        return jsonResponse({ namespaces: active?.id === bob.id ? ["kb-b"] : [] });
      }
      if (path === "/health") return jsonResponse({ backend: "test" });
      return jsonResponse({ detail: "not found" }, 404);
    });

    localStorage.setItem("rag.namespace.u-b", "kb-b");
    renderConsole();

    // Alice's list is still in flight when the account switches.
    await waitFor(() => expect(namespaceCalls).toBe(1));
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "选择知识库" }).textContent).toContain("加载中");
    });

    fireEvent.click(screen.getByRole("button", { name: "账号菜单" }));
    fireEvent.click(await screen.findByText("退出登录"));
    await screen.findByLabelText("用户名");
    fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "bob" } });
    fireEvent.change(screen.getByLabelText("密码"), { target: { value: "password123" } });
    fireEvent.click(screen.getByRole("button", { name: "登录" }));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "选择知识库" }).textContent).toContain("kb-b");
    });

    // Alice's response lands after Bob is signed in: it must not overwrite the
    // list/selection (the hook epoch guard plus the real AbortController).
    await act(async () => {
      resolveAliceNamespaces?.(jsonResponse({ namespaces: ["kb-a", "kb-b"] }));
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(screen.getByRole("button", { name: "选择知识库" }).textContent).toContain("kb-b");
    expect(screen.getByRole("button", { name: "选择知识库" }).textContent).not.toContain("kb-a");
    expect(localStorage.getItem("rag.namespace.u-b")).toBe("kb-b");
    expect(localStorage.getItem("rag.namespace.u-a")).toBeNull();
    expect(namespaceCalls).toBe(2);
  });
});
