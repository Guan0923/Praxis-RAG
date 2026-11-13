import { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  Button,
  Card,
  Col,
  Descriptions,
  Flex,
  Row,
  Spin,
  Statistic,
  Switch,
  Table,
  Tag,
  Tooltip,
} from "antd";
import { ReloadOutlined } from "@ant-design/icons";
import {
  errorText,
  fetchMonitorOverview,
  formatDateTime,
  formatDuration,
  monitorDependencyStatusColors,
  monitorDependencyStatusLabels,
  uploadJobStageLabels,
  type MonitorDependency,
  type MonitorDependencyStatus,
  type MonitorOverallStatus,
  type MonitorOverview,
  type MonitorPipelineKafkaGroup,
} from "./api";

// monitorRefreshMs is the auto-refresh period: the backend does not push
// snapshot updates, so the page polls. It stays off the request path and only
// re-fetches while the switch is on.
const monitorRefreshMs = 15_000;

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

// Queue statuses are rendered as compact count tags. document_jobs and
// search_tasks share the first four states.
const documentJobStatusLabels: Record<string, string> = {
  queued: "排队中",
  processing: "处理中",
  succeeded: "成功",
  failed: "失败",
};
const searchTaskStatusLabels: Record<string, string> = {
  queued: "排队中",
  processing: "处理中",
  succeeded: "成功",
  failed: "失败",
  timed_out: "超时",
};

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

// DependencyTable lists the five built-in probes. The horizontal scroll keeps
// the four columns readable on a phone without stretching the card, and the
// detail cell wraps long diagnostics instead of pushing the table wider.
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

// captureProgressText never invents a Debezium capture percentage: the service
// does not read Debezium offsets, so the only honest value is 未知.
function captureProgressText(progress?: number | null) {
  return typeof progress === "number" && Number.isFinite(progress)
    ? `采集进度：${progress}`
    : "采集进度：未知（需在 Debezium 侧查看）";
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
      ["Debezium 任务连接器", pipeline.debezium?.tasks],
      ["Debezium 缓存连接器", pipeline.debezium?.cache],
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

// MonitorSnapshot renders one successfully fetched overview. It is a separate
// component so every section can narrow null queues/pipeline/library/activity
// without repeating guards in the page shell.
function MonitorSnapshot({ overview }: { overview: MonitorOverview }) {
  const { service, dependencies, queues, pipeline, library, activity } = overview;
  const jobs = queues?.document_jobs;
  const pipelineJobs = pipeline?.document_jobs;
  const worker = pipeline?.worker;
  const debeziumTasks = pipeline?.debezium?.tasks;
  const debeziumCache = pipeline?.debezium?.cache;
  const kafkaGroups = pipeline?.kafka?.groups;
  const kafkaDocument = kafkaGroups?.document;
  const kafkaSearch = kafkaGroups?.search;
  const kafkaCache = kafkaGroups?.cache;
  const cache = pipeline?.cache;
  const searchTasks = queues?.search_tasks;
  const uploads = queues?.uploads;
  const stages = Object.entries(jobs?.by_stage || {}).map(([stage, count]) => ({ stage, count }));

  // The key metrics are the page's first screen: task health instead of static
  // service configuration. Each value degrades to 未知 when its section is
  // unavailable rather than pretending the queue is empty.
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
    {
      key: "result_snapshots",
      title: "当前结果快照",
      value: library ? unknownOrNumber(library.result_snapshots) : "未知",
    },
  ];

  return (
    <Flex vertical gap={16}>
      <Card size="small" title="关键指标">
        <Row gutter={[16, 16]}>
          {metrics.map((metric) => (
            <Col key={metric.key} xs={12} sm={8} xl={6}>
              <Statistic className="monitor-stat" title={metric.title} value={metric.value} />
            </Col>
          ))}
        </Row>
      </Card>

      <Card size="small" title="依赖与投递">
        <Row gutter={[24, 16]}>
          <Col xs={24} xl={12}>
            <section className="monitor-block" aria-label="依赖状态">
              <h3>依赖状态</h3>
              <DependencyTable dependencies={dependencies} />
            </section>
          </Col>
          <Col xs={24} xl={12}>
            <section className="monitor-block" aria-label="投递与消费进度">
              <h3>投递与消费进度</h3>
              <Flex vertical gap={12}>
                <div className="monitor-progress-row">
                  <Flex wrap gap={8} align="center">
                    <strong>Debezium 任务连接器</strong>
                    <PipelineStatusTag status={debeziumTasks?.status} />
                    <span className="secondary break-text">{debeziumTasks?.detail || "未知"}</span>
                  </Flex>
                  <span className="break-text">{captureProgressText(debeziumTasks?.capture_progress)}</span>
                </div>
                <div className="monitor-progress-row">
                  <Flex wrap gap={8} align="center">
                    <strong>Debezium 缓存连接器</strong>
                    <PipelineStatusTag status={debeziumCache?.status} />
                    <span className="secondary break-text">{debeziumCache?.detail || "未知"}</span>
                  </Flex>
                  <span className="break-text">{captureProgressText(debeziumCache?.capture_progress)}</span>
                </div>
                {([
                  ["Kafka 文档消费", kafkaDocument],
                  ["Kafka 搜索消费", kafkaSearch],
                  ["Kafka 缓存消费", kafkaCache],
                ] as const).map(([label, group]) => (
                  <div className="monitor-progress-row" key={label}>
                    <Flex wrap gap={8} align="center">
                      <strong>{label}</strong>
                      <PipelineStatusTag status={group?.status} />
                      <span className="secondary break-text">{group?.detail || "未知"}</span>
                    </Flex>
                    <span className="secondary break-text">
                      Topic：{group?.topic || "未知"}　Consumer Group：{group?.group || "未知"}
                    </span>
                    <span className="break-text">{kafkaGroupLagText(group)}</span>
                  </div>
                ))}
                <div className="monitor-progress-row">
                  <Flex wrap gap={8} align="center">
                    <strong>文档 Worker</strong>
                    <PipelineStatusTag status={worker?.status} />
                    <span className="secondary break-text">{worker?.detail || "未知"}</span>
                  </Flex>
                </div>
                <div className="monitor-progress-row">
                  <Flex wrap gap={8} align="center">
                    <strong>文档任务状态</strong>
                    <PipelineStatusTag status={pipelineJobs?.status} />
                    <span className="secondary break-text">{pipelineJobs?.detail || "未知"}</span>
                  </Flex>
                </div>
              </Flex>
            </section>
          </Col>
        </Row>
      </Card>

      <Card size="small" title="队列与记录">
        <Row gutter={[24, 16]}>
          <Col xs={24} xl={12}>
            <section className="monitor-block" aria-label="文档任务队列">
              <h3>文档任务</h3>
              <StatusCountTags counts={jobs?.by_status} labels={documentJobStatusLabels} />
              <Descriptions
                size="small"
                column={1}
                items={[
                  { key: "oldest", label: "最老排队", children: queues ? formatDuration(jobs?.oldest_queued_seconds) : "未知" },
                  { key: "stuck", label: "卡死处理中", children: queues ? `${jobs?.stuck_processing ?? 0} 个` : "未知" },
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
          </Col>
          <Col xs={24} xl={12}>
            <Flex vertical gap={20}>
              <section className="monitor-block" aria-label="缓存水位">
                <h3>缓存水位</h3>
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
              <section className="monitor-block" aria-label="检索任务队列">
                <h3>检索任务</h3>
                <StatusCountTags counts={searchTasks?.by_status} labels={searchTaskStatusLabels} />
                <Descriptions
                  size="small"
                  column={1}
                  items={[
                    { key: "oldest", label: "最老活跃", children: queues ? formatDuration(searchTasks?.oldest_active_seconds) : "未知" },
                    { key: "timeout", label: "近 24 小时超时", children: queues ? `${searchTasks?.timed_out_recent ?? 0} 个` : "未知" },
                    { key: "overdue", label: "逾期未收敛", children: queues ? `${searchTasks?.overdue_active ?? 0} 个` : "未知" },
                  ]}
                />
              </section>
            </Flex>
          </Col>
        </Row>
        <section className="monitor-block monitor-uploads" aria-label="上传队列">
          <h3>上传操作</h3>
          {uploads ? (
            <>
              <Row gutter={[12, 12]}>
                <Col xs={12} sm={6}>
                  <Statistic className="monitor-stat" title="进行中" value={unknownOrNumber(uploads.in_progress)} />
                </Col>
                <Col xs={12} sm={6}>
                  <Statistic className="monitor-stat" title="失败" value={unknownOrNumber(uploads.failed)} />
                </Col>
                <Col xs={12} sm={6}>
                  <Statistic className="monitor-stat" title="回滚中" value={unknownOrNumber(uploads.rolling_back)} />
                </Col>
                <Col xs={12} sm={6}>
                  <Statistic className="monitor-stat" title="待清理" value={unknownOrNumber(uploads.cleanup_pending)} />
                </Col>
              </Row>
              <span className="secondary">
                未完成操作合计：{unknownOrNumber(uploads.unfinished)}
              </span>
              <p className="secondary monitor-note">
                未完成操作 = 进行中 + 失败/回滚待处理 + 待清理，不代表当前正在上传或入库的文件数。
              </p>
            </>
          ) : (
            <span className="secondary">上传操作：未知</span>
          )}
        </section>
      </Card>

      <Card size="small" title="服务信息" className="monitor-service-info">
        <Descriptions
          size="small"
          column={{ xs: 1, sm: 2, lg: 3 }}
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
    </Flex>
  );
}

// MonitorPage is the read-only system overview for administrators. It leads with
// the task metrics, then the dependency probes and delivery/consumption progress,
// then the queue/record statistics, and only ends with the static service facts.
// It auto-refreshes every 15 seconds while the switch stays on.
export function MonitorPage() {
  const [overview, setOverview] = useState<MonitorOverview>();
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [autoRefresh, setAutoRefresh] = useState(true);
  const request = useRef<AbortController | null>(null);

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
  const errors = overview?.errors ?? [];
  const degradedNotes = overview ? degradedDependencyNotes(overview) : [];

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
          <Button icon={<ReloadOutlined />} loading={refreshing} onClick={() => void load()}>
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
      {overview && status === "error" && (
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
      {overview && status === "degraded" && (
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
      {loading && !overview ? (
        <Spin description="正在读取监控快照…" />
      ) : overview ? (
        <MonitorSnapshot overview={overview} />
      ) : null}
    </section>
  );
}
