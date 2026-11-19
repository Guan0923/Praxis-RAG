import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { App as AntApp, ConfigProvider } from "antd";
import { MonitorPage } from "./MonitorPage";
import {
  setCSRFToken,
  type K8sPod,
  type K8sPodView,
  type K8sRestartState,
  type K8sWorkload,
  type K8sWorkloadView,
  type MonitorLogPage,
  type MonitorOverview,
  type Role,
} from "./api";

// This file covers the page-level per-second tick and the administrator-only
// Kubernetes tab. The fake-timer describe drives the interval explicitly; the
// restart describe uses real timers because antd Modal/message transitions are
// easier to observe that way.

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

function overviewFixture(overrides: Partial<MonitorOverview> = {}): MonitorOverview {
  return {
    generated_at: "2026-11-17T12:00:00Z",
    status: "ok",
    ok: true,
    service: {
      name: "praxis-rag",
      version: "9.9.9",
      backend: "go",
      collection: "praxis_chunks",
      started_at: "2026-11-17T00:00:00Z",
      uptime_seconds: 3600,
      upload_mode: "async",
      delivery: "debezium",
      pdf_engine: "vision",
    },
    dependencies: [],
    queues: null,
    pipeline: null,
    library: null,
    activity: null,
    system: null,
    errors: [],
    ...overrides,
  };
}

const runningPod: K8sPod = {
  namespace: "default",
  name: "web-5f6c",
  node: "node-a",
  phase: "Running",
  ready_containers: 1,
  total_containers: 1,
  restarts: 0,
  owner: { kind: "Deployment", name: "web" },
};

function submittedRestart(): K8sRestartState {
  return {
    status: "submitted",
    message: "等待新副本创建",
    started_at: "2026-11-17T12:00:00Z",
    updated_at: "2026-11-17T12:00:01Z",
    updated: 0,
    desired: 3,
    ready: 2,
  };
}

function webWorkload(overrides: Partial<K8sWorkload> = {}): K8sWorkload {
  return {
    kind: "Deployment",
    resource: "deployments",
    namespace: "default",
    name: "web",
    uid: "uid-web",
    status: "运行中",
    desired: 3,
    ready: 2,
    available: 2,
    updated: 1,
    completions: 0,
    succeeded: 0,
    failed: 0,
    active: 0,
    restartable: true,
    pods: [runningPod],
    ...overrides,
  };
}

function apiWorkload(overrides: Partial<K8sWorkload> = {}): K8sWorkload {
  return {
    kind: "Deployment",
    resource: "deployments",
    namespace: "default",
    name: "api",
    uid: "uid-api",
    status: "运行中",
    desired: 2,
    ready: 2,
    available: 2,
    updated: 2,
    completions: 0,
    succeeded: 0,
    failed: 0,
    active: 0,
    restartable: true,
    pods: [],
    ...overrides,
  };
}

function blockedWorkload(): K8sWorkload {
  return {
    kind: "StatefulSet",
    resource: "statefulsets",
    namespace: "data",
    name: "mysql",
    uid: "uid-mysql",
    status: "运行中",
    desired: 1,
    ready: 1,
    available: 1,
    updated: 1,
    completions: 0,
    succeeded: 0,
    failed: 0,
    active: 0,
    restartable: false,
    restart_blocked_reason: "当前 RBAC 未授予 statefulsets 的 patch 权限",
    pods: [],
  };
}

function jobWorkload(): K8sWorkload {
  return {
    kind: "Job",
    resource: "jobs",
    namespace: "batch",
    name: "import",
    uid: "uid-import",
    status: "运行中",
    desired: 0,
    ready: 0,
    available: 0,
    updated: 0,
    completions: 3,
    succeeded: 2,
    failed: 1,
    active: 1,
    restartable: false,
    restart_blocked_reason: "Job 不支持滚动重启",
    pods: [],
  };
}

function workloadsView(workloads: K8sWorkload[] = []): K8sWorkloadView {
  return {
    generated_at: "2026-11-17T12:00:00Z",
    cluster: { name: "kind-praxis", source: "in-cluster" },
    namespaces: ["default", "data", "batch"],
    workloads,
  };
}

function podsView(): K8sPodView {
  return {
    generated_at: "2026-11-17T12:00:00Z",
    namespaces: ["default", "data", "batch"],
    pods: [
      runningPod,
      {
        namespace: "data",
        name: "mysql-0",
        node: "node-b",
        phase: "Pending",
        reason: "ImagePullBackOff",
        ready_containers: 0,
        total_containers: 1,
        restarts: 2,
        owner: { kind: "StatefulSet", name: "mysql" },
      },
    ],
  };
}

function logPageFixture(): MonitorLogPage {
  return {
    generated_at: "2026-11-17T12:00:00Z",
    source: "memory",
    available: true,
    entries: [],
    total: 0,
    has_more: false,
    components: [],
    file: { configured: false, path: "", size_bytes: 0, truncated: false, note: "" },
    notes: [],
  };
}

const fetchMock = vi.fn();
let overviewResponder: (url: string) => Response | Promise<Response>;
let workloadsResponder: (url: string) => Response | Promise<Response>;
let podsResponder: (url: string) => Response | Promise<Response>;
let restartResponder: (url: string) => Response | Promise<Response>;
let overviewCalls: string[];
let workloadCalls: string[];
let podCalls: string[];
let restartCalls: string[];
let logCalls: string[];

function installEnvironment() {
  setCSRFToken("test-csrf");
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
}

function installFetchMock() {
  overviewCalls = [];
  workloadCalls = [];
  podCalls = [];
  restartCalls = [];
  logCalls = [];
  overviewResponder = () => jsonResponse(overviewFixture());
  workloadsResponder = () => jsonResponse(workloadsView([webWorkload(), blockedWorkload()]));
  podsResponder = () => jsonResponse(podsView());
  restartResponder = () => jsonResponse({ restart: submittedRestart() });
  fetchMock.mockReset();
  fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
    const url = String(input);
    // The restart POST path starts with the workloads list path, so it is
    // matched first.
    if (url.endsWith("/restart")) {
      restartCalls.push(url);
      return await restartResponder(url);
    }
    if (url === "/v1/admin/k8s/memory") {
      return jsonResponse({
        generated_at: "2026-11-19T00:00:00Z",
        warnings: ["Pod 用量不可用"],
        nodes: [{ name: "node-a", pressure: "True", capacity: "8Gi", allocatable: "7Gi", usage: "7168Mi" }],
        pods: [{ name: "doc", namespace: "rag", node: "node-a", priority: 0, priority_class: "",
          containers: [{ name: "worker", kind: "应用", request: "512Mi", limit: "2Gi", termination: "OOMKilled" }] }],
      });
    }
    if (url.startsWith("/v1/admin/k8s/workloads")) {
      workloadCalls.push(url);
      return await workloadsResponder(url);
    }
    if (url.startsWith("/v1/admin/k8s/pods")) {
      podCalls.push(url);
      return await podsResponder(url);
    }
    if (url.startsWith("/v1/admin/monitor/logs")) {
      logCalls.push(url);
      return jsonResponse(logPageFixture());
    }
    overviewCalls.push(url);
    return await overviewResponder(url);
  });
}

function renderPage(role?: Role) {
  return render(
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <AntApp>
        <MonitorPage role={role} />
      </AntApp>
    </ConfigProvider>,
  );
}

async function flush() {
  // Drain the promise chain of a fetch (mock -> apiResponse -> api -> panel
  // setState) before asserting: fake timers only control timers, so a few
  // microtask turns are enough and keep the assertions deterministic.
  await act(async () => {
    for (let index = 0; index < 24; index += 1) {
      await Promise.resolve();
    }
  });
}

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

describe("MonitorPage per-second polling", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    installEnvironment();
    installFetchMock();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("loads the overview immediately and does not request it again before the first tick", async () => {
    renderPage();
    await flush();

    expect(overviewCalls).toHaveLength(1);
    expect(screen.getByText("9.9.9")).toBeTruthy();
    expect(screen.getByText(/更新于/)).toBeTruthy();
    // 没有 switch、没有手动刷新按钮。
    expect(screen.queryByRole("switch")).toBeNull();
    expect(screen.queryByRole("button", { name: "刷新监控数据" })).toBeNull();
  });

  it("refreshes the overview on every 1 second tick", async () => {
    renderPage();
    await flush();
    expect(overviewCalls).toHaveLength(1);

    await advance(1000);
    await flush();
    expect(overviewCalls).toHaveLength(2);

    await advance(1000);
    await flush();
    expect(overviewCalls).toHaveLength(3);
  });

  it("skips a tick while the previous overview request is still running", async () => {
    let resolveFirst!: (response: Response) => void;
    const first = new Promise<Response>((resolve) => { resolveFirst = resolve; });
    let requests = 0;
    overviewResponder = () => {
      requests += 1;
      return requests === 1 ? first : jsonResponse(overviewFixture());
    };

    renderPage();
    await flush();
    expect(overviewCalls).toHaveLength(1);

    // 慢请求未结束时，这一 tick 被跳过（不 abort、不堆积）。
    await advance(1000);
    expect(overviewCalls).toHaveLength(1);

    // 请求 resolve 后的下一个 tick 才发新请求。
    await act(async () => { resolveFirst(jsonResponse(overviewFixture())); });
    await flush();
    expect(screen.getByText("9.9.9")).toBeTruthy();

    await advance(1000);
    await flush();
    expect(overviewCalls).toHaveLength(2);
  });

  it("polls only the selected Kubernetes subview", async () => {
    renderPage("admin");
    await flush();
    expect(screen.getByText("9.9.9")).toBeTruthy();

    fireEvent.click(screen.getByRole("tab", { name: "Kubernetes" }));
    await flush();
    expect(workloadCalls).toHaveLength(1);
    expect(podCalls).toHaveLength(0);
    expect(screen.getByText("web")).toBeTruthy();

    fireEvent.click(screen.getByRole("radio", { name: "全部 Pod" }));
    await flush();
    expect(podCalls).toHaveLength(1);
    const workloadsWhileSwitching = workloadCalls.length;

    // 之后每秒只刷新当前子视图（全部 Pod），旧的工作负载子视图不再请求。
    await advance(1000);
    await flush();
    expect(podCalls).toHaveLength(2);
    expect(workloadCalls).toHaveLength(workloadsWhileSwitching);
    expect(screen.getByText("web-5f6c")).toBeTruthy();
  });

  it("stops polling the logs of an inactive tab", async () => {
    renderPage();
    await flush();

    fireEvent.click(screen.getByRole("tab", { name: "MySQL" }));
    await flush();
    const activeLogCalls = logCalls.length;
    expect(activeLogCalls).toBeGreaterThan(0);

    fireEvent.click(screen.getByRole("tab", { name: "概览" }));
    await flush();
    await advance(1000);
    await flush();
    expect(logCalls).toHaveLength(activeLogCalls);
  });

  it("stops polling after unmount", async () => {
    const view = renderPage("admin");
    await flush();
    fireEvent.click(screen.getByRole("tab", { name: "Kubernetes" }));
    await flush();

    const overviewsBefore = overviewCalls.length;
    const workloadsBefore = workloadCalls.length;
    view.unmount();

    await advance(3000);
    expect(overviewCalls).toHaveLength(overviewsBefore);
    expect(workloadCalls).toHaveLength(workloadsBefore);
  });

  it("renders the Kubernetes tab only for administrators", async () => {
    const admin = renderPage("admin");
    await flush();
    const tabs = screen.getAllByRole("tab").map((tab) => (tab.textContent ?? "").trim());
    expect(tabs.slice(0, 2)).toEqual(["概览", "Kubernetes"]);
    admin.unmount();

    renderPage("user");
    await flush();
    expect(screen.queryByRole("tab", { name: "Kubernetes" })).toBeNull();
    cleanup();

    renderPage();
    await flush();
    expect(screen.queryByRole("tab", { name: "Kubernetes" })).toBeNull();
  });

  it("still lets an administrator open the Kubernetes tab when the overview fails", async () => {
    overviewResponder = () => {
      throw new Error(
        "GET /v1/admin/monitor/overview\nHTTP 403：仅管理员可访问",
      );
    };
    renderPage("admin");
    await flush();

    expect(screen.getByText("监控数据加载失败")).toBeTruthy();
    expect(screen.queryByText("关键指标")).toBeNull();
    // 概览失败时标签栏只剩 Kubernetes，且直接显示其内容。
    expect(screen.getAllByRole("tab").map((tab) => (tab.textContent ?? "").trim())).toEqual([
      "Kubernetes",
    ]);
    expect(workloadCalls).toHaveLength(1);
    expect(screen.getByText("web")).toBeTruthy();
    expect(screen.getByText("当前 RBAC 未授予 statefulsets 的 patch 权限")).toBeTruthy();
  });
});

describe("MonitorPage Kubernetes workloads", () => {
  beforeEach(() => {
    installEnvironment();
    installFetchMock();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  function tableRow(name: string) {
    const cells = screen.getAllByText(name).filter((node) => node.closest("tr") !== null);
    expect(cells.length).toBeGreaterThan(0);
    return cells[0].closest("tr") as HTMLElement;
  }

  async function openKubernetes() {
    expect(await screen.findByText("9.9.9")).toBeTruthy();
    fireEvent.click(screen.getByRole("tab", { name: "Kubernetes" }));
    await screen.findByText("web");
  }

  it("shows node pressure and Pod specifications when Pod metrics are missing", async () => {
    renderPage("admin");
    await openKubernetes();
    fireEvent.click(screen.getByText("节点与内存"));
    expect(await screen.findByText("有压力")).toBeTruthy();
    expect(within(tableRow("有压力")).getByText("1.0 GiB")).toBeTruthy();
    expect(screen.getByText("Pod 用量不可用")).toBeTruthy();
    const row = tableRow("rag/doc");
    expect(within(row).getByText("worker：512Mi / 2Gi")).toBeTruthy();
    expect(within(row).getByText("0")).toBeTruthy();
    fireEvent.click(within(row).getByRole("button"));
    expect(await screen.findByText("OOMKilled")).toBeTruthy();
  });

  it("shows kind-specific counters and blocked reasons inline", async () => {
    workloadsResponder = () => jsonResponse(workloadsView([
      webWorkload(),
      jobWorkload(),
      blockedWorkload(),
    ]));
    renderPage("admin");
    await openKubernetes();

    expect(within(tableRow("web")).getByText("2/3")).toBeTruthy();
    expect(within(tableRow("import")).getByText("成功 2/3 · 失败 1 · 运行 1")).toBeTruthy();
    // 不可重启行内直接显示原因（不是 Tooltip）。
    expect(
      within(tableRow("mysql")).getByText("当前 RBAC 未授予 statefulsets 的 patch 权限"),
    ).toBeTruthy();
    expect(within(tableRow("mysql")).queryByRole("button", { name: "滚动重启" })).toBeNull();
  });

  it("expands a workload row to show its nested pods", async () => {
    renderPage("admin");
    await openKubernetes();

    const row = tableRow("web");
    const expand = row.querySelector(".ant-table-row-expand-icon") as HTMLElement;
    expect(expand).toBeTruthy();
    fireEvent.click(expand);

    expect(await screen.findByText("web-5f6c")).toBeTruthy();
    expect(screen.getByText("node-a")).toBeTruthy();
  });

  it("renders rolling and failed restart states from the snapshot", async () => {
    workloadsResponder = () => jsonResponse(workloadsView([
      webWorkload({
        restart: {
          status: "rolling",
          message: "等待新副本就绪",
          started_at: "2026-11-17T12:00:00Z",
          updated_at: "2026-11-17T12:00:05Z",
          updated: 1,
          desired: 3,
          ready: 1,
        },
      }),
      apiWorkload({
        restart: {
          status: "failed",
          message: "rollout 超时",
          started_at: "2026-11-17T12:00:00Z",
          updated_at: "2026-11-17T12:00:09Z",
          updated: 0,
          desired: 2,
          ready: 2,
        },
      }),
    ]));
    renderPage("admin");
    await openKubernetes();

    expect(within(tableRow("web")).getByText("滚动中：已更新 1/3，就绪 1")).toBeTruthy();
    expect(within(tableRow("web")).getByText("等待新副本就绪")).toBeTruthy();
    expect(within(tableRow("api")).getByText("rollout 超时")).toBeTruthy();
    // rolling 期间该行按钮禁用，completed/failed 后的其他行仍可操作。
    const webButton = within(tableRow("web")).getByRole("button", { name: "滚动重启" }) as HTMLButtonElement;
    expect(webButton.disabled).toBe(true);
    const apiButton = within(tableRow("api")).getByRole("button", { name: "滚动重启" }) as HTMLButtonElement;
    expect(apiButton.disabled).toBe(false);
  });

  it("submits the observed worker identity and replica count when scaling down", async () => {
    const worker = webWorkload({ name: "rag-sag-worker", scalable: true, desired: 2 });
    workloadsResponder = () => jsonResponse(workloadsView([webWorkload(), worker]));
    renderPage("admin");
    await openKubernetes();
    expect(within(tableRow("web")).queryByText("调整副本数")).toBeNull();
    fireEvent.click(within(tableRow(worker.name)).getByText("调整副本数"));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByRole("spinbutton"), { target: { value: "1" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "应用" }));
    await waitFor(() => {
      const call = fetchMock.mock.calls.find(([url]) => String(url).endsWith("/rag-sag-worker/scale"));
      expect(call).toBeTruthy();
      expect(JSON.parse(String((call![1] as RequestInit).body))).toEqual({
        uid: worker.uid, current_replicas: 2, replicas: 1,
      });
    });
  });

  it("confirms a rolling restart and keeps the submitted row disabled", async () => {
    let restarted = false;
    workloadsResponder = () => jsonResponse(workloadsView([
      webWorkload(restarted ? { restart: submittedRestart() } : {}),
      apiWorkload(),
      blockedWorkload(),
    ]));
    restartResponder = () => {
      restarted = true;
      return jsonResponse({ restart: submittedRestart() });
    };

    renderPage("admin");
    await openKubernetes();

    const webButton = within(tableRow("web")).getByRole("button", { name: "滚动重启" }) as HTMLButtonElement;
    expect(webButton.disabled).toBe(false);
    fireEvent.click(webButton);

    // 确认框显示资源类型 / 命名空间 / 名称。
    const modal = (await waitFor(() => {
      const node = document.querySelector(".ant-modal") as HTMLElement | null;
      expect(node).toBeTruthy();
      return node;
    })) as HTMLElement;
    expect(within(modal).getByText("资源类型")).toBeTruthy();
    expect(within(modal).getByText("Deployment")).toBeTruthy();
    expect(within(modal).getByText("命名空间")).toBeTruthy();
    expect(within(modal).getByText("default")).toBeTruthy();
    expect(within(modal).getByText("名称")).toBeTruthy();
    expect(within(modal).getByText("web")).toBeTruthy();

    fireEvent.click(within(modal).getByRole("button", { name: "确认重启" }));

    await waitFor(() => expect(restartCalls).toHaveLength(1));
    expect(restartCalls[0]).toBe("/v1/admin/k8s/workloads/deployments/default/web/restart");
    const postCall = fetchMock.mock.calls.find((item) => String(item[0]).endsWith("/restart"))!;
    expect((postCall[1] as RequestInit).method).toBe("POST");
    const headers = new Headers((postCall[1] as RequestInit).headers);
    expect(headers.get("X-CSRF-Token")).toBe("test-csrf");
    // 提交成功的提示与行内状态都显示「已提交」。
    expect((await screen.findAllByText("已提交")).length).toBeGreaterThanOrEqual(1);

    // 下一秒快照带回 submitted：该行按钮禁用，其他行仍可操作。
    await waitFor(() => {
      const button = within(tableRow("web")).getByRole("button", { name: "滚动重启" }) as HTMLButtonElement;
      expect(button.disabled).toBe(true);
    }, { timeout: 4000 });
    const apiButton = within(tableRow("api")).getByRole("button", { name: "滚动重启" }) as HTMLButtonElement;
    expect(apiButton.disabled).toBe(false);
  });
});
