import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ConfigProvider } from "antd";
import { MonitorPage } from "./MonitorPage";
import { formatDuration, type MonitorOverview } from "./api";

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

// overviewFixture mirrors the frozen contract of
// GET /v1/admin/monitor/overview so the test doubles as a contract check.
function overviewFixture(overrides: Partial<MonitorOverview> = {}): MonitorOverview {
  return {
    generated_at: "2026-11-11T14:20:00Z",
    ok: true,
    service: {
      name: "praxis-rag",
      version: "0.4.0",
      backend: "go",
      collection: "praxis_chunks",
      started_at: "2026-11-11T12:00:00Z",
      uptime_seconds: 8400.2,
      upload_mode: "async",
      outbox_bridge: "relay",
      document_worker: "in-process",
      pdf_engine: "docling",
    },
    dependencies: [
      { name: "mysql", label: "MySQL", status: "ok", latency_ms: 1.2, detail: "连接正常 pool open=1 in_use=0 idle=1 wait=0" },
      { name: "redis", label: "Redis", status: "ok", latency_ms: 0.4, detail: "PING 成功" },
      { name: "qdrant", label: "Qdrant", status: "ok", latency_ms: 5.1, detail: "3 个集合，目标集合存在", points: 1234 },
      { name: "kafka", label: "Kafka", status: "disabled", latency_ms: 0, detail: "未配置 Kafka" },
      { name: "pdf-service", label: "PDF 解析服务", status: "disabled", latency_ms: 0, detail: "Go 内置解析引擎" },
    ],
    queues: {
      document_jobs: {
        by_status: { queued: 0, processing: 1, succeeded: 10, failed: 0 },
        by_stage: { parsing: 1 },
        oldest_queued_seconds: 12.5,
        stuck_processing: 0,
        failed_recent: 0,
      },
      document_outbox: {
        // outbox 的 status 在状态库中存大写字面量，页面必须仍能本地化。
        by_status: { PENDING: 2, PUBLISHED: 5 },
        oldest_pending_seconds: 0,
      },
      search_tasks: {
        by_status: { queued: 0, processing: 0, succeeded: 3, failed: 0, timed_out: 0 },
        oldest_active_seconds: 0,
      },
      uploads: { unfinished: 0 },
    },
    library: { documents: 12, knowledge_bases: 2, result_snapshots: 0, artifact_snapshots: 3 },
    activity: { window_hours: 24, jobs_succeeded: 3, jobs_failed: 0, searches: 12 },
    errors: [],
    ...overrides,
  };
}

function renderPage() {
  return render(
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <MonitorPage />
    </ConfigProvider>,
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

describe("MonitorPage", () => {
  it("renders the service facts, dependency states and queue counts", async () => {
    fetchMock.mockResolvedValue(jsonResponse(overviewFixture()));
    renderPage();

    expect(await screen.findByRole("heading", { name: "系统监控" })).toBeTruthy();
    expect(await screen.findByText("0.4.0")).toBeTruthy();
    expect(screen.getByText("运行正常")).toBeTruthy();
    expect(screen.getByText(/更新于/)).toBeTruthy();
    // 服务信息：运行时长按中文时长渲染，后端/集合/上传方式都在。
    expect(screen.getByText("2 小时 20 分")).toBeTruthy();
    expect(screen.getByText("go")).toBeTruthy();
    expect(screen.getByText("praxis_chunks")).toBeTruthy();
    expect(screen.getByText("async")).toBeTruthy();
    expect(screen.getByText("docling")).toBeTruthy();

    // 依赖表：MySQL 行显示正常与延迟，禁用项不显示无意义的 0ms。
    const mysqlRow = screen.getByText("MySQL").closest("tr");
    expect(mysqlRow).toBeTruthy();
    expect(within(mysqlRow as HTMLElement).getByText("正常")).toBeTruthy();
    expect(within(mysqlRow as HTMLElement).getByText("1.2 ms")).toBeTruthy();
    const kafkaRow = screen.getByText("Kafka").closest("tr");
    expect(within(kafkaRow as HTMLElement).getByText("未启用")).toBeTruthy();
    expect(within(kafkaRow as HTMLElement).getByText("—")).toBeTruthy();
    expect(screen.getByText(/向量点 1,234/)).toBeTruthy();

    // 队列计数绑定到各自的区域，避免和检索任务同名状态混淆。
    const jobsBlock = screen.getByLabelText("文档任务队列");
    expect(within(jobsBlock).getByText("排队中 0")).toBeTruthy();
    expect(within(jobsBlock).getByText("处理中 1")).toBeTruthy();
    expect(within(jobsBlock).getByText("12 秒")).toBeTruthy();
    expect(within(jobsBlock).getByText("解析中")).toBeTruthy();
    const outboxBlock = screen.getByLabelText("文档 Outbox 队列");
    expect(within(outboxBlock).getByText("待投递 2")).toBeTruthy();
    expect(within(outboxBlock).getByText("已投递 5")).toBeTruthy();

    // 资料库与活动统计。
    const libraryCard = screen.getByText("资料库").closest(".ant-card") as HTMLElement;
    expect(within(libraryCard).getByText("12")).toBeTruthy();
    expect(within(libraryCard).getByText("Artifact 快照")).toBeTruthy();
    const activityCard = screen.getByText("活动（最近 24 小时）").closest(".ant-card") as HTMLElement;
    expect(within(activityCard).getByText("检索次数")).toBeTruthy();
    expect(within(activityCard).getByText("3")).toBeTruthy();

    // 首次请求直接命中聚合接口且绕过缓存。
    expect(fetchMock.mock.calls[0][0]).toBe("/v1/admin/monitor/overview");
    const requestOptions = fetchMock.mock.calls[0][1] as RequestInit;
    expect(requestOptions.cache).toBe("no-store");
    expect(requestOptions.signal).toBeInstanceOf(AbortSignal);
  });

  it("shows an error dependency and the backend diagnostics when ok is false", async () => {
    const fixture = overviewFixture({
      ok: false,
      errors: ["Qdrant 连接失败：connection refused"],
      dependencies: overviewFixture().dependencies.map((dependency) =>
        dependency.name === "qdrant"
          ? { ...dependency, status: "error" as const, detail: "连接失败" }
          : dependency,
      ),
    });
    fetchMock.mockResolvedValue(jsonResponse(fixture));
    renderPage();

    expect(await screen.findByText("服务状态异常")).toBeTruthy();
    expect(screen.getByText("存在异常")).toBeTruthy();
    expect(screen.getByText("Qdrant 连接失败：connection refused")).toBeTruthy();
    const qdrantRow = screen.getByText("Qdrant").closest("tr");
    expect(within(qdrantRow as HTMLElement).getByText("异常")).toBeTruthy();
  });

  it("shows the load failure and retries with a fresh snapshot", async () => {
    fetchMock.mockRejectedValueOnce(
      new Error("GET /v1/admin/monitor/overview\nHTTP 403：仅管理员可访问"),
    );
    renderPage();

    expect(await screen.findByText("监控数据加载失败")).toBeTruthy();
    expect(screen.getByText(/HTTP 403/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "重试" })).toBeTruthy();
    // 首次失败没有快照：不显示任何统计卡片，只保留错误与重试入口。
    expect(screen.queryByText("服务信息")).toBeNull();

    fetchMock.mockResolvedValue(jsonResponse(overviewFixture()));
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByText("0.4.0")).toBeTruthy();
    await waitFor(() => expect(screen.queryByText("监控数据加载失败")).toBeNull());
  });

  it("degrades to placeholders when the state database snapshot is unavailable", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(overviewFixture({ queues: null, library: null, activity: null })),
    );
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    expect(screen.getByText("运行正常")).toBeTruthy();
    expect(screen.getAllByText("状态库暂不可用")).toHaveLength(3);
  });

  it("starts with auto refresh on and refreshes on demand", async () => {
    fetchMock.mockResolvedValue(jsonResponse(overviewFixture()));
    renderPage();
    await screen.findByText("0.4.0");

    const toggle = screen.getByRole("switch");
    expect(toggle.getAttribute("aria-checked")).toBe("true");

    // 刷新按钮在首轮请求结束前处于 loading，antd 会忽略点击：等它恢复再点。
    const refreshButton = screen.getByRole("button", { name: /刷新/ });
    await waitFor(() => expect(refreshButton.className).not.toContain("ant-btn-loading"));
    const before = fetchMock.mock.calls.length;
    fireEvent.click(refreshButton);
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(before));

    fireEvent.click(toggle);
    await waitFor(() => expect(toggle.getAttribute("aria-checked")).toBe("false"));
  });
});

describe("formatDuration", () => {
  it("formats uptimes and queue ages in Chinese without raw seconds", () => {
    expect(formatDuration(0)).toBe("0 秒");
    expect(formatDuration(12.5)).toBe("12 秒");
    expect(formatDuration(90)).toBe("1 分 30 秒");
    expect(formatDuration(8400.2)).toBe("2 小时 20 分");
    expect(formatDuration(90061)).toBe("1 天 1 小时 1 分");
    expect(formatDuration(undefined)).toBe("—");
    expect(formatDuration(Number.NaN)).toBe("—");
    expect(formatDuration(-1)).toBe("—");
  });
});
