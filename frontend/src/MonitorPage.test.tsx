import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ConfigProvider } from "antd";
import { MonitorPage } from "./MonitorPage";
import {
  formatDuration,
  type MonitorLogEntry,
  type MonitorLogPage,
  type MonitorOverview,
} from "./api";

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
let overviewPayload: () => Response;
let logsResponder: (url: string) => MonitorLogPage;
let logUrls: string[];

// overviewFixture mirrors the frozen contract of
// GET /v1/admin/monitor/overview so the test doubles as a contract check.
function overviewFixture(overrides: Partial<MonitorOverview> = {}): MonitorOverview {
  return {
    generated_at: "2026-11-11T14:20:00Z",
    status: "ok",
    ok: true,
    service: {
      name: "praxis-rag",
      version: "0.4.0",
      backend: "go",
      collection: "praxis_chunks",
      started_at: "2026-11-11T12:00:00Z",
      uptime_seconds: 8400.2,
      upload_mode: "async",
      delivery: "debezium",
      pdf_engine: "vision",
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
        by_status: { queued: 2, processing: 1, succeeded: 10, failed: 0 },
        by_stage: { parsing: 1, chunking: 2 },
        oldest_queued_seconds: 12.5,
        stuck_processing: 1,
        failed_recent: 2,
      },
      // 上传统计的是“操作”而不是正在上传的文件：四个互斥分类 + 兼容用的合计。
      uploads: {
        unfinished: 5,
        by_status: { IN_PROGRESS: 2, FAILED: 1, ROLLING_BACK: 1, CLEANUP_PENDING: 1 },
        in_progress: 2,
        failed: 1,
        rolling_back: 1,
        cleanup_pending: 1,
      },
    },
    // pipeline 把视图分开：任务/缓存两个 Debezium 连接器、两组 Kafka 消费、
    // 缓存水位、文档 Worker 心跳与文档任务状态。capture_progress 与 offsets
    // 为 null 时必须显示“未知”。
    pipeline: {
      document_jobs: { status: "ok", detail: "文档任务正常，最近成功 10 个" },
      worker: { status: "ok", detail: "最近心跳 3.0 秒前，1 个实例运行中" },
      debezium: {
        tasks: { status: "unknown", detail: "任务连接器未读健康端点", capture_progress: null },
        cache: { status: "ok", detail: "缓存连接器运行中", capture_progress: null },
      },
      kafka: {
        status: "unknown",
        detail: "未取得消费位点",
        // 兼容镜像字段：页面读取 groups，两组各自可读。
        topic: "praxis.rag.document_jobs",
        group: "rag-worker",
        committed_offset: null,
        end_offset: null,
        consumer_lag: null,
        groups: {
          document: {
            status: "unknown",
            detail: "文档组未取得消费位点",
            topic: "praxis.rag.document_jobs",
            group: "rag-document-workers",
            committed_offset: null,
            end_offset: null,
            consumer_lag: null,
          },
          cache: {
            status: "unknown",
            detail: "缓存组未取得消费位点",
            topic: "praxis.rag.cache",
            group: "rag-cache-workers",
            committed_offset: null,
            end_offset: null,
            consumer_lag: null,
          },
        },
      },
      cache: { status: "degraded", dirty_keys: 7, refresh_failures: 2 },
    },
    library: { documents: 12, knowledge_bases: 2, artifact_snapshots: 3 },
    activity: { window_hours: 24, jobs_succeeded: 3, jobs_failed: 0, searches: 12 },
    // system 是宿主机资源快照：CPU / 内存 / 网络 / 进程。首次采样为 null 时
    // 页面显示“采样中/未知”，Windows 没有 load average 时显示“未知”。
    system: {
      collected_at: "2026-11-11T14:19:58Z",
      window_seconds: 1,
      notes: [],
      cpu: {
        usage_percent: 37.5,
        cores: 8,
        model: "AMD Ryzen 7",
        load1: 0.5,
        load5: 0.4,
        load15: 0.3,
      },
      memory: {
        total_bytes: 16 * 1024 ** 3,
        used_bytes: 8 * 1024 ** 3,
        available_bytes: 8 * 1024 ** 3,
        used_percent: 50,
        swap_total_bytes: 2 * 1024 ** 3,
        swap_used_bytes: 0,
        swap_used_percent: 0,
      },
      network: {
        rx_bytes_per_second: 12.3 * 1024 * 1024,
        tx_bytes_per_second: 1.5 * 1024 * 1024,
        rx_bytes_total: 10 * 1024 ** 3,
        tx_bytes_total: 2 * 1024 ** 3,
        interfaces: [
          {
            name: "eth0",
            rx_bytes_per_second: 12.3 * 1024 * 1024,
            tx_bytes_per_second: 1.5 * 1024 * 1024,
            rx_bytes_total: 10 * 1024 ** 3,
            tx_bytes_total: 2 * 1024 ** 3,
          },
        ],
      },
      process: {
        pid: 4321,
        rss_bytes: 256 * 1024 * 1024,
        vms_bytes: 1024 * 1024 * 1024,
        goroutines: 42,
        heap_alloc_bytes: 64 * 1024 * 1024,
      },
    },
    errors: [],
    ...overrides,
  };
}

// logEntries are the frozen response rows of GET /v1/admin/monitor/logs.
// source 是调用方代码位置（仓库相对 file:line），不是 memory/file 日志来源。
const logEntries: MonitorLogEntry[] = [
  {
    seq: 3,
    time: "2026-11-14T06:00:02Z",
    level: "warn",
    component: "mysql",
    component_label: "MySQL",
    message: "MySQL 连接池等待",
    attrs: { wait: "3" },
    source: "internal/state/store.go:431",
  },
  {
    seq: 2,
    time: "2026-11-14T06:00:01Z",
    level: "info",
    component: "mysql",
    component_label: "MySQL",
    message: "文档任务入队",
    attrs: {},
    source: "internal/ingest/jobs.go:88",
  },
  {
    seq: 1,
    time: "2026-11-14T06:00:00Z",
    level: "error",
    component: "redis",
    component_label: "Redis",
    message: "缓存刷新失败",
    attrs: { key: "kb:1" },
    source: "internal/cache/refresher.go:57",
  },
  {
    seq: 4,
    time: "2026-11-14T05:59:59Z",
    level: "info",
    component: "worker",
    component_label: "文档 Worker",
    message: "worker 心跳写入",
    attrs: {},
    // 空代码位置在表格中退化为 “—”。
    source: "",
  },
];

function logPageFixture(overrides: Partial<MonitorLogPage> = {}): MonitorLogPage {
  return {
    generated_at: "2026-11-14T06:00:03Z",
    source: "memory",
    available: true,
    entries: logEntries,
    total: logEntries.length,
    has_more: false,
    components: [
      { id: "mysql", label: "MySQL", count: 2 },
      { id: "redis", label: "Redis", count: 1 },
      { id: "worker", label: "文档 Worker", count: 1 },
    ],
    file: {
      configured: false,
      path: "",
      size_bytes: 0,
      truncated: false,
      note: "",
    },
    notes: [],
    ...overrides,
  };
}

// defaultLogsResponder answers the log endpoint through its own query so the
// file-source switch sees a configured file.
function defaultLogsResponder(url: string): MonitorLogPage {
  const source = new URL(url, "http://localhost").searchParams.get("source") ?? "memory";
  if (source === "file") {
    return logPageFixture({
      source: "file",
      file: {
        configured: true,
        path: "/var/log/praxis-rag.log",
        size_bytes: 4096,
        truncated: false,
        note: "",
      },
    });
  }
  return logPageFixture();
}

function renderPage() {
  return render(
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <MonitorPage />
    </ConfigProvider>,
  );
}

// statisticValue reads one Statistic's rendered number by its title, avoiding
// ambiguity when different cards show the same count.
function statisticValue(scope: HTMLElement, title: string) {
  const statistic = within(scope).getByText(title).closest(".ant-statistic") as HTMLElement;
  return (statistic.querySelector(".ant-statistic-content")?.textContent ?? "").trim();
}

// cardTitles lists the card headings in DOM order for the active tab.
function cardTitles(scope: HTMLElement) {
  return Array.from(scope.querySelectorAll(".ant-card-head-title")).map(
    (node) => (node.textContent ?? "").trim(),
  );
}

// logParams parses the query of one captured log request.
function logParams(url: string) {
  return new URL(url, "http://localhost").searchParams;
}

function lastLogUrl() {
  return logUrls[logUrls.length - 1] ?? "";
}

// openTab clicks a top-bar tab and waits for its panel to mount.
async function openTab(name: string) {
  fireEvent.click(screen.getByRole("tab", { name }));
}

// selectOption opens one antd Select and clicks the matching option. The
// dropdown is portalled to body, so options are queried from the document.
// aria-label lands on the Select wrapper; the dropdown opens on mousedown of
// the inner selector, so dispatch there explicitly.
async function selectOption(label: string, optionText: string) {
  const wrapper = screen.getByLabelText(label).closest(".ant-select") as HTMLElement;
  const trigger = (wrapper.querySelector(".ant-select-selector") ?? wrapper) as HTMLElement;
  fireEvent.mouseDown(trigger);
  const options = await screen.findAllByRole("option");
  // antd renders the option label into aria-label; textContent may hold only the
  // raw value, so match both.
  const option = options.find((node) => {
    const text = `${node.getAttribute("aria-label") ?? ""} ${node.textContent ?? ""}`;
    return text.includes(optionText);
  });
  expect(option).toBeTruthy();
  fireEvent.click(option!);
}

// logCard resolves the Card that owns one log panel section, so the header
// refresh button (outside the aria-labelled section) can be queried too.
function logCard(panelLabel: string) {
  return screen.getByLabelText(panelLabel).closest(".ant-card") as HTMLElement;
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
  overviewPayload = () => jsonResponse(overviewFixture());
  logsResponder = defaultLogsResponder;
  logUrls = [];
  fetchMock.mockImplementation(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.startsWith("/v1/admin/monitor/logs")) {
      logUrls.push(url);
      return jsonResponse(logsResponder(url));
    }
    return overviewPayload();
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("MonitorPage", () => {
  it("renders the nine top-bar tabs and the overview cards", async () => {
    renderPage();

    expect(await screen.findByRole("heading", { name: "系统监控" })).toBeTruthy();
    expect(await screen.findByText("0.4.0")).toBeTruthy();
    expect(screen.getByText("运行正常")).toBeTruthy();
    expect(screen.getByText(/更新于/)).toBeTruthy();

    // 九标签 top bar：概览 + 七个组件 + 全部日志，顺序固定。
    expect(screen.getAllByRole("tab").map((tab) => (tab.textContent ?? "").trim())).toEqual([
      "概览",
      "MySQL",
      "Redis",
      "Qdrant",
      "Kafka",
      "PDF 解析服务",
      "文档 Worker",
      "Debezium",
      "全部日志",
    ]);

    // 概览版式：状态告警（正常时不渲染）+ 关键指标 + 系统资源 + 服务信息。
    const page = screen.getByLabelText("系统监控页面") as HTMLElement;
    expect(cardTitles(page)).toEqual(["关键指标", "系统资源", "服务信息"]);

    // 关键指标：五个任务指标都以 Statistic 渲染，数据来自 queues/library/activity。
    const metricsCard = screen.getByText("关键指标").closest(".ant-card") as HTMLElement;
    expect(statisticValue(metricsCard, "有效文档")).toBe("12");
    expect(statisticValue(metricsCard, "排队文档任务")).toBe("2");
    expect(statisticValue(metricsCard, "处理中文档任务")).toBe("1");
    expect(statisticValue(metricsCard, "上传进行中")).toBe("2");
    expect(statisticValue(metricsCard, "近 24 小时检索次数")).toBe("12");

    // 系统资源：CPU 使用率、内存百分比、网络速率与进程 RSS 都来自 system。
    const systemCard = screen.getByText("系统资源").closest(".ant-card") as HTMLElement;
    expect(within(systemCard).getByText("37.5%")).toBeTruthy();
    expect(within(systemCard).getByText("8 核")).toBeTruthy();
    expect(within(systemCard).getByText("AMD Ryzen 7")).toBeTruthy();
    expect(within(systemCard).getByText("50.0%")).toBeTruthy();
    expect(within(systemCard).getByText("8.0 GB / 16.0 GB")).toBeTruthy();
    // 网络速率在汇总行与网卡 Tag 里各出现一次。
    expect(within(systemCard).getAllByText("12.3 MB/s").length).toBeGreaterThanOrEqual(1);
    expect(within(systemCard).getAllByText("1.5 MB/s").length).toBeGreaterThanOrEqual(1);
    expect(within(systemCard).getByText("10.0 GB")).toBeTruthy();
    expect(within(systemCard).getByText("4321")).toBeTruthy();
    expect(within(systemCard).getByText("256.0 MB")).toBeTruthy();
    expect(within(systemCard).getByText("42")).toBeTruthy();
    expect(within(systemCard).getByText(/1\/5\/15 分钟：0\.50 \/ 0\.40 \/ 0\.30/)).toBeTruthy();

    // 服务信息仍然完整可读。
    const serviceCard = screen.getByText("服务信息").closest(".ant-card") as HTMLElement;
    expect(within(serviceCard).getByText("2 小时 20 分")).toBeTruthy();
    expect(within(serviceCard).getByText("go")).toBeTruthy();
    expect(within(serviceCard).getByText("praxis_chunks")).toBeTruthy();
    expect(within(serviceCard).getByText("async")).toBeTruthy();
    expect(within(serviceCard).getByText("debezium")).toBeTruthy();
    expect(within(serviceCard).getByText("vision")).toBeTruthy();

    // 首次请求直接命中聚合接口且绕过缓存。
    expect(fetchMock.mock.calls[0][0]).toBe("/v1/admin/monitor/overview");
    const requestOptions = fetchMock.mock.calls[0][1] as RequestInit;
    expect(requestOptions.cache).toBe("no-store");
    expect(requestOptions.signal).toBeInstanceOf(AbortSignal);
    // 组件日志面板只在切换到对应标签后才请求。
    expect(logUrls).toHaveLength(0);
  });

  it("renders unknown system resources when the collector is unavailable", async () => {
    overviewPayload = () => jsonResponse(overviewFixture({ system: null }));
    renderPage();

    expect(await screen.findByText("系统资源：未知（采集不可用）")).toBeTruthy();
    // 快照其余部分不受影响。
    expect(screen.getByText("关键指标")).toBeTruthy();
    expect(screen.getByText("0.4.0")).toBeTruthy();
  });

  it("renders 采样中/未知 and 未知 for unsampled CPU values and Windows load averages", async () => {
    const fixture = overviewFixture();
    fixture.system!.cpu.usage_percent = null;
    fixture.system!.cpu.load1 = null;
    fixture.system!.cpu.load5 = null;
    fixture.system!.cpu.load15 = null;
    fixture.system!.network.rx_bytes_per_second = null;
    fixture.system!.network.tx_bytes_per_second = null;
    overviewPayload = () => jsonResponse(fixture);
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    const systemCard = screen.getByText("系统资源").closest(".ant-card") as HTMLElement;
    expect(within(systemCard).getByText("采样中/未知")).toBeTruthy();
    // 负载、网络速率等未采样值统一显示“未知”，不会伪造成 0。
    expect(within(systemCard).getAllByText("未知").length).toBeGreaterThanOrEqual(3);
  });

  it("shows 存在降级 and summarizes the degraded dependencies", async () => {
    const fixture = overviewFixture({
      status: "degraded",
      ok: false,
      dependencies: overviewFixture().dependencies.map((dependency) =>
        dependency.name === "qdrant"
          ? { ...dependency, status: "degraded" as const, detail: "目标集合不存在" }
          : dependency,
      ),
    });
    overviewPayload = () => jsonResponse(fixture);
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    expect(screen.getByText("存在降级")).toBeTruthy();
    expect(screen.queryByText("存在异常")).toBeNull();
    expect(screen.getByText("服务存在降级")).toBeTruthy();
    // 降级依赖自动汇总为 label + detail，而不是一句泛泛的警告。
    expect(screen.getByText("Qdrant：目标集合不存在")).toBeTruthy();
  });

  it("shows the backend diagnostics when status is error", async () => {
    const fixture = overviewFixture({
      status: "error",
      ok: false,
      errors: ["Qdrant 连接失败：connection refused"],
      dependencies: overviewFixture().dependencies.map((dependency) =>
        dependency.name === "qdrant"
          ? { ...dependency, status: "error" as const, detail: "连接失败" }
          : dependency,
      ),
    });
    overviewPayload = () => jsonResponse(fixture);
    renderPage();

    expect(await screen.findByText("服务状态异常")).toBeTruthy();
    expect(screen.getByText("存在异常")).toBeTruthy();
    expect(screen.getByText("Qdrant 连接失败：connection refused")).toBeTruthy();
  });

  it("falls back to the ok boolean when the status field is missing", async () => {
    overviewPayload = () => jsonResponse(overviewFixture({ status: undefined, ok: true }));
    const first = renderPage();
    expect(await screen.findByText("运行正常")).toBeTruthy();
    first.unmount();

    overviewPayload = () => jsonResponse(
      overviewFixture({ status: undefined, ok: false, errors: ["监控统计不可用：timeout"] }),
    );
    renderPage();
    expect(await screen.findByText("存在异常")).toBeTruthy();
    expect(screen.getByText("监控统计不可用：timeout")).toBeTruthy();
  });

  it("shows the load failure and retries with a fresh snapshot", async () => {
    fetchMock.mockImplementationOnce(async () => {
      throw new Error("GET /v1/admin/monitor/overview\\nHTTP 403：仅管理员可访问");
    });
    renderPage();

    expect(await screen.findByText("监控数据加载失败")).toBeTruthy();
    expect(screen.getByText(/HTTP 403/)).toBeTruthy();
    // 首次失败没有快照：不渲染任何标签页或统计卡片，只保留错误与重试入口。
    expect(screen.queryByRole("tab")).toBeNull();
    expect(screen.queryByText("关键指标")).toBeNull();
    expect(screen.queryByText("服务信息")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByText("0.4.0")).toBeTruthy();
    await waitFor(() => expect(screen.queryByText("监控数据加载失败")).toBeNull());
  });

  it("renders 未知 for every metric when the state database snapshot is unavailable", async () => {
    overviewPayload = () => jsonResponse(
      overviewFixture({ queues: null, library: null, activity: null }),
    );
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    expect(screen.getByText("运行正常")).toBeTruthy();

    const metricsCard = screen.getByText("关键指标").closest(".ant-card") as HTMLElement;
    expect(statisticValue(metricsCard, "有效文档")).toBe("未知");
    expect(statisticValue(metricsCard, "排队文档任务")).toBe("未知");
    expect(statisticValue(metricsCard, "处理中文档任务")).toBe("未知");
    expect(statisticValue(metricsCard, "上传进行中")).toBe("未知");
    expect(statisticValue(metricsCard, "近 24 小时检索次数")).toBe("未知");

    // MySQL 标签：队列统计不可用时，文档任务明细降级为“未知”，阶段分布提示状态库不可用。
    await openTab("MySQL");
    const jobsBlock = await screen.findByLabelText("文档任务队列");
    expect(within(jobsBlock).getAllByText("未知")).toHaveLength(3);
    expect(within(jobsBlock).getByText("状态库暂不可用")).toBeTruthy();

    const uploadsBlock = screen.getByLabelText("上传队列");
    expect(within(uploadsBlock).getByText("上传操作：未知")).toBeTruthy();

    const libraryBlock = screen.getByLabelText("库与活动");
    expect(within(libraryBlock).getAllByText("未知").length).toBeGreaterThanOrEqual(4);

    // Redis 标签：缓存水位独立于状态库队列，仍可读。
    await openTab("Redis");
    const cacheBlock = await screen.findByLabelText("缓存水位");
    expect(within(cacheBlock).getByText("7")).toBeTruthy();
    expect(within(cacheBlock).getByText("2")).toBeTruthy();
    expect(within(cacheBlock).getByText(/refreshed_revision/)).toBeTruthy();
  });

  it("renders 暂无处理中任务 instead of an empty stage table", async () => {
    const fixture = overviewFixture();
    fixture.queues!.document_jobs.by_stage = {};
    overviewPayload = () => jsonResponse(fixture);
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    await openTab("MySQL");
    const jobsBlock = await screen.findByLabelText("文档任务队列");
    expect(within(jobsBlock).getByText("暂无处理中任务")).toBeTruthy();
    expect(jobsBlock.querySelector(".ant-table")).toBeNull();
    expect(within(jobsBlock).queryByText(/解析中/)).toBeNull();
  });

  it("renders the upload operation buckets including zeros", async () => {
    const fixture = overviewFixture();
    fixture.queues!.uploads = {
      unfinished: 5,
      by_status: { IN_PROGRESS: 0, FAILED: 2, ROLLING_BACK: 0, CLEANUP_PENDING: 3 },
      in_progress: 0,
      failed: 2,
      rolling_back: 0,
      cleanup_pending: 3,
    };
    overviewPayload = () => jsonResponse(fixture);
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    await openTab("MySQL");
    const uploadsBlock = await screen.findByLabelText("上传队列");
    expect(statisticValue(uploadsBlock, "进行中")).toBe("0");
    expect(statisticValue(uploadsBlock, "失败")).toBe("2");
    expect(statisticValue(uploadsBlock, "回滚中")).toBe("0");
    expect(statisticValue(uploadsBlock, "待清理")).toBe("3");
    expect(within(uploadsBlock).getByText("未完成操作合计：5")).toBeTruthy();
  });

  it("shows the MySQL tab blocks and requests the mysql log panel", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("MySQL");
    const logPanel = await screen.findByLabelText("MySQL日志面板");

    // 依赖行只保留 MySQL，并保留状态/延迟/详情列。
    const dependencyBlock = screen.getByLabelText("依赖状态");
    const mysqlRow = within(dependencyBlock).getByText("MySQL").closest("tr") as HTMLElement;
    expect(within(mysqlRow).getByText("正常")).toBeTruthy();
    expect(within(mysqlRow).getByText("1.2 ms")).toBeTruthy();
    expect(within(mysqlRow).getByText(/连接正常 pool open=1/)).toBeTruthy();
    expect(within(dependencyBlock).queryByText("Redis")).toBeNull();

    // 文档任务队列：状态计数、队列年龄/卡死/失败与阶段 Tag 列表。
    const jobsBlock = screen.getByLabelText("文档任务队列");
    expect(within(jobsBlock).getByText("排队中 2")).toBeTruthy();
    expect(within(jobsBlock).getByText("处理中 1")).toBeTruthy();
    expect(within(jobsBlock).getByText("成功 10")).toBeTruthy();
    expect(within(jobsBlock).getByText("12 秒")).toBeTruthy();
    expect(within(jobsBlock).getByText("1 个")).toBeTruthy();
    expect(within(jobsBlock).getByText("2 个")).toBeTruthy();
    expect(within(jobsBlock).getByText("解析中 1")).toBeTruthy();
    expect(within(jobsBlock).getByText("切分中 2")).toBeTruthy();

    // 上传操作按互斥分类展示，并注明不代表当前正在上传的文件数。
    const uploadsBlock = screen.getByLabelText("上传队列");
    expect(statisticValue(uploadsBlock, "进行中")).toBe("2");
    expect(statisticValue(uploadsBlock, "失败")).toBe("1");
    expect(statisticValue(uploadsBlock, "回滚中")).toBe("1");
    expect(statisticValue(uploadsBlock, "待清理")).toBe("1");
    expect(within(uploadsBlock).getByText("未完成操作合计：5")).toBeTruthy();
    expect(within(uploadsBlock).getByText(/不代表当前正在上传或入库的文件数/)).toBeTruthy();

    // 库与活动：文档数/知识库/artifact/近 24h 检索。
    const libraryBlock = screen.getByLabelText("库与活动");
    expect(within(libraryBlock).getByText("有效文档")).toBeTruthy();
    expect(within(libraryBlock).getByText("知识库")).toBeTruthy();
    expect(within(libraryBlock).getByText("artifact 快照")).toBeTruthy();
    expect(within(libraryBlock).getByText("近 24 小时检索次数")).toBeTruthy();
    expect(within(libraryBlock).getByText("3")).toBeTruthy();

    // 组件日志：默认 source=memory、component=mysql、最近 50 条。
    await waitFor(() => expect(lastLogUrl()).toContain("component=mysql"));
    const params = logParams(lastLogUrl());
    expect(params.get("source")).toBe("memory");
    expect(params.get("component")).toBe("mysql");
    expect(params.get("limit")).toBe("50");
    expect(logPanel).toBeTruthy();
    expect(await within(logPanel).findByText("MySQL 连接池等待")).toBeTruthy();
    expect(within(logPanel).getByText("WARN")).toBeTruthy();

    // 组件面板的刷新按钮重新拉取同一组参数。
    const before = logUrls.length;
    const logRefresh = within(logCard("MySQL日志面板")).getByRole("button", { name: /刷新日志/ });
    await waitFor(() => expect(logRefresh.className).not.toContain("ant-btn-loading"));
    fireEvent.click(logRefresh);
    await waitFor(() => expect(logUrls.length).toBeGreaterThan(before));
    expect(logParams(lastLogUrl()).get("component")).toBe("mysql");
  });

  it("shows the Redis dependency row and the cache watermark", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("Redis");
    const dependencyBlock = await screen.findByLabelText("依赖状态");
    expect(within(dependencyBlock).getByText("Redis")).toBeTruthy();
    expect(within(dependencyBlock).getByText("0.4 ms")).toBeTruthy();
    expect(within(dependencyBlock).getByText("PING 成功")).toBeTruthy();

    const cacheBlock = screen.getByLabelText("缓存水位");
    expect(within(cacheBlock).getByText("脏 key")).toBeTruthy();
    expect(within(cacheBlock).getByText("7")).toBeTruthy();
    expect(within(cacheBlock).getByText("刷新失败")).toBeTruthy();
    expect(within(cacheBlock).getByText("2")).toBeTruthy();
    expect(within(cacheBlock).getByText("降级")).toBeTruthy();
    expect(within(cacheBlock).getByText(/refreshed_revision/)).toBeTruthy();

    await screen.findByLabelText("Redis日志面板");
    await waitFor(() => expect(lastLogUrl()).toContain("component=redis"));
  });

  it("shows the Qdrant dependency row including vector points and the collection name", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("Qdrant");
    const dependencyBlock = await screen.findByLabelText("依赖状态");
    const qdrantRow = within(dependencyBlock).getByText("Qdrant").closest("tr") as HTMLElement;
    expect(within(qdrantRow).getByText("正常")).toBeTruthy();
    expect(within(qdrantRow).getByText("5.1 ms")).toBeTruthy();
    expect(within(qdrantRow).getByText(/3 个集合，目标集合存在/)).toBeTruthy();
    expect(within(qdrantRow).getByText(/向量点 1,234/)).toBeTruthy();

    const collectionBlock = screen.getByLabelText("Qdrant 集合");
    expect(within(collectionBlock).getByText("集合名")).toBeTruthy();
    expect(within(collectionBlock).getByText("praxis_chunks")).toBeTruthy();
    await waitFor(() => expect(lastLogUrl()).toContain("component=qdrant"));
  });

  it("shows the Kafka dependency row, both consumer groups and the document job delivery state", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("Kafka");
    const dependencyBlock = await screen.findByLabelText("依赖状态");
    const kafkaRow = within(dependencyBlock).getByText("Kafka").closest("tr") as HTMLElement;
    expect(within(kafkaRow).getByText("未启用")).toBeTruthy();
    expect(within(kafkaRow).getByText("—")).toBeTruthy();

    // 两组 Topic / Consumer Group / offset / 滞后分开显示，未知不伪造成 0。
    const groupsBlock = screen.getByLabelText("Kafka 消费进度");
    expect(within(groupsBlock).getByText("Kafka 文档消费")).toBeTruthy();
    expect(within(groupsBlock).getByText("Kafka 缓存消费")).toBeTruthy();
    expect(within(groupsBlock).getByText(/Topic：praxis\.rag\.document_jobs/)).toBeTruthy();
    expect(within(groupsBlock).getByText(/Topic：praxis\.rag\.cache/)).toBeTruthy();
    expect(within(groupsBlock).getByText(/Consumer Group：rag-document-workers/)).toBeTruthy();
    expect(within(groupsBlock).getByText(/Consumer Group：rag-cache-workers/)).toBeTruthy();
    expect(within(groupsBlock).getAllByText("消费滞后：未知")).toHaveLength(2);

    const deliveryBlock = screen.getByLabelText("文档任务状态");
    expect(within(deliveryBlock).getByText("文档任务正常，最近成功 10 个")).toBeTruthy();
    await waitFor(() => expect(lastLogUrl()).toContain("component=kafka"));
  });

  it("shows the PDF dependency row and the configured PDF engine", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("PDF 解析服务");
    const dependencyBlock = await screen.findByLabelText("依赖状态");
    const pdfRow = within(dependencyBlock).getByText("PDF 解析服务").closest("tr") as HTMLElement;
    expect(within(pdfRow).getByText("未启用")).toBeTruthy();
    expect(within(pdfRow).getByText("Go 内置解析引擎")).toBeTruthy();
    expect(within(pdfRow).getByText("—")).toBeTruthy();

    const engineBlock = screen.getByLabelText("PDF 引擎");
    expect(within(engineBlock).getByText("引擎")).toBeTruthy();
    expect(within(engineBlock).getByText("vision")).toBeTruthy();
    expect(within(engineBlock).getByText(/可选独立微服务/)).toBeTruthy();
    await waitFor(() => expect(lastLogUrl()).toContain("component=pdf"));
  });

  it("shows the worker heartbeat, document job pipeline state and stage distribution", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("文档 Worker");
    const heartbeatBlock = await screen.findByLabelText("Worker 心跳");
    expect(within(heartbeatBlock).getByText("文档 Worker")).toBeTruthy();
    expect(
      within(heartbeatBlock).getByText("最近心跳 3.0 秒前，1 个实例运行中"),
    ).toBeTruthy();
    expect(within(heartbeatBlock).getByText("正常")).toBeTruthy();

    const pipelineJobsBlock = screen.getByLabelText("文档任务状态");
    expect(within(pipelineJobsBlock).getByText("文档任务正常，最近成功 10 个")).toBeTruthy();

    const stagesBlock = screen.getByLabelText("阶段分布");
    expect(within(stagesBlock).getByText("解析中 1")).toBeTruthy();
    expect(within(stagesBlock).getByText("切分中 2")).toBeTruthy();
    await waitFor(() => expect(lastLogUrl()).toContain("component=worker"));
  });

  it("shows both Debezium connectors with the honest 未知 capture progress", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("Debezium");
    const connectorBlock = await screen.findByLabelText("Debezium 连接器");
    expect(within(connectorBlock).getByText("Debezium 任务连接器")).toBeTruthy();
    expect(within(connectorBlock).getByText("任务连接器未读健康端点")).toBeTruthy();
    expect(within(connectorBlock).getByText("Debezium 缓存连接器")).toBeTruthy();
    expect(within(connectorBlock).getByText("缓存连接器运行中")).toBeTruthy();
    expect(
      within(connectorBlock).getAllByText("采集进度：未知（需在 Debezium 侧查看）"),
    ).toHaveLength(2);

    expect(screen.getByLabelText("文档任务状态")).toBeTruthy();
    await waitFor(() => expect(lastLogUrl()).toContain("component=debezium"));
  });

  it("renders 未知 for every delivery view when the pipeline snapshot is missing", async () => {
    overviewPayload = () => jsonResponse(overviewFixture({ pipeline: null }));
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    // 依赖表与关键指标不受 pipeline 缺失影响。
    const metricsCard = screen.getByText("关键指标").closest(".ant-card") as HTMLElement;
    expect(statisticValue(metricsCard, "有效文档")).toBe("12");

    await openTab("Redis");
    const cacheBlock = await screen.findByLabelText("缓存水位");
    // 状态标签、脏 key、刷新失败都降级为“未知”。
    expect(within(cacheBlock).getAllByText("未知")).toHaveLength(3);

    await openTab("Debezium");
    const connectorBlock = await screen.findByLabelText("Debezium 连接器");
    // 两个连接器各有一个状态 Tag 与详情文本降级为“未知”。
    expect(within(connectorBlock).getAllByText("未知").length).toBeGreaterThanOrEqual(4);
    expect(
      within(connectorBlock).getAllByText("采集进度：未知（需在 Debezium 侧查看）"),
    ).toHaveLength(2);

    await openTab("Kafka");
    const groupsBlock = await screen.findByLabelText("Kafka 消费进度");
    expect(within(groupsBlock).getAllByText("消费滞后：未知")).toHaveLength(2);
    expect(within(groupsBlock).getAllByText(/Topic：未知/)).toHaveLength(2);
  });

  it("renders the numeric Kafka consumer lag when offsets are reported", async () => {
    const fixture = overviewFixture();
    fixture.pipeline!.kafka.groups.document = {
      status: "ok",
      detail: "文档消费正常",
      topic: "praxis.rag.document_jobs",
      group: "rag-document-workers",
      committed_offset: 95,
      end_offset: 100,
      consumer_lag: 5,
    };
    overviewPayload = () => jsonResponse(fixture);
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("Kafka");
    const groupsBlock = await screen.findByLabelText("Kafka 消费进度");
    expect(within(groupsBlock).getByText("消费滞后：5 条（已提交 95 / 末尾 100）")).toBeTruthy();
    // 缓存组仍无位点：未知不会被 0 覆盖。
    expect(within(groupsBlock).getAllByText("消费滞后：未知")).toHaveLength(1);
  });

  it("renders the worker row as 未知 when the heartbeat is missing and 正常 once it arrives", async () => {
    const fixture = overviewFixture();
    fixture.pipeline!.worker = {
      status: "unknown",
      detail: "未收到独立文档 worker 心跳（确认 rag-document-worker 已启动）",
    };
    overviewPayload = () => jsonResponse(fixture);
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("文档 Worker");
    const heartbeatBlock = await screen.findByLabelText("Worker 心跳");
    expect(
      within(heartbeatBlock).getByText("未收到独立文档 worker 心跳（确认 rag-document-worker 已启动）"),
    ).toBeTruthy();

    // 下一次快照带回心跳后，该行切换为“正常”。
    overviewPayload = () => jsonResponse(overviewFixture());
    const refreshButton = screen.getByRole("button", { name: "刷新监控数据" });
    await waitFor(() => expect(refreshButton.className).not.toContain("ant-btn-loading"));
    fireEvent.click(refreshButton);
    expect(await screen.findByText("最近心跳 3.0 秒前，1 个实例运行中")).toBeTruthy();
  });

  it("filters the 全部日志 tab by level, component, keyword and source", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("全部日志");
    const panel = await screen.findByLabelText("全部日志面板");
    await waitFor(() => expect(logUrls.length).toBeGreaterThan(0));

    // 默认：内存来源、第一页、客户端分页，不带组件过滤。
    const initial = logParams(logUrls[0]);
    expect(initial.get("source")).toBe("memory");
    expect(initial.get("limit")).toBe("100");
    expect(initial.get("component")).toBeNull();
    expect(initial.get("level")).toBeNull();
    expect(initial.get("query")).toBeNull();
    // 表格展示时间/级别/组件/内容/代码位置；代码位置是调用方 file:line，
    // 不是日志来源（来源由上方 Segmented 与「当前来源」表达），空值退化为 —。
    expect(within(panel).getByRole("columnheader", { name: "代码位置" })).toBeTruthy();
    expect(within(panel).getByText("internal/state/store.go:431")).toBeTruthy();
    expect(within(panel).getByText("internal/cache/refresher.go:57")).toBeTruthy();
    expect(within(panel).getByText("—")).toBeTruthy();
    expect(await within(panel).findByText("MySQL 连接池等待")).toBeTruthy();
    expect(within(panel).getByText("WARN")).toBeTruthy();
    expect(within(panel).getByText("缓存刷新失败")).toBeTruthy();

    // 级别过滤：全部/INFO/WARN/ERROR。
    await selectOption("全部日志面板级别", "WARN");
    await waitFor(() => expect(logParams(lastLogUrl()).get("level")).toBe("warn"));

    // 组件过滤。
    await selectOption("全部日志面板组件", "Redis");
    await waitFor(() => expect(logParams(lastLogUrl()).get("component")).toBe("redis"));

    // 关键字：回车后带 query 参数。
    const keyword = screen.getByLabelText("全部日志面板关键字");
    fireEvent.change(keyword, { target: { value: "连接 超时" } });
    fireEvent.keyDown(keyword, { key: "Enter", code: "Enter" });
    await waitFor(() => expect(logParams(lastLogUrl()).get("query")).toBe("连接 超时"));

    // 来源切换：文件。
    fireEvent.click(within(panel).getByRole("radio", { name: "文件" }));
    await waitFor(() => expect(logParams(lastLogUrl()).get("source")).toBe("file"));
    // 文件信息：配置路径/大小/截断说明。
    expect(within(panel).getByText("配置路径")).toBeTruthy();
    expect(within(panel).getByText("/var/log/praxis-rag.log")).toBeTruthy();
    expect(within(panel).getByText("4.0 KB")).toBeTruthy();

    // 生效的参数会随之后的刷新一起发送。
    const applied = logParams(lastLogUrl());
    expect(applied.get("component")).toBe("redis");
    expect(applied.get("level")).toBe("warn");
  });

  it("expands a log row to show its attrs", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("全部日志");
    const panel = await screen.findByLabelText("全部日志面板");
    const row = (await within(panel).findByText("缓存刷新失败")).closest("tr") as HTMLElement;
    const expand = row.querySelector(".ant-table-row-expand-icon") as HTMLElement;
    fireEvent.click(expand);
    expect(await within(panel).findByText("kb:1")).toBeTruthy();
  });

  it("renders 暂无日志 for an empty log page and shows notes", async () => {
    logsResponder = () => logPageFixture({ entries: [], total: 0, notes: ["日志只保留最近 2000 条"] });
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("全部日志");
    const panel = await screen.findByLabelText("全部日志面板");
    expect(await within(panel).findByText("暂无日志")).toBeTruthy();
    expect(within(panel).getByText("日志只保留最近 2000 条")).toBeTruthy();
  });

  it("shows the Chinese note when the file source is not configured", async () => {
    logsResponder = (url) => {
      const source = logParams(url).get("source") ?? "memory";
      if (source !== "file") return logPageFixture();
      return logPageFixture({
        source: "file",
        available: false,
        entries: [],
        notes: ["文件日志不可用：日志文件未配置"],
        file: {
          configured: false,
          path: "",
          size_bytes: 0,
          truncated: false,
          note: "未设置 RAG_LOG_FILE",
        },
      });
    };
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("全部日志");
    const panel = await screen.findByLabelText("全部日志面板");
    fireEvent.click(within(panel).getByRole("radio", { name: "文件" }));
    expect(await within(panel).findByText("日志不可用")).toBeTruthy();
    expect(within(panel).getAllByText(/日志文件未配置/).length).toBeGreaterThanOrEqual(1);
    expect(within(panel).getByText(/未设置 RAG_LOG_FILE/)).toBeTruthy();
  });

  it("shows the Chinese note when a component log buffer is unavailable", async () => {
    logsResponder = (url) => {
      const component = logParams(url).get("component");
      if (component === "mysql") {
        return logPageFixture({ available: false, entries: [], total: 0, notes: ["MySQL 日志缓冲不可用"] });
      }
      return logPageFixture();
    };
    renderPage();
    await waitFor(() => expect(screen.getByText("0.4.0")).toBeTruthy());

    await openTab("MySQL");
    const panel = await screen.findByLabelText("MySQL日志面板");
    expect(await within(panel).findByText("日志不可用")).toBeTruthy();
    expect(within(panel).getByText("MySQL 日志缓冲不可用")).toBeTruthy();
    expect(within(panel).getByText("暂无日志")).toBeTruthy();
  });

  it("starts with auto refresh on and refreshes on demand", async () => {
    renderPage();
    await screen.findByText("0.4.0");

    const toggle = screen.getByRole("switch");
    expect(toggle.getAttribute("aria-checked")).toBe("true");

    // 刷新按钮在首轮请求结束前处于 loading，antd 会忽略点击：等它恢复再点。
    const refreshButton = screen.getByRole("button", { name: "刷新监控数据" });
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
