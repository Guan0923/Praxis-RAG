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
        // 兼容镜像字段：页面读取 groups，三组各自可读。
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

// statisticValue reads one Statistic's rendered number by its title, avoiding
// ambiguity when different cards show the same count.
function statisticValue(scope: HTMLElement, title: string) {
  const statistic = within(scope).getByText(title).closest(".ant-statistic") as HTMLElement;
  return (statistic.querySelector(".ant-statistic-content")?.textContent ?? "").trim();
}

// cardTitles lists the card headings in DOM order: the layout contract puts the
// key metrics first and the static service facts last.
function cardTitles(scope: HTMLElement) {
  return Array.from(scope.querySelectorAll(".ant-card-head-title")).map(
    (node) => (node.textContent ?? "").trim(),
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
  it("leads with the key metrics, then dependencies, queues and the static service facts", async () => {
    fetchMock.mockResolvedValue(jsonResponse(overviewFixture()));
    renderPage();

    expect(await screen.findByRole("heading", { name: "系统监控" })).toBeTruthy();
    expect(await screen.findByText("0.4.0")).toBeTruthy();
    expect(screen.getByText("运行正常")).toBeTruthy();
    expect(screen.getByText(/更新于/)).toBeTruthy();

    // 版式验收：第一张内容卡片是关键指标，服务信息移到最后。
    const page = screen.getByLabelText("系统监控页面") as HTMLElement;
    expect(cardTitles(page)).toEqual(["关键指标", "依赖与投递", "队列与记录", "服务信息"]);

    // 关键指标：五个任务指标都以 Statistic 渲染，数据来自 queues/library/activity。
    const metricsCard = screen.getByText("关键指标").closest(".ant-card") as HTMLElement;
    expect(statisticValue(metricsCard, "有效文档")).toBe("12");
    expect(statisticValue(metricsCard, "排队文档任务")).toBe("2");
    expect(statisticValue(metricsCard, "处理中文档任务")).toBe("1");
    expect(statisticValue(metricsCard, "上传进行中")).toBe("2");
    expect(statisticValue(metricsCard, "近 24 小时检索次数")).toBe("12");

    // 依赖表：MySQL 行显示正常与延迟，禁用项不显示无意义的 0ms。
    const mysqlRow = screen.getByText("MySQL").closest("tr");
    expect(mysqlRow).toBeTruthy();
    expect(within(mysqlRow as HTMLElement).getByText("正常")).toBeTruthy();
    expect(within(mysqlRow as HTMLElement).getByText("1.2 ms")).toBeTruthy();
    const kafkaRow = screen.getByText("Kafka").closest("tr");
    expect(within(kafkaRow as HTMLElement).getByText("未启用")).toBeTruthy();
    expect(within(kafkaRow as HTMLElement).getByText("—")).toBeTruthy();
    expect(screen.getByText(/向量点 1,234/)).toBeTruthy();

    // 文档任务：状态计数、队列年龄/卡死/失败与阶段 Tag 列表。
    const jobsBlock = screen.getByLabelText("文档任务队列");
    expect(within(jobsBlock).getByText("排队中 2")).toBeTruthy();
    expect(within(jobsBlock).getByText("处理中 1")).toBeTruthy();
    expect(within(jobsBlock).getByText("成功 10")).toBeTruthy();
    expect(within(jobsBlock).getByText("12 秒")).toBeTruthy();
    expect(within(jobsBlock).getByText("1 个")).toBeTruthy();
    expect(within(jobsBlock).getByText("2 个")).toBeTruthy();
    // 阶段分布是紧凑 Tag 列表，不再是表格（Descriptions 自身的布局表格不算）。
    expect(within(jobsBlock).getByText("解析中 1")).toBeTruthy();
    expect(within(jobsBlock).getByText("切分中 2")).toBeTruthy();
    expect(jobsBlock.querySelector(".ant-table")).toBeNull();

    // 缓存水位取代旧的 outbox 记录卡：脏 key 与刷新失败分别展示。
    const cacheBlock = screen.getByLabelText("缓存水位");
    expect(within(cacheBlock).getByText("脏 key")).toBeTruthy();
    expect(within(cacheBlock).getByText("7")).toBeTruthy();
    expect(within(cacheBlock).getByText("刷新失败")).toBeTruthy();
    expect(within(cacheBlock).getByText("2")).toBeTruthy();
    expect(within(cacheBlock).getByText(/refreshed_revision/)).toBeTruthy();

    // 投递与消费进度：任务/缓存两个 Debezium 连接器、两组 Kafka 消费、
    // 文档 Worker 与文档任务状态分开；未知进度显示“未知”，不伪造 0 或百分比。
    const pipelineBlock = screen.getByLabelText("投递与消费进度");
    expect(within(pipelineBlock).getByText("Debezium 任务连接器")).toBeTruthy();
    expect(within(pipelineBlock).getByText("任务连接器未读健康端点")).toBeTruthy();
    expect(within(pipelineBlock).getByText("Debezium 缓存连接器")).toBeTruthy();
    expect(within(pipelineBlock).getByText("缓存连接器运行中")).toBeTruthy();
    expect(
      within(pipelineBlock).getAllByText("采集进度：未知（需在 Debezium 侧查看）"),
    ).toHaveLength(2);
    expect(within(pipelineBlock).getByText("Kafka 文档消费")).toBeTruthy();
    expect(within(pipelineBlock).getByText("Kafka 缓存消费")).toBeTruthy();
    expect(within(pipelineBlock).getAllByText("消费滞后：未知")).toHaveLength(2);
    expect(within(pipelineBlock).getByText(/Topic：praxis\.rag\.cache/)).toBeTruthy();
    expect(within(pipelineBlock).getByText("文档 Worker")).toBeTruthy();
    expect(within(pipelineBlock).getByText("最近心跳 3.0 秒前，1 个实例运行中")).toBeTruthy();
    expect(within(pipelineBlock).getByText("文档任务状态")).toBeTruthy();
    expect(within(pipelineBlock).getByText("文档任务正常，最近成功 10 个")).toBeTruthy();

    // 上传操作按互斥分类展示，0 也要显示，并注明不代表当前正在上传的文件数。
    const uploadsBlock = screen.getByLabelText("上传队列");
    expect(statisticValue(uploadsBlock, "进行中")).toBe("2");
    expect(statisticValue(uploadsBlock, "失败")).toBe("1");
    expect(statisticValue(uploadsBlock, "回滚中")).toBe("1");
    expect(statisticValue(uploadsBlock, "待清理")).toBe("1");
    expect(within(uploadsBlock).getByText("未完成操作合计：5")).toBeTruthy();
    expect(within(uploadsBlock).getByText(/未完成操作 = 进行中 \+ 失败\/回滚待处理 \+ 待清理/)).toBeTruthy();
    expect(within(uploadsBlock).getByText(/不代表当前正在上传或入库的文件数/)).toBeTruthy();

    // 服务信息在页面最后：静态配置仍完整可读。
    const serviceCard = screen.getByText("服务信息").closest(".ant-card") as HTMLElement;
    expect(within(serviceCard).getByText("2 小时 20 分")).toBeTruthy();
    expect(within(serviceCard).getByText("go")).toBeTruthy();
    expect(within(serviceCard).getByText("praxis_chunks")).toBeTruthy();
    expect(within(serviceCard).getByText("async")).toBeTruthy();
    expect(within(serviceCard).getByText("投递方式")).toBeTruthy();
    expect(within(serviceCard).getByText("debezium")).toBeTruthy();
    expect(within(serviceCard).getByText("vision")).toBeTruthy();

    // 首次请求直接命中聚合接口且绕过缓存。
    expect(fetchMock.mock.calls[0][0]).toBe("/v1/admin/monitor/overview");
    const requestOptions = fetchMock.mock.calls[0][1] as RequestInit;
    expect(requestOptions.cache).toBe("no-store");
    expect(requestOptions.signal).toBeInstanceOf(AbortSignal);
  });

  it("shows 存在降级 and summarizes the degraded dependencies when status is degraded", async () => {
    const fixture = overviewFixture({
      status: "degraded",
      ok: false,
      dependencies: overviewFixture().dependencies.map((dependency) =>
        dependency.name === "qdrant"
          ? { ...dependency, status: "degraded" as const, detail: "目标集合不存在" }
          : dependency,
      ),
    });
    fetchMock.mockResolvedValue(jsonResponse(fixture));
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    expect(screen.getByText("存在降级")).toBeTruthy();
    expect(screen.queryByText("存在异常")).toBeNull();
    expect(screen.getByText("服务存在降级")).toBeTruthy();
    // 降级依赖自动汇总为 label + detail，而不是一句泛泛的警告。
    expect(screen.getByText("Qdrant：目标集合不存在")).toBeTruthy();

    const qdrantRow = screen.getByText("Qdrant").closest("tr");
    expect(within(qdrantRow as HTMLElement).getByText("降级")).toBeTruthy();
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
    fetchMock.mockResolvedValue(jsonResponse(fixture));
    renderPage();

    expect(await screen.findByText("服务状态异常")).toBeTruthy();
    expect(screen.getByText("存在异常")).toBeTruthy();
    expect(screen.getByText("Qdrant 连接失败：connection refused")).toBeTruthy();
    const qdrantRow = screen.getByText("Qdrant").closest("tr");
    expect(within(qdrantRow as HTMLElement).getByText("异常")).toBeTruthy();
  });

  it("falls back to the ok boolean when the status field is missing", async () => {
    fetchMock.mockResolvedValue(jsonResponse(overviewFixture({ status: undefined, ok: true })));
    const first = renderPage();
    expect(await screen.findByText("运行正常")).toBeTruthy();
    first.unmount();

    fetchMock.mockResolvedValue(
      jsonResponse(overviewFixture({ status: undefined, ok: false, errors: ["监控统计不可用：timeout"] })),
    );
    renderPage();
    expect(await screen.findByText("存在异常")).toBeTruthy();
    expect(screen.getByText("监控统计不可用：timeout")).toBeTruthy();
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
    expect(screen.queryByText("关键指标")).toBeNull();
    expect(screen.queryByText("服务信息")).toBeNull();

    fetchMock.mockResolvedValue(jsonResponse(overviewFixture()));
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(await screen.findByText("0.4.0")).toBeTruthy();
    await waitFor(() => expect(screen.queryByText("监控数据加载失败")).toBeNull());
  });

  it("renders 未知 for every metric when the state database snapshot is unavailable", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(overviewFixture({ queues: null, library: null, activity: null })),
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

    // 队列统计不可用时，文档任务明细降级为“未知”，阶段分布提示状态库不可用。
    const jobsBlock = screen.getByLabelText("文档任务队列");
    expect(within(jobsBlock).getAllByText("未知")).toHaveLength(3);
    expect(within(jobsBlock).getByText("状态库暂不可用")).toBeTruthy();

    // 上传分类整体不可用时保持“未知”，不显示伪造的 0。
    const uploadsBlock = screen.getByLabelText("上传队列");
    expect(within(uploadsBlock).getByText("上传操作：未知")).toBeTruthy();

    // 缓存水位独立于状态库队列，仍可读。
    const cacheBlock = screen.getByLabelText("缓存水位");
    expect(within(cacheBlock).getByText("7")).toBeTruthy();
    expect(within(cacheBlock).getByText("2")).toBeTruthy();
  });

  it("renders 暂无处理中任务 instead of an empty stage table", async () => {
    const fixture = overviewFixture();
    fixture.queues!.document_jobs.by_stage = {};
    fetchMock.mockResolvedValue(jsonResponse(fixture));
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    const jobsBlock = screen.getByLabelText("文档任务队列");
    expect(within(jobsBlock).getByText("暂无处理中任务")).toBeTruthy();
    // 阶段分布不再使用 Table：该区块内不应出现 .ant-table（Descriptions 自身的
    // 布局表格不算），也不应残留阶段 Tag。
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
    fetchMock.mockResolvedValue(jsonResponse(fixture));
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    const uploadsBlock = screen.getByLabelText("上传队列");
    expect(statisticValue(uploadsBlock, "进行中")).toBe("0");
    expect(statisticValue(uploadsBlock, "失败")).toBe("2");
    expect(statisticValue(uploadsBlock, "回滚中")).toBe("0");
    expect(statisticValue(uploadsBlock, "待清理")).toBe("3");
    expect(within(uploadsBlock).getByText("未完成操作合计：5")).toBeTruthy();
  });

  it("renders the worker row as 未知 when the heartbeat is missing and 正常 once it arrives", async () => {
    const fixture = overviewFixture();
    fixture.pipeline!.worker = {
      status: "unknown",
      detail: "未收到独立文档 worker 心跳（确认 rag-document-worker 已启动）",
    };
    fetchMock.mockResolvedValue(jsonResponse(fixture));
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    const pipelineBlock = screen.getByLabelText("投递与消费进度");
    expect(within(pipelineBlock).getByText("文档 Worker")).toBeTruthy();
    expect(
      within(pipelineBlock).getByText("未收到独立文档 worker 心跳（确认 rag-document-worker 已启动）"),
    ).toBeTruthy();

    // 下一次快照带回心跳后，该行切换为“正常”。
    fetchMock.mockResolvedValue(jsonResponse(overviewFixture()));
    const refreshButton = screen.getByRole("button", { name: /刷新/ });
    await waitFor(() => expect(refreshButton.className).not.toContain("ant-btn-loading"));
    fireEvent.click(refreshButton);
    expect(await screen.findByText("最近心跳 3.0 秒前，1 个实例运行中")).toBeTruthy();
    const refreshedBlock = screen.getByLabelText("投递与消费进度");
    // 缓存连接器、文档 Worker 与文档任务状态都是“正常”。
    expect(within(refreshedBlock).getAllByText("正常")).toHaveLength(3);
  });

  it("renders 未知 for every delivery view when the pipeline snapshot is missing", async () => {
    fetchMock.mockResolvedValue(jsonResponse(overviewFixture({ pipeline: null })));
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    // 依赖表与关键指标不受 pipeline 缺失影响。
    expect(screen.getByText("MySQL")).toBeTruthy();
    const metricsCard = screen.getByText("关键指标").closest(".ant-card") as HTMLElement;
    expect(statisticValue(metricsCard, "有效文档")).toBe("12");

    const cacheBlock = screen.getByLabelText("缓存水位");
    // 状态标签、脏 key、刷新失败都降级为“未知”。
    expect(within(cacheBlock).getAllByText("未知")).toHaveLength(3);

    const pipelineBlock = screen.getByLabelText("投递与消费进度");
    // 两个连接器、两组 Kafka、Worker 与文档任务的状态和详情都降级为“未知”，
    // 不会显示 0 或伪造进度。
    expect(within(pipelineBlock).getAllByText("未知").length).toBeGreaterThanOrEqual(12);
    expect(
      within(pipelineBlock).getAllByText("采集进度：未知（需在 Debezium 侧查看）"),
    ).toHaveLength(2);
    expect(within(pipelineBlock).getAllByText("消费滞后：未知")).toHaveLength(2);
    expect(within(pipelineBlock).getAllByText(/Topic：未知/)).toHaveLength(2);
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
    fetchMock.mockResolvedValue(jsonResponse(fixture));
    renderPage();

    expect(await screen.findByText("0.4.0")).toBeTruthy();
    const pipelineBlock = screen.getByLabelText("投递与消费进度");
    expect(within(pipelineBlock).getByText("消费滞后：5 条（已提交 95 / 末尾 100）")).toBeTruthy();
    // 缓存组仍无位点：未知不会被 0 覆盖。
    expect(within(pipelineBlock).getAllByText("消费滞后：未知")).toHaveLength(1);
    // 缓存连接器、文档组、Worker 与文档任务都是“正常”。
    expect(within(pipelineBlock).getAllByText("正常")).toHaveLength(4);
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
