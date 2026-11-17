import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, createContext, useContext, type ReactNode } from "react";
import {
  Alert,
  App,
  Button,
  Card,
  Col,
  Descriptions,
  Flex,
  Input,
  InputNumber,
  Row,
  Segmented,
  Select,
  Spin,
  Statistic,
  Switch,
  Table,
  Tabs,
  Tag,
  Tooltip,
} from "antd";
import { ReloadOutlined } from "@ant-design/icons";
import {
  errorText,
  fetchMonitorLogs,
  fetchMonitorOverview,
  fetchWorkerDeployment,
  formatBytes,
  formatDateTime,
  formatDuration,
  formatPercent,
  formatRate,
  monitorComponentLabels,
  monitorDependencyStatusColors,
  monitorDependencyStatusLabels,
  monitorTabComponents,
  requeueDocumentJobs,
  restartMonitorConnector,
  scaleWorker,
  uploadJobStageLabels,
  type MonitorActionCapabilities,
  type MonitorActivity,
  type MonitorDependency,
  type MonitorDependencyStatus,
  type MonitorLibrary,
  type MonitorLogEntry,
  type MonitorLogFile,
  type MonitorLogPage,
  type MonitorLogSource,
  type MonitorOverallStatus,
  type MonitorOverview,
  type MonitorPipeline,
  type MonitorPipelineConnector,
  type MonitorPipelineKafkaGroup,
  type MonitorQueues,
  type MonitorService,
  type MonitorSystem,
  type MonitorTabComponent,
  type MonitorWorkerDeployment,
} from "./api";

// monitorRefreshMs is the auto-refresh period: the backend does not push
// snapshot updates, so the page polls. It stays off the request path and only
// re-fetches while the switch is on.
const monitorRefreshMs = 15_000;

// monitorLogLimit is the number of rows a component panel asks for; the 全部日志
// tab asks for a wider client-paginated window instead.
const monitorComponentLogLimit = 50;
const monitorAllLogLimit = 100;

// --- Operator actions (监控操作) ---------------------------------------------
//
// The monitor page is read-only by construction, but a handful of rows name a
// concrete operator remedy (stuck jobs, exhausted connector restarts, worker
// capacity). Those rows carry their action button. Every action is
// admin-scoped on the backend; the page hides a button whose capability flag
// is false (older backend or unwired deployment) instead of calling an
// endpoint that answers 503.

// MonitorActionContext carries the action runners and the capability flags
// down to the blocks, so each block stays presentational. Each runner takes
// the thunk to call, surfaces the result message, refreshes the snapshot, and
// resolves when the underlying call settles so the button spinner stops.
type MonitorActionContextValue = {
  capabilities?: MonitorActionCapabilities;
  refresh: () => void;
  requeueJobs: (run: () => Promise<{ requeued: number }>) => Promise<void>;
  restartConnector: (run: () => Promise<{ restarted: boolean }>, connector: string) => Promise<void>;
  scale: (run: () => Promise<{ replicas: number }>) => Promise<void>;
};

const MonitorActionContext = createContext<MonitorActionContextValue | null>(null);

// useMonitorActions is the block-side accessor. A null context (unit tests
// rendering a block standalone) simply hides the buttons.
function useMonitorActions(): MonitorActionContextValue | null {
  return useContext(MonitorActionContext);
}

// The three-state overall status ranks the snapshot. An older backend omits the
// field entirely, in which case the boolean fallback keeps the header honest.
const overallStatusLabels: Record<MonitorOverallStatus, string> = {
  ok: "运行正常",
  degraded: "存在降级",
  error: "存在异常",
};
const overallStatusColors: Record<MonitorOverallStatus, string> = {
  ok: "success",
  degraded: "warning",
  error: "error",
};

function overallStatusOf(overview: MonitorOverview): MonitorOverallStatus {
  return overview.status ?? (overview.ok ? "ok" : "error");
}

// Queue statuses are rendered as compact count tags.
const documentJobStatusLabels: Record<string, string> = {
  queued: "排队中",
  processing: "处理中",
  succeeded: "成功",
  failed: "失败",
};

// Log levels keep the backend's lowercase keys; the table renders them as the
// upper-case tags operators expect.
const logLevelLabels: Record<string, string> = {
  debug: "DEBUG",
  info: "INFO",
  warn: "WARN",
  error: "ERROR",
};
const logLevelColors: Record<string, string> = {
  debug: "default",
  info: "blue",
  warn: "warning",
  error: "error",
};
// The level filter is fixed to 全部/INFO/WARN/ERROR; debug records stay visible
// under 全部级别 but are not a dedicated filter.
const logLevelOptions = [
  { value: "", label: "全部级别" },
  { value: "info", label: "INFO" },
  { value: "warn", label: "WARN" },
  { value: "error", label: "ERROR" },
];
const logSourceOptions = [
  { value: "memory", label: "内存" },
  { value: "file", label: "文件" },
];

// unknownOrNumber keeps absent counters out of the page instead of printing a
// fake zero. A real 0 renders as 0, so "no failed uploads" stays visible.
function unknownOrNumber(value: number | null | undefined) {
  return typeof value === "number" && Number.isFinite(value) ? value : "未知";
}

// countOf reads one bucket of a by_status map. A whole-snapshot miss is handled
// by the caller (未知); a missing bucket inside a present snapshot simply has no
// rows for that status and therefore counts as 0.
function countOf(counts: Record<string, number> | undefined | null, status: string) {
  const value = counts?.[status];
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

// formatClock keeps only the wall-clock part of generated_at: the page already
// lives next to the data, so the date would just add noise.
function formatClock(value?: string) {
  if (!value) return "—";
  const time = Date.parse(value);
  return Number.isFinite(time) ? new Date(time).toLocaleTimeString() : value;
}

// latencyText hides the meaningless 0ms latency of a disabled dependency.
function latencyText(dependency: MonitorDependency) {
  if (dependency.status === "disabled") return "—";
  return typeof dependency.latency_ms === "number" && Number.isFinite(dependency.latency_ms)
    ? `${dependency.latency_ms.toFixed(1)} ms`
    : "—";
}

// cpuUsageText marks a first sample as 采样中 instead of a fake 0%: the
// collector needs two polls before a usage percentage exists.
function cpuUsageText(value: number | null | undefined) {
  if (typeof value !== "number" || !Number.isFinite(value)) return "采样中/未知";
  return formatPercent(value);
}

// loadAverageText joins the three load averages. Windows never reports them, so
// a null triple stays 未知 instead of looking like an idle host.
function loadAverageText(cpu: MonitorSystem["cpu"]) {
  const { load1, load5, load15 } = cpu;
  if (load1 === null && load5 === null && load15 === null) return "未知";
  const part = (value: number | null) => (
    typeof value === "number" && Number.isFinite(value) ? value.toFixed(2) : "未知"
  );
  return `1/5/15 分钟：${part(load1)} / ${part(load5)} / ${part(load15)}`;
}

// StatusCountTags renders one queue's by_status map as count tags. Known
// statuses keep the label map's display order; unknown keys are appended as-is.
function StatusCountTags({ counts, labels }: {
  counts?: Record<string, number>;
  labels: Record<string, string>;
}) {
  const countsMap = counts || {};
  const known = Object.keys(labels).filter((status) => status in countsMap);
  const rest = Object.keys(countsMap).filter((status) => !known.includes(status));
  const entries = [...known, ...rest].map(
    (status) => [status, countsMap[status]] as const,
  );
  const tags = (
    <Flex wrap gap={6} className="monitor-tags">
      {entries.map(([status, count]) => {
        // 状态 key 的大小写由后端存储决定，查文案时统一兜底。
        const label = labels[status] ?? labels[status.toLowerCase()] ?? status;
        return (
          <Tag key={status}>
            {label} {count}
          </Tag>
        );
      })}
    </Flex>
  );
  return entries.length ? tags : <span className="secondary">暂无队列数据</span>;
}

// DependencyTable lists the built-in probes. The horizontal scroll keeps the
// four columns readable on a phone without stretching the card, and the detail
// cell wraps long diagnostics instead of pushing the table wider.
function DependencyTable({ dependencies }: { dependencies: MonitorDependency[] }) {
  return (
    <div className="monitor-dependency-table">
      <Table<MonitorDependency>
        rowKey="name"
        size="small"
        pagination={false}
        dataSource={dependencies}
        tableLayout="fixed"
        scroll={{ x: 520 }}
        locale={{ emptyText: "暂无依赖信息" }}
        columns={[
          {
            title: "依赖",
            dataIndex: "label",
            render: (_label: string, dependency) => (
              <span className="break-text">{dependency.label || dependency.name}</span>
            ),
          },
          {
            title: "状态",
            dataIndex: "status",
            width: 84,
            render: (status: MonitorDependency["status"]) => (
              <Tag color={monitorDependencyStatusColors[status] || "default"}>
                {monitorDependencyStatusLabels[status] || status}
              </Tag>
            ),
          },
          {
            title: "延迟",
            dataIndex: "latency_ms",
            width: 92,
            render: (_latency: number, dependency) => latencyText(dependency),
          },
          {
            title: "详情",
            dataIndex: "detail",
            render: (detail: string, dependency) => (
              <Flex wrap gap={8} align="center">
                <span className="break-text">{detail || "—"}</span>
                {typeof dependency.points === "number" && (
                  <span className="secondary">向量点 {dependency.points.toLocaleString()}</span>
                )}
              </Flex>
            ),
          },
        ]}
      />
    </div>
  );
}

// PipelineStatusTag renders one delivery/consumption section status with the
// same vocabulary as the dependency probes. A missing section stays 未知
// instead of being reported as healthy.
function PipelineStatusTag({ status }: { status?: MonitorDependencyStatus }) {
  if (!status) return <Tag>未知</Tag>;
  return (
    <Tag color={monitorDependencyStatusColors[status] || "default"}>
      {monitorDependencyStatusLabels[status] || "未知"}
    </Tag>
  );
}

// captureProgressText never invents a capture percentage: the binlog position
// lives in Kafka Connect's offset topic and the service does not read it, so the
// only honest value is 未知.
function captureProgressText(progress?: number | null) {
  return typeof progress === "number" && Number.isFinite(progress)
    ? `采集进度：${progress}`
    : "采集进度：未知（需在 Kafka Connect 侧查看）";
}

// connectorWorkerText answers "which worker runs this capture path?". After a
// worker failure this is the value that proves the task moved to a peer.
function connectorWorkerText(connector?: MonitorPipelineConnector | null) {
  return `所在 Worker：${connector?.worker_id || "未知"}`;
}

// connectorTaskText lists every task state with its worker. A connector without
// tasks is reported explicitly instead of looking healthy by omission.
function connectorTaskText(connector?: MonitorPipelineConnector | null) {
  const tasks = connector?.tasks ?? [];
  if (tasks.length === 0) return "Task：未调度";
  return `Task：${tasks
    .map((task) => {
      const state = task.state || "未知";
      return task.worker_id ? `task ${task.id} ${state}（${task.worker_id}）` : `task ${task.id} ${state}`;
    })
    .join("，")}`;
}

// connectorFailureText keeps the first failure reason visible on the page; the
// full bounded trace is also in connector.detail.
function connectorFailureText(connector?: MonitorPipelineConnector | null) {
  return connector?.failure_reason ? `失败原因：${connector.failure_reason}` : null;
}

// connectorRestartText renders the supervisor's recovery result: an in-flight
// restart with its attempt number, an exhausted budget that needs an operator,
// a recovered task, or no restart observed. A FAILED task never becomes a green
// row through this text.
function connectorRestartText(connector?: MonitorPipelineConnector | null) {
  if (!connector) return "自动重启：未知";
  if (connector.restart_exhausted) {
    return `自动重启：已耗尽（累计 ${connector.restart_attempts} 次），需人工处理`;
  }
  if (connector.restarting) {
    return `自动重启：进行中（第 ${connector.restart_attempts} 次）`;
  }
  if (connector.recovery === "recovered") return "自动重启：已恢复";
  if (!connector.supervised) return "自动重启：未启用（未配置 RAG_CONNECT_URLS）";
  return "自动重启：未触发";
}

// kafkaGroupLagText renders one consumer group's lag. The lag is only shown
// when Kafka itself reports it, otherwise it stays 未知 (never 0).
function kafkaGroupLagText(group?: MonitorPipelineKafkaGroup | null) {
  const lag = group?.consumer_lag;
  if (typeof lag !== "number" || !Number.isFinite(lag)) return "消费滞后：未知";
  const committed = typeof group?.committed_offset === "number"
    ? group.committed_offset.toLocaleString()
    : "未知";
  const end = typeof group?.end_offset === "number"
    ? group.end_offset.toLocaleString()
    : "未知";
  return `消费滞后：${lag.toLocaleString()} 条（已提交 ${committed} / 末尾 ${end}）`;
}

// degradedDependencyNotes summarizes what made the snapshot degraded so the
// banner does not just say "something is wrong": dependency probes and the
// delivery views both contribute label/detail pairs.
function degradedDependencyNotes(overview: MonitorOverview): string[] {
  const notes = overview.dependencies
    .filter((dependency) => dependency.status === "degraded")
    .map((dependency) => `${dependency.label || dependency.name}：${dependency.detail || "降级"}`);
  const pipeline = overview.pipeline;
  if (pipeline) {
    const views: Array<[string, { status: MonitorDependencyStatus; detail: string } | undefined]> = [
      ["文档采集", pipeline.debezium?.tasks],
      ["缓存采集", pipeline.debezium?.cache],
      ["文档 Worker", pipeline.worker],
      ["Kafka 消费", pipeline.kafka],
      ["文档任务", pipeline.document_jobs],
    ];
    for (const [label, view] of views) {
      if (view?.status === "degraded") notes.push(`${label}：${view.detail || "降级"}`);
    }
    if (pipeline.cache?.status === "degraded") {
      notes.push("缓存刷新：存在脏 key 或刷新失败未归零");
    }
  }
  return notes;
}

// PipelineRow is the shared status/detail row used by every delivery view. The
// optional children carry the connector-specific offsets or capture progress.
function PipelineRow({ label, status, detail, children }: {
  label: string;
  status?: MonitorDependencyStatus;
  detail?: string;
  children?: ReactNode;
}) {
  return (
    <div className="monitor-progress-row">
      <Flex wrap gap={8} align="center">
        <strong>{label}</strong>
        <PipelineStatusTag status={status} />
        <span className="secondary break-text">{detail || "未知"}</span>
      </Flex>
      {children}
    </div>
  );
}

// MonitorMasonry packs its blocks into columns so a short card can be followed
// by the next card instead of waiting for its tall neighbour to end. Native CSS
// masonry is not available in stable browsers yet (Chrome/Edge 140 gate it
// behind about:flags; Safari 26.4 exposes display:grid-lanes), so the columns are
// distributed here: every block is measured once per layout pass and placed in
// the currently shortest column. Blocks keep their relative order inside a
// column, and a single-column flow (narrow screens, or a browser that reports no
// width) is the same as the previous stacked layout.
type MonitorMasonryBlock = { key: string; node: ReactNode };

function MonitorMasonry({ blocks, dense = false }: { blocks: MonitorMasonryBlock[]; dense?: boolean }) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const itemRefs = useRef(new Map<string, HTMLDivElement>());
  const lastSignature = useRef("");
  const keys = blocks.map((block) => block.key).join("|");
  const [columnCount, setColumnCount] = useState(1);
  const [columns, setColumns] = useState<string[][]>(() => [blocks.map((block) => block.key)]);

  // Column count follows the container, not the viewport, so the layout still
  // works when the Console is embedded in a narrower pane.
  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    const resolve = () => {
      const width = host.clientWidth;
      const next = width >= 1200 && dense ? 3 : width >= 992 ? 2 : 1;
      setColumnCount((current) => (current === next ? current : next));
    };
    resolve();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(resolve);
    observer.observe(host);
    return () => observer.disconnect();
  }, [dense]);

  // A changed block set or column count resets to an even distribution; the
  // measuring pass below then repacks by real height.
  useEffect(() => {
    setColumns((current) => {
      const next: string[][] = Array.from({ length: columnCount }, () => []);
      blocks.forEach((block, index) => next[index % columnCount].push(block.key));
      const same = current.length === next.length
        && current.every((column, index) => column.join("|") === next[index].join("|"));
      if (same) return current;
      lastSignature.current = "";
      return next;
    });
    // blocks identity changes every render; the key string is the real input.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [keys, columnCount]);

  // Repack by measured height. The signature guard keeps this from looping:
  // column width does not change with the assignment, so heights are stable.
  useLayoutEffect(() => {
    if (columnCount <= 1) return;
    const measured = blocks.map((block) => ({
      key: block.key,
      height: itemRefs.current.get(block.key)?.offsetHeight ?? 0,
    }));
    if (measured.every((item) => item.height === 0)) return;
    const signature = `${columnCount}:${measured.map((item) => item.height).join(",")}`;
    if (signature === lastSignature.current) return;
    lastSignature.current = signature;
    const totals = new Array(columnCount).fill(0) as number[];
    const next: string[][] = Array.from({ length: columnCount }, () => []);
    for (const item of measured) {
      let target = 0;
      for (let index = 1; index < totals.length; index += 1) {
        if (totals[index] < totals[target]) target = index;
      }
      next[target].push(item.key);
      totals[target] += item.height + 12;
    }
    setColumns(next);
  });

  // Blocks added between renders stay visible until the packing pass runs.
  const placed = new Set(columns.flat());
  const pending = blocks.filter((block) => !placed.has(block.key)).map((block) => block.key);
  const rendered = columns.length
    ? columns.map((column, index) => (
      (index === 0 && pending.length ? [...column, ...pending] : column)
    ))
    : [blocks.map((block) => block.key)];
  const byKey = new Map(blocks.map((block) => [block.key, block.node]));

  return (
    <div className="monitor-masonry" ref={hostRef} data-columns={rendered.length}>
      {rendered.map((column, index) => (
        <div className="monitor-masonry-column" key={index}>
          {column.map((key) => (
            <div
              className="monitor-masonry-item"
              key={key}
              ref={(node) => {
                if (node) itemRefs.current.set(key, node);
                else itemRefs.current.delete(key);
              }}
            >
              {byKey.get(key)}
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

// KeyMetricsCard carries the page's first screen: task health instead of static
// service configuration. Each value degrades to 未知 when its section is
// unavailable rather than pretending the queue is empty.
function KeyMetricsCard({ overview }: { overview: MonitorOverview }) {
  const { queues, library, activity } = overview;
  const jobs = queues?.document_jobs;
  const uploads = queues?.uploads;
  const metrics: Array<{ key: string; title: string; value: number | string }> = [
    {
      key: "documents",
      title: "有效文档",
      value: library ? unknownOrNumber(library.documents) : "未知",
    },
    {
      key: "queued",
      title: "排队文档任务",
      value: queues ? countOf(jobs?.by_status, "queued") : "未知",
    },
    {
      key: "processing",
      title: "处理中文档任务",
      value: queues ? countOf(jobs?.by_status, "processing") : "未知",
    },
    {
      key: "uploads",
      title: "上传进行中",
      // in_progress is the strict INTENT/RUNNING count; the legacy unfinished
      // union is not used here because it mixes failures and cleanup work in.
      value: uploads ? unknownOrNumber(uploads.in_progress) : "未知",
    },
    {
      key: "searches",
      title: `近 ${activity?.window_hours ?? 24} 小时检索次数`,
      value: activity ? unknownOrNumber(activity.searches) : "未知",
    },
  ];

  return (
    <Card size="small" title="关键指标">
      <Row gutter={[16, 12]}>
        {metrics.map((metric) => (
          // 整行铺开：五个指标在宽屏上排成一行，窄屏回落 2/行。
          <Col key={metric.key} xs={12} sm={8} lg={4}>
            <Statistic className="monitor-stat" title={metric.title} value={metric.value} />
          </Col>
        ))}
      </Row>
    </Card>
  );
}

// SystemResourcesCard renders the host sample: CPU, memory, network and the
// service process. A missing collector or a first-sample null rate stays 未知.
function SystemResourcesCard({ system }: { system?: MonitorSystem | null }) {
  if (!system) {
    return (
      <Card size="small" title="系统资源">
        <span className="secondary">系统资源：未知（采集不可用）</span>
      </Card>
    );
  }
  const { cpu, memory, network, process } = system;
  // notes/interfaces are arrays in the wire contract, but a partial sample can
  // serialize them as null; fall back to empty collections instead of throwing.
  const interfaces = network.interfaces ?? [];
  const systemNotes = system.notes ?? [];
  return (
    <Card size="small" title="系统资源" className="monitor-system-card">
      <Row gutter={[20, 12]}>
        {/* 卡片现在只占半行，四组资源按 2×2 排布，避免每组被压到 100px 宽。 */}
        <Col xs={24} sm={12}>
          <h4 className="monitor-subheading">CPU</h4>
          <Descriptions
            size="small"
            column={1}
            items={[
              { key: "usage", label: "使用率", children: cpuUsageText(cpu.usage_percent) },
              { key: "cores", label: "核心数", children: `${cpu.cores} 核` },
              { key: "model", label: "型号", children: cpu.model || "未知" },
              { key: "load", label: "负载", children: loadAverageText(cpu) },
            ]}
          />
        </Col>
        <Col xs={24} sm={12}>
          <h4 className="monitor-subheading">内存</h4>
          <Descriptions
            size="small"
            column={1}
            items={[
              { key: "percent", label: "使用率", children: formatPercent(memory.used_percent) },
              {
                key: "used",
                label: "已用",
                children: `${formatBytes(memory.used_bytes)} / ${formatBytes(memory.total_bytes)}`,
              },
              { key: "available", label: "可用", children: formatBytes(memory.available_bytes) },
              {
                key: "swap",
                label: "Swap",
                children: `${formatBytes(memory.swap_used_bytes)} / ${formatBytes(memory.swap_total_bytes)}（${formatPercent(memory.swap_used_percent)}）`,
              },
            ]}
          />
        </Col>
        <Col xs={24} sm={12}>
          <h4 className="monitor-subheading">网络</h4>
          <Descriptions
            size="small"
            column={1}
            items={[
              { key: "rxRate", label: "接收速率", children: formatRate(network.rx_bytes_per_second) },
              { key: "txRate", label: "发送速率", children: formatRate(network.tx_bytes_per_second) },
              { key: "rxTotal", label: "累计接收", children: formatBytes(network.rx_bytes_total) },
              { key: "txTotal", label: "累计发送", children: formatBytes(network.tx_bytes_total) },
            ]}
          />
        </Col>
        <Col xs={24} sm={12}>
          <h4 className="monitor-subheading">进程</h4>
          <Descriptions
            size="small"
            column={1}
            items={[
              { key: "pid", label: "PID", children: String(process.pid) },
              { key: "rss", label: "RSS", children: formatBytes(process.rss_bytes) },
              { key: "vms", label: "虚拟内存", children: formatBytes(process.vms_bytes) },
              { key: "goroutines", label: "Goroutines", children: String(process.goroutines) },
              { key: "heap", label: "堆内存", children: formatBytes(process.heap_alloc_bytes) },
            ]}
          />
        </Col>
      </Row>
      {interfaces.length > 0 && (
        <>
          <h4 className="monitor-subheading">网卡</h4>
          <Flex wrap gap={6} className="monitor-tags">
            {interfaces.map((item) => (
              <Tag key={item.name}>
                {item.name}：接收 {formatRate(item.rx_bytes_per_second)} / 发送 {formatRate(item.tx_bytes_per_second)}
              </Tag>
            ))}
          </Flex>
        </>
      )}
      {systemNotes.length > 0 && (
        <ul className="monitor-error-list secondary monitor-note">
          {systemNotes.map((note, index) => <li key={index}>{note}</li>)}
        </ul>
      )}
      <p className="secondary monitor-note">
        采集于 {formatDateTime(system.collected_at)}（窗口 {system.window_seconds} 秒）；首次采样时速率与 CPU 使用率可能为未知。
      </p>
    </Card>
  );
}

// ServiceInfoCard renders the static service facts last: they never change
// between refreshes and would otherwise crowd the task metrics.
function ServiceInfoCard({ service }: { service?: MonitorService }) {
  return (
    <Card size="small" title="服务信息" className="monitor-service-info">
      <Descriptions
        size="small"
        column={{ xs: 1, sm: 2 }}
        items={[
          { key: "version", label: "版本", children: service?.version || "—" },
          { key: "uptime", label: "运行时长", children: formatDuration(service?.uptime_seconds) },
          { key: "backend", label: "后端", children: service?.backend || "—" },
          { key: "collection", label: "集合", children: service?.collection || "—" },
          { key: "upload", label: "上传模式", children: service?.upload_mode || "—" },
          { key: "delivery", label: "投递方式", children: service?.delivery || "—" },
          { key: "pdf", label: "PDF 引擎", children: service?.pdf_engine || "—" },
          { key: "started", label: "启动时间", children: formatDateTime(service?.started_at) },
        ]}
      />
    </Card>
  );
}

// OverviewTab keeps the page-level health banner together with the metrics,
// host resources and static service facts. 关键指标 runs the full width so its
// five counters sit on one line; 服务信息 and the host-resource sample then
// share the two-column row below it.
function OverviewTab({ overview }: { overview: MonitorOverview }) {
  const status = overallStatusOf(overview);
  const errors = overview.errors ?? [];
  const degradedNotes = degradedDependencyNotes(overview);
  return (
    <Flex vertical gap={16}>
      {status === "error" && (
        <Alert
          className="page-alert"
          type="error"
          showIcon
          title="服务状态异常"
          description={errors.length ? (
            <ul className="monitor-error-list">
              {errors.map((message, index) => <li key={index}>{message}</li>)}
            </ul>
          ) : "后端未返回具体错误，请查看下方依赖状态。"}
        />
      )}
      {status === "degraded" && (
        <Alert
          className="page-alert"
          type="warning"
          showIcon
          title="服务存在降级"
          description={degradedNotes.length ? (
            <ul className="monitor-error-list">
              {degradedNotes.map((note, index) => <li key={index}>{note}</li>)}
            </ul>
          ) : "后端未返回具体降级详情，请查看下方依赖状态。"}
        />
      )}
      <div className="monitor-stack">
        {/* 关键指标整行铺开（五个指标一行），其余两块按最小列高填充。 */}
        <KeyMetricsCard overview={overview} />
        <MonitorMasonry blocks={[
          { key: "service", node: <ServiceInfoCard service={overview.service} /> },
          { key: "system", node: <SystemResourcesCard system={overview.system} /> },
        ]} />
      </div>
    </Flex>
  );
}

// DependencyCard narrows the probe table to one component tab. Keeping the same
// table keeps the status/latency/detail vocabulary identical everywhere.
function DependencyCard({ overview, names }: { overview: MonitorOverview; names: string[] }) {
  const dependencies = overview.dependencies.filter((dependency) => names.includes(dependency.name));
  return (
    <Card size="small" title="依赖状态">
      <section className="monitor-block" aria-label="依赖状态">
        <DependencyTable dependencies={dependencies} />
      </section>
    </Card>
  );
}

// RequeueJobsAction is the「立即重投」button placed under the stuck-jobs
// counter. It requeues due retries and expired leases now instead of waiting
// for the worker's periodic sweep; the backend keeps the attempt budget, so
// this cannot force a fourth execution. The result count and follow-up
// refresh are handled by the shared runner in MonitorPage.
function RequeueJobsAction() {
  const actions = useMonitorActions();
  const [running, setRunning] = useState(false);
  if (!actions?.capabilities?.requeue_jobs) return null;
  return (
    <Button
      size="small"
      loading={running}
      onClick={() => {
        setRunning(true);
        void actions.requeueJobs(() => requeueDocumentJobs()).finally(() => setRunning(false));
      }}
    >
      立即重投
    </Button>
  );
}

// RestartConnectorAction is the「重启连接器」button on one capture row. It
// shows when the connector reports failed tasks or an exhausted restart
// budget — the two states an operator remedies by hand.
function RestartConnectorAction({ connector }: { connector?: MonitorPipelineConnector | null }) {
  const actions = useMonitorActions();
  const [running, setRunning] = useState(false);
  const name = connector?.connector;
  const needed = !!connector && (connector.failed_tasks > 0 || connector.restart_exhausted);
  if (!actions?.capabilities?.restart_connectors || !needed || !name) return null;
  return (
    <Button
      size="small"
      loading={running}
      onClick={() => {
        setRunning(true);
        void actions
          .restartConnector(() => restartMonitorConnector(name), name)
          .finally(() => setRunning(false));
      }}
    >
      重启连接器
    </Button>
  );
}

// WorkerScaleCard is the document-worker capacity card on the Worker tab. It
// reads the Deployment replica state (desired vs ready) and applies a new
// replica count through the same merge patch kubectl scale uses. Outside a
// cluster (no service account / RBAC) the card reports 未启用 instead of
// pretending to work.
function WorkerScaleCard({ active }: { active: boolean }) {
  const actions = useMonitorActions();
  const [deployment, setDeployment] = useState<MonitorWorkerDeployment>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [replicas, setReplicas] = useState<number | null>(null);
  const [applying, setApplying] = useState(false);
  const request = useRef<AbortController | null>(null);
  const enabled = !!actions?.capabilities?.scale_worker;

  const load = useCallback(async () => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setLoading(true);
    try {
      const view = await fetchWorkerDeployment(controller.signal);
      if (!controller.signal.aborted) {
        setDeployment(view);
        setReplicas(view.desired_replicas);
        setError("");
      }
    } catch (cause) {
      if (!controller.signal.aborted) setError(errorText(cause));
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, []);

  useEffect(() => {
    if (!enabled || !active) return;
    void load();
    return () => request.current?.abort();
  }, [enabled, active, load]);

  if (!enabled) {
    return (
      <section className="monitor-block" aria-label="Worker 扩缩容">
        <p className="secondary monitor-note">
          扩缩容操作未启用：需要在 Kubernetes 内部署 rag-service 并应用 RBAC 清单
          （deploy/k8s/base/rbac-monitor-ops.yaml）。Compose/宿主机部署不受影响。
        </p>
      </section>
    );
  }
  if (error) {
    return (
      <section className="monitor-block" aria-label="Worker 扩缩容">
        <Alert type="error" showIcon title="副本状态读取失败" description={<span className="preserve-lines">{error}</span>} />
      </section>
    );
  }
  return (
    <section className="monitor-block" aria-label="Worker 扩缩容">
      <Flex vertical gap={8}>
        <Flex wrap gap={12} align="center">
          <span className="secondary">
            {deployment
              ? `期望副本 ${deployment.desired_replicas}　就绪 ${deployment.ready_replicas}`
              : loading ? "正在读取副本状态" : "副本状态未知"}
          </span>
          <InputNumber
            size="small"
            min={0}
            max={64}
            value={replicas}
            onChange={(value) => setReplicas(typeof value === "number" ? value : null)}
            aria-label="目标副本数"
          />
          <Button
            size="small"
            loading={applying}
            disabled={replicas === null || (deployment !== undefined && replicas === deployment.desired_replicas)}
            onClick={() => {
              if (replicas === null || !actions) return;
              setApplying(true);
              void actions.scale(() => scaleWorker(replicas)).finally(() => setApplying(false));
            }}
          >
            应用副本数
          </Button>
        </Flex>
        <p className="secondary monitor-note">
          缩容时正在处理的分区会 rebalance 给存活副本；未提交消息重新投递，由 schedule_version 与租约保证幂等。
          修改的是 Deployment 的 spec.replicas（与 kubectl scale 等价）。
        </p>
      </Flex>
    </section>
  );
}
function DocumentJobsBlock({ queues }: { queues: MonitorQueues | null }) {
  const jobs = queues?.document_jobs;
  const stages = Object.entries(jobs?.by_stage || {}).map(([stage, count]) => ({ stage, count }));
  return (
    <section className="monitor-block" aria-label="文档任务队列">
      <StatusCountTags counts={jobs?.by_status} labels={documentJobStatusLabels} />
      <Descriptions
        size="small"
        column={1}
        items={[
          { key: "oldest", label: "最老排队", children: queues ? formatDuration(jobs?.oldest_queued_seconds) : "未知" },
          {
            key: "stuck",
            label: "卡死处理中",
            children: (
              <Flex gap={8} align="center">
                <span>{queues ? `${jobs?.stuck_processing ?? 0} 个` : "未知"}</span>
                <RequeueJobsAction />
              </Flex>
            ),
          },
          { key: "failed", label: "近 24 小时失败", children: queues ? `${jobs?.failed_recent ?? 0} 个` : "未知" },
        ]}
      />
      <h4 className="monitor-subheading">阶段分布</h4>
      {stages.length ? (
        <Flex wrap gap={6} className="monitor-tags">
          {stages.map(({ stage, count }) => (
            <Tag key={stage}>
              {uploadJobStageLabels[stage] || stage} {count}
            </Tag>
          ))}
        </Flex>
      ) : (
        // 没有处理中任务时不渲染空表格，只留一行说明。
        <span className="secondary">{queues ? "暂无处理中任务" : "状态库暂不可用"}</span>
      )}
    </section>
  );
}

// UploadsBlock lists the four disjoint upload operation buckets. A missing
// section stays 未知 instead of printing fake zeros.
function UploadsBlock({ uploads }: { uploads?: MonitorQueues["uploads"] | null }) {
  if (!uploads) {
    return (
      <section className="monitor-block" aria-label="上传队列">
        <span className="secondary">上传操作：未知</span>
      </section>
    );
  }
  return (
    <section className="monitor-block" aria-label="上传队列">
      <Row gutter={[12, 8]}>
        <Col xs={12} sm={12}>
          <Statistic className="monitor-stat" title="进行中" value={unknownOrNumber(uploads.in_progress)} />
        </Col>
        <Col xs={12} sm={12}>
          <Statistic className="monitor-stat" title="失败" value={unknownOrNumber(uploads.failed)} />
        </Col>
        <Col xs={12} sm={12}>
          <Statistic className="monitor-stat" title="回滚中" value={unknownOrNumber(uploads.rolling_back)} />
        </Col>
        <Col xs={12} sm={12}>
          <Statistic className="monitor-stat" title="待清理" value={unknownOrNumber(uploads.cleanup_pending)} />
        </Col>
      </Row>
      <span className="secondary">
        未完成操作合计：{unknownOrNumber(uploads.unfinished)}
      </span>
      <p className="secondary monitor-note">
        未完成操作 = 进行中 + 失败/回滚待处理 + 待清理，不代表当前正在上传或入库的文件数。
      </p>
    </section>
  );
}

// LibraryActivityBlock shows the state-database counts: documents, knowledge
// bases, artifact snapshots and the recent search volume.
function LibraryActivityBlock({ library, activity }: {
  library: MonitorLibrary | null;
  activity: MonitorActivity | null;
}) {
  return (
    <section className="monitor-block" aria-label="库与活动">
      <Descriptions
        size="small"
        column={{ xs: 1, sm: 2 }}
        items={[
          { key: "documents", label: "有效文档", children: library ? unknownOrNumber(library.documents) : "未知" },
          { key: "knowledge-bases", label: "知识库", children: library ? unknownOrNumber(library.knowledge_bases) : "未知" },
          { key: "artifacts", label: "artifact 快照", children: library ? unknownOrNumber(library.artifact_snapshots) : "未知" },
          {
            key: "searches",
            label: `近 ${activity?.window_hours ?? 24} 小时检索次数`,
            children: activity ? unknownOrNumber(activity.searches) : "未知",
          },
        ]}
      />
      <p className="secondary monitor-note">
        统计来自状态库；artifact 快照按 ID 独立读取，检索为同步执行，不经过 Kafka。
      </p>
    </section>
  );
}

// CacheBlock shows the read-model revision watermark. The explanatory note is
// part of the contract: dirty keys and refresh failures are not the same thing.
function CacheBlock({ pipeline }: { pipeline: MonitorPipeline | null }) {
  const cache = pipeline?.cache;
  return (
    <section className="monitor-block" aria-label="缓存水位">
      <Descriptions
        size="small"
        column={1}
        items={[
          {
            key: "status",
            label: "状态",
            children: <PipelineStatusTag status={cache?.status} />,
          },
          {
            key: "dirty",
            label: "脏 key",
            children: cache ? unknownOrNumber(cache.dirty_keys) : "未知",
          },
          {
            key: "failures",
            label: "刷新失败",
            children: cache ? unknownOrNumber(cache.refresh_failures) : "未知",
          },
        ]}
      />
      <p className="secondary monitor-note">
        脏 key = refreshed_revision 小于 revision；刷新失败 = last_error 非空。数字未知表示状态库或统计不可读。
      </p>
    </section>
  );
}

// KafkaGroupsBlock renders both independent consumer groups: document and cache.
// Offsets stay 未知 when Kafka does not report them.
function KafkaGroupsBlock({ pipeline }: { pipeline: MonitorPipeline | null }) {
  const documentGroup = pipeline?.kafka?.groups?.document;
  const cacheGroup = pipeline?.kafka?.groups?.cache;
  return (
    <section className="monitor-block" aria-label="Kafka 消费进度">
      <Flex vertical gap={12}>
        {([
          ["Kafka 文档消费", documentGroup],
          ["Kafka 缓存消费", cacheGroup],
        ] as const).map(([label, group]) => (
          <PipelineRow key={label} label={label} status={group?.status} detail={group?.detail}>
            <span className="secondary break-text">
              Topic：{group?.topic || "未知"}　Consumer Group：{group?.group || "未知"}
            </span>
            <span className="break-text">{kafkaGroupLagText(group)}</span>
          </PipelineRow>
        ))}
      </Flex>
    </section>
  );
}

// CaptureRow is one named Kafka Connect connector row. The status/detail line
// answers "is it delivering?", and the nested lines answer the operator's
// follow-ups: which worker, which task, why it failed, whether an automatic
// restart is in flight or exhausted, and the last recovery result. The two rows
// are never merged: document and cache delivery are independent.
function CaptureRow({ label, connector }: {
  label: string;
  connector?: MonitorPipelineConnector | null;
}) {
  const failure = connectorFailureText(connector);
  return (
    <PipelineRow label={label} status={connector?.status} detail={connector?.detail}>
      <Flex vertical gap={2} className="secondary break-text">
        <span>{`连接器：${connector?.connector || "未知"}（${connector?.connector_state || "未知"}）`}</span>
        <span>{connectorWorkerText(connector)}</span>
        <span>{connectorTaskText(connector)}</span>
        {failure ? <span>{failure}</span> : null}
        <span>{connectorRestartText(connector)}</span>
        <span>{captureProgressText(connector?.capture_progress)}</span>
        <RestartConnectorAction connector={connector} />
      </Flex>
    </PipelineRow>
  );
}

// WorkerBlock shows the document worker heartbeat. Only a fresh heartbeat row
// proves the independent worker process is alive.
function WorkerBlock({ pipeline }: { pipeline: MonitorPipeline | null }) {
  return (
    <section className="monitor-block" aria-label="Worker 心跳">
      <PipelineRow
        label="文档 Worker"
        status={pipeline?.worker?.status}
        detail={pipeline?.worker?.detail}
      />
    </section>
  );
}

// DocumentJobsStatusBlock is the pipeline-level document job view, distinct from
// the state-database queue counts.
function DocumentJobsStatusBlock({ pipeline }: { pipeline: MonitorPipeline | null }) {
  return (
    <section className="monitor-block" aria-label="文档任务状态">
      <PipelineRow
        label="文档任务状态"
        status={pipeline?.document_jobs?.status}
        detail={pipeline?.document_jobs?.detail}
      />
    </section>
  );
}

// StageDistributionBlock isolates the by_stage view for the worker tab.
function StageDistributionBlock({ queues }: { queues: MonitorQueues | null }) {
  const jobs = queues?.document_jobs;
  const stages = Object.entries(jobs?.by_stage || {}).map(([stage, count]) => ({ stage, count }));
  return (
    <section className="monitor-block" aria-label="阶段分布">
      {stages.length ? (
        <Flex wrap gap={6} className="monitor-tags">
          {stages.map(({ stage, count }) => (
            <Tag key={stage}>
              {uploadJobStageLabels[stage] || stage} {count}
            </Tag>
          ))}
        </Flex>
      ) : (
        <span className="secondary">{queues ? "暂无处理中任务" : "状态库暂不可用"}</span>
      )}
    </section>
  );
}

// logFileNote explains why the file source cannot be read, without pretending a
// missing configuration is an empty log.
function logFileNote(file: MonitorLogFile) {
  const reason = file.note || "请先配置日志文件路径";
  return `日志文件未配置：${reason}`;
}

// MonitorLogPanel is the single log viewer reused by every component tab and the
// 全部日志 tab. It defaults to the in-memory source, follows the page's auto
// refresh switch while its tab is active, and never invents rows when the
// backend reports the source unavailable.
type MonitorLogPanelProps = {
  component?: MonitorTabComponent;
  autoRefresh: boolean;
  active: boolean;
  allFilters?: boolean;
  title: string;
  panelLabel: string;
};

function MonitorLogPanel({
  component,
  autoRefresh,
  active,
  allFilters = false,
  title,
  panelLabel,
}: MonitorLogPanelProps) {
  const [source, setSource] = useState<MonitorLogSource>("memory");
  const [level, setLevel] = useState("");
  const [componentFilter, setComponentFilter] = useState("");
  const [keyword, setKeyword] = useState("");
  const [draftKeyword, setDraftKeyword] = useState("");
  const [data, setData] = useState<MonitorLogPage>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const request = useRef<AbortController | null>(null);
  const effectiveComponent = allFilters ? componentFilter : (component ?? "");
  const limit = allFilters ? monitorAllLogLimit : monitorComponentLogLimit;

  const load = useCallback(async () => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setLoading(true);
    try {
      const page = await fetchMonitorLogs(
        {
          source,
          component: effectiveComponent || undefined,
          level: level || undefined,
          query: keyword || undefined,
          limit,
        },
        controller.signal,
      );
      if (!controller.signal.aborted) {
        setData(page);
        setError("");
      }
    } catch (cause) {
      if (!controller.signal.aborted) setError(errorText(cause));
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, [source, effectiveComponent, level, keyword, limit]);

  useEffect(() => {
    void load();
    return () => request.current?.abort();
  }, [load]);

  useEffect(() => {
    if (!autoRefresh || !active) return;
    const timer = setInterval(() => void load(), monitorRefreshMs);
    return () => clearInterval(timer);
  }, [autoRefresh, active, load]);

  // The component filter offers the frozen vocabulary first and appends any
  // future component the backend reports, so an upgrade never hides log rows.
  const componentOptions = useMemo(() => {
    const counted = new Map((data?.components ?? []).map((item) => [item.id, item]));
    const options = Object.entries(monitorComponentLabels).map(([id, label]) => {
      const item = counted.get(id);
      return { value: id, label: item ? `${item.label || label}（${item.count}）` : label };
    });
    for (const item of data?.components ?? []) {
      if (!(item.id in monitorComponentLabels)) {
        options.push({ value: item.id, label: `${item.label || item.id}（${item.count}）` });
      }
    }
    return options;
  }, [data?.components]);

  const entries = data?.entries ?? [];
  const file = data?.file;
  const unavailableNote = data && !data.available
    ? (data.notes.length
      ? data.notes.join("；")
      : `${source === "file" ? "文件日志" : "内存日志"}不可用`)
    : "";
  const fileNote = source === "file" && file && !file.configured ? logFileNote(file) : "";
  const pageNotes = data?.notes ?? [];

  return (
    <Card
      size="small"
      title={title}
      className="monitor-log-card"
      extra={(
        <Button
          size="small"
          icon={<ReloadOutlined />}
          loading={loading}
          onClick={() => void load()}
        >
          刷新日志
        </Button>
      )}
    >
      <section aria-label={panelLabel} className="monitor-log-panel">
        <Flex wrap gap={8} align="center" className="monitor-log-filters">
          <Segmented
            size="small"
            options={logSourceOptions}
            value={source}
            onChange={(value) => setSource(value as MonitorLogSource)}
          />
          {allFilters && (
            <Select
              size="small"
              virtual={false}
              aria-label={`${panelLabel}组件`}
              value={componentFilter}
              onChange={(value) => setComponentFilter(value)}
              options={[{ value: "", label: "全部组件" }, ...componentOptions]}
              style={{ minWidth: 160 }}
            />
          )}
          <Select
            size="small"
            virtual={false}
            aria-label={`${panelLabel}级别`}
            value={level}
            onChange={(value) => setLevel(value)}
            options={logLevelOptions}
            style={{ minWidth: 110 }}
          />
          <Input
            size="small"
            allowClear
            aria-label={`${panelLabel}关键字`}
            placeholder="关键字"
            value={draftKeyword}
            onChange={(event) => {
              const value = event.target.value;
              setDraftKeyword(value);
              // 清空输入即恢复无关键字，不必再按查询。
              if (!value) setKeyword("");
            }}
            onPressEnter={() => setKeyword(draftKeyword.trim())}
            style={{ maxWidth: 220, minWidth: 140 }}
          />
          <Button size="small" onClick={() => setKeyword(draftKeyword.trim())}>查询</Button>
        </Flex>

        {unavailableNote && (
          <Alert
            className="page-alert"
            type="warning"
            showIcon
            title="日志不可用"
            description={<span className="preserve-lines">{unavailableNote}</span>}
          />
        )}
        {fileNote && (
          // 文件未配置通常同时让来源不可用；把配置原因单独再说明一次，
          // 避免操作员只看到“不可用”却不知道要配置什么。
          <Alert
            className="page-alert"
            type="info"
            showIcon
            title="日志文件未配置"
            description={<span className="preserve-lines">{fileNote}</span>}
          />
        )}
        {error && (
          <Alert
            className="page-alert"
            type="error"
            showIcon
            title="日志加载失败"
            description={<span className="preserve-lines">{error}</span>}
          />
        )}

        {allFilters && (
          <Descriptions
            size="small"
            className="monitor-log-file"
            column={{ xs: 1, sm: 2, lg: 4 }}
            items={[
              { key: "path", label: "配置路径", children: file?.configured ? file.path : "未配置" },
              { key: "size", label: "大小", children: file ? formatBytes(file.size_bytes) : "未知" },
              { key: "truncated", label: "截断", children: file ? (file.truncated ? "已截断（仅保留最近日志）" : "未截断") : "未知" },
              { key: "source", label: "当前来源", children: source === "file" ? "文件" : "内存" },
            ]}
          />
        )}

        <Table<MonitorLogEntry>
          rowKey="seq"
          size="small"
          loading={loading}
          dataSource={entries}
          locale={{ emptyText: "暂无日志" }}
          scroll={{ x: 860 }}
          pagination={{
            pageSize: allFilters ? 20 : 10,
            size: "small",
            hideOnSinglePage: true,
            showSizeChanger: false,
          }}
          expandable={{
            rowExpandable: (entry) => Object.keys(entry.attrs || {}).length > 0,
            expandedRowRender: (entry) => (
              <Descriptions
                size="small"
                column={1}
                className="monitor-log-attrs"
                items={Object.entries(entry.attrs || {}).map(([key, value]) => ({
                  key,
                  label: key,
                  children: value,
                }))}
              />
            ),
          }}
          columns={[
            {
              title: "时间",
              dataIndex: "time",
              width: 180,
              render: (value: string) => <span className="break-text">{formatDateTime(value)}</span>,
            },
            {
              title: "级别",
              dataIndex: "level",
              width: 90,
              render: (value: string) => (
                <Tag color={logLevelColors[value] || "default"}>
                  {logLevelLabels[value] || value || "未知"}
                </Tag>
              ),
            },
            {
              title: "组件",
              dataIndex: "component",
              width: 130,
              render: (value: string, entry) => (
                <span className="break-text">
                  {entry.component_label || monitorComponentLabels[value] || value || "未知"}
                </span>
              ),
            },
            {
              title: "内容",
              dataIndex: "message",
              render: (value: string) => <span className="break-text">{value}</span>,
            },
            {
              // source is the caller code location (repo-relative file:line),
              // never the memory/file log source; that is the page-level filter.
              title: "代码位置",
              dataIndex: "source",
              width: 220,
              render: (value: string) => (
                <span className="break-text">{value || "—"}</span>
              ),
            },
          ]}
        />

        {pageNotes.length > 0 && !unavailableNote && (
          <ul className="secondary monitor-error-list monitor-note">
            {pageNotes.map((note, index) => <li key={index}>{note}</li>)}
          </ul>
        )}
      </section>
    </Card>
  );
}

// ComponentTab maps one component tab to its blocks plus that component's log
// panel. Every existing monitor block is preserved here or on the overview tab.
function ComponentTab({ tab, overview, autoRefresh, active }: {
  tab: MonitorTabComponent;
  overview: MonitorOverview;
  autoRefresh: boolean;
  active: boolean;
}) {
  const label = monitorComponentLabels[tab];
  const logPanel = (
    <MonitorLogPanel
      component={tab}
      autoRefresh={autoRefresh}
      active={active}
      title={`${label}日志`}
      panelLabel={`${label}日志面板`}
    />
  );

  switch (tab) {
    case "mysql":
      return (
        <div className="monitor-stack">
          {/* 依赖表需要整行宽度；其余三块按最小列高填充（宽屏三列）。 */}
          <DependencyCard overview={overview} names={["mysql"]} />
          <MonitorMasonry dense blocks={[
            { key: "jobs", node: (
              <Card size="small" title="文档任务队列">
                <DocumentJobsBlock queues={overview.queues} />
              </Card>
            ) },
            { key: "uploads", node: (
              <Card size="small" title="上传操作">
                <UploadsBlock uploads={overview.queues?.uploads} />
              </Card>
            ) },
            { key: "library", node: (
              <Card size="small" title="库与活动">
                <LibraryActivityBlock library={overview.library} activity={overview.activity} />
              </Card>
            ) },
          ]} />
          {logPanel}
        </div>
      );
    case "redis":
      return (
        <div className="monitor-stack">
          <MonitorMasonry blocks={[
            { key: "dependency", node: <DependencyCard overview={overview} names={["redis"]} /> },
            { key: "cache", node: (
              <Card size="small" title="缓存水位">
                <CacheBlock pipeline={overview.pipeline} />
              </Card>
            ) },
          ]} />
          {logPanel}
        </div>
      );
    case "qdrant":
      return (
        <div className="monitor-stack">
          <MonitorMasonry blocks={[
            { key: "dependency", node: <DependencyCard overview={overview} names={["qdrant"]} /> },
            { key: "collection", node: (
              <Card size="small" title="Qdrant 集合">
                <section className="monitor-block" aria-label="Qdrant 集合">
                  <Descriptions
                    size="small"
                    column={1}
                    items={[
                      { key: "collection", label: "集合名", children: overview.service?.collection || "未知" },
                    ]}
                  />
                  <p className="secondary monitor-note">
                    依赖行中的“向量点”为该集合当前存储的点数；集合名来自服务配置。
                  </p>
                </section>
              </Card>
            ) },
          ]} />
          {logPanel}
        </div>
      );
    case "kafka":
      return (
        <div className="monitor-stack">
          <MonitorMasonry blocks={[
            { key: "dependency", node: <DependencyCard overview={overview} names={["kafka"]} /> },
            { key: "delivery", node: (
              <Card size="small" title="文档任务投递状态">
                <DocumentJobsStatusBlock pipeline={overview.pipeline} />
              </Card>
            ) },
          ]} />
          {/* 消费进度行包含 topic/group/位点长文本，保留整行宽度换行。 */}
          <Card size="small" title="Kafka 消费进度">
            <KafkaGroupsBlock pipeline={overview.pipeline} />
          </Card>
          {logPanel}
        </div>
      );
    case "pdf":
      return (
        <div className="monitor-stack">
          <MonitorMasonry blocks={[
            { key: "dependency", node: <DependencyCard overview={overview} names={["pdf-service"]} /> },
            { key: "engine", node: (
              <Card size="small" title="PDF 引擎">
                <section className="monitor-block" aria-label="PDF 引擎">
                  <Descriptions
                    size="small"
                    column={1}
                    items={[
                      { key: "engine", label: "引擎", children: overview.service?.pdf_engine || "未知" },
                    ]}
                  />
                  <p className="secondary monitor-note">
                    PDF 解析服务是可选独立微服务；依赖行的延迟与详情来自其健康探针。版面/OCR/表格/公式/图片描述由服务端配置的远端视觉模型提供。
                  </p>
                </section>
              </Card>
            ) },
          ]} />
          {logPanel}
        </div>
      );
    case "worker":
      return (
        <div className="monitor-stack">
          <MonitorMasonry dense blocks={[
            { key: "heartbeat", node: (
              <Card size="small" title="Worker 心跳">
                <WorkerBlock pipeline={overview.pipeline} />
              </Card>
            ) },
            { key: "delivery", node: (
              <Card size="small" title="文档任务状态">
                <DocumentJobsStatusBlock pipeline={overview.pipeline} />
              </Card>
            ) },
            { key: "scale", node: (
              <Card size="small" title="副本数（Kubernetes）">
                <WorkerScaleCard active={active} />
              </Card>
            ) },
            { key: "stages", node: (
              <Card size="small" title="阶段分布">
                <StageDistributionBlock queues={overview.queues} />
              </Card>
            ) },
          ]} />
          {logPanel}
        </div>
      );
    case "debezium":
      return (
        <div className="monitor-stack">
          {/* 文档与缓存采集是两条独立链路：各占一列，保持“互不合并”。 */}
          <section aria-label="采集连接器">
            <MonitorMasonry blocks={[
              { key: "document", node: (
                <Card size="small" title="文档采集连接器">
                  <CaptureRow label="文档采集" connector={overview.pipeline?.debezium?.tasks} />
                </Card>
              ) },
              { key: "cache", node: (
                <Card size="small" title="缓存采集连接器">
                  <CaptureRow label="缓存采集" connector={overview.pipeline?.debezium?.cache} />
                </Card>
              ) },
            ]} />
          </section>
          <Card size="small" title="文档任务投递状态">
            <DocumentJobsStatusBlock pipeline={overview.pipeline} />
          </Card>
          {logPanel}
        </div>
      );
    default:
      return null;
  }
}

// monitorTabs builds the top bar: overview, the seven component tabs in the
// frozen order, then the all-logs tab.
function monitorTabs(overview: MonitorOverview, autoRefresh: boolean, activeTab: string) {
  return [
    {
      key: "overview",
      label: "概览",
      children: <OverviewTab overview={overview} />,
    },
    ...monitorTabComponents.map((tab) => ({
      key: tab,
      label: monitorComponentLabels[tab],
      children: (
        <ComponentTab
          tab={tab}
          overview={overview}
          autoRefresh={autoRefresh}
          active={activeTab === tab}
        />
      ),
    })),
    {
      key: "logs",
      label: "全部日志",
      children: (
        <MonitorLogPanel
          allFilters
          autoRefresh={autoRefresh}
          active={activeTab === "logs"}
          title="全部日志"
          panelLabel="全部日志面板"
        />
      ),
    },
  ];
}

// MonitorPage is the read-only system overview for administrators. The top bar
// splits the snapshot by component, the overview tab leads with the task
// metrics and host resources, and every tab carries its own log panel. The
// quick facts (updated at, auto refresh) stay visible above the tabs.
export function MonitorPage() {
  const [overview, setOverview] = useState<MonitorOverview>();
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [activeTab, setActiveTab] = useState("overview");
  const request = useRef<AbortController | null>(null);
  const { message } = App.useApp();

  const load = useCallback(async () => {
    // A new request supersedes the previous one, so a slow response can never
    // overwrite a newer snapshot.
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setRefreshing(true);
    try {
      const snapshot = await fetchMonitorOverview(controller.signal);
      if (!controller.signal.aborted) {
        setOverview(snapshot);
        setError("");
      }
    } catch (cause) {
      if (!controller.signal.aborted) setError(errorText(cause));
    } finally {
      if (!controller.signal.aborted) {
        setRefreshing(false);
        setLoading(false);
      }
    }
  }, []);

  // The three operator-action runners. Each surfaces a result message and
  // refreshes the snapshot, so the operator immediately sees the effect
  // (requeued counters, connector state, replica counts).
  const actionContext = useMemo<MonitorActionContextValue>(() => ({
    capabilities: overview?.actions,
    refresh: () => void load(),
    requeueJobs: (run) => run().then((result) => {
      message.success(`已重投 ${result.requeued} 个到期/租约过期任务`);
      void load();
    }).catch((cause: unknown) => {
      message.error(`任务重投失败：${errorText(cause)}`);
    }),
    restartConnector: (run, connector) => run().then(() => {
      message.success(`已请求重启连接器 ${connector}（含任务）`);
      void load();
    }).catch((cause: unknown) => {
      message.error(`连接器重启失败：${errorText(cause)}`);
    }),
    scale: (run) => run().then((result) => {
      message.success(`已应用副本数 ${result.replicas}`);
      void load();
    }).catch((cause: unknown) => {
      message.error(`副本数修改失败：${errorText(cause)}`);
    }),
  }), [overview?.actions, load, message]);

  useEffect(() => {
    void load();
    return () => request.current?.abort();
  }, [load]);

  useEffect(() => {
    if (!autoRefresh) return;
    const timer = setInterval(() => void load(), monitorRefreshMs);
    return () => clearInterval(timer);
  }, [autoRefresh, load]);

  const status = overview ? overallStatusOf(overview) : undefined;

  return (
    <section aria-label="系统监控页面">
      <div className="page-heading">
        <div className="page-heading-title">
          <h1>系统监控</h1>
          {status && <Tag color={overallStatusColors[status]}>{overallStatusLabels[status]}</Tag>}
        </div>
        <div className="toolbar">
          <span className="secondary">
            {overview ? `更新于 ${formatClock(overview.generated_at)}` : "正在获取监控快照"}
          </span>
          <Tooltip title={`每 ${monitorRefreshMs / 1000} 秒自动刷新`}>
            <label className="monitor-auto-refresh">
              <span className="secondary">自动刷新</span>
              <Switch
                size="small"
                title="自动刷新"
                checked={autoRefresh}
                onChange={setAutoRefresh}
              />
            </label>
          </Tooltip>
          <Button
            icon={<ReloadOutlined />}
            aria-label="刷新监控数据"
            loading={refreshing}
            onClick={() => void load()}
          >
            刷新
          </Button>
        </div>
      </div>
      {error && (
        <Alert
          className="page-alert"
          type="error"
          showIcon
          title="监控数据加载失败"
          description={<span className="preserve-lines">{error}</span>}
          action={<Button size="small" onClick={() => void load()}>重试</Button>}
        />
      )}
      {loading && !overview ? (
        <Spin description="正在读取监控快照…" />
      ) : overview ? (
        <MonitorActionContext.Provider value={actionContext}>
          <Tabs
            className="monitor-tabs"
            activeKey={activeTab}
            onChange={setActiveTab}
            items={monitorTabs(overview, autoRefresh, activeTab)}
          />
        </MonitorActionContext.Provider>
      ) : null}
    </section>
  );
}
