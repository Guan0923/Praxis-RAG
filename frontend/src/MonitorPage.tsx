import { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert, Button, Card, Col, Descriptions, Empty, Flex, Row, Spin, Statistic, Switch,
  Table, Tag, Tooltip,
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
  type MonitorOverview,
  type MonitorPipelineKafka,
} from "./api";

// monitorRefreshMs is the auto-refresh period: the backend does not push
// snapshot updates, so the page polls. It stays off the request path and only
// re-fetches while the switch is on.
const monitorRefreshMs = 15_000;

// Queue statuses are rendered as compact count tags. document_jobs and
// search_tasks share the first four states.
const documentJobStatusLabels: Record<string, string> = {
  queued: "排队中",
  processing: "处理中",
  succeeded: "成功",
  failed: "失败",
};
// Outbox rows are retained records, not deliveries still waiting to happen: the
// application never writes PUBLISHED back by design, Debezium captures the rows
// and Kafka records consumption progress. The copy therefore describes record
// state only and never says 待投递/已投递 or reports a delivery backlog. The
// outbox stores its status as an upper-case literal ("PENDING" / "PUBLISHED"),
// so the map carries both spellings.
const outboxRecordStatusLabels: Record<string, string> = {
  pending: "PENDING 记录",
  published: "PUBLISHED 记录",
  failed: "FAILED 记录",
  PENDING: "PENDING 记录",
  PUBLISHED: "PUBLISHED 记录",
  FAILED: "FAILED 记录",
};
const searchTaskStatusLabels: Record<string, string> = {
  queued: "排队中",
  processing: "处理中",
  succeeded: "成功",
  failed: "失败",
  timed_out: "超时",
};

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

// QueueStatusTags renders one queue's by_status map as count tags. Known
// statuses keep the label map's display order; unknown keys are appended as-is.
function QueueStatusTags({ counts, labels }: {
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
        // 状态 key 的大小写由后端存储决定（outbox 用大写），查文案时统一兜底。
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

// UnavailableCard is the placeholder shown when the backend could not read the
// state database: the section degrades to a hint instead of crashing.
function UnavailableCard({ title }: { title: string }) {
  return (
    <Card size="small" title={title}>
      <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="状态库暂不可用" />
    </Card>
  );
}

function DependencyTable({ dependencies }: { dependencies: MonitorDependency[] }) {
  return (
    <Table<MonitorDependency>
      rowKey="name"
      size="small"
      pagination={false}
      dataSource={dependencies}
      locale={{ emptyText: "暂无依赖信息" }}
      columns={[
        {
          title: "依赖",
          dataIndex: "label",
          render: (_label: string, dependency) => (
            <Flex wrap gap={6} align="center">
              <span>{dependency.label || dependency.name}</span>
              <span className="secondary">{dependency.name}</span>
            </Flex>
          ),
        },
        {
          title: "状态",
          dataIndex: "status",
          width: 100,
          render: (status: MonitorDependency["status"]) => (
            <Tag color={monitorDependencyStatusColors[status] || "default"}>
              {monitorDependencyStatusLabels[status] || status}
            </Tag>
          ),
        },
        {
          title: "延迟",
          dataIndex: "latency_ms",
          width: 110,
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

// kafkaLagText keeps Kafka consumer lag apart from outbox records: the lag is
// only shown when Kafka itself reports it, otherwise it stays 未知.
function kafkaLagText(kafka?: MonitorPipelineKafka | null) {
  const lag = kafka?.consumer_lag;
  if (typeof lag !== "number" || !Number.isFinite(lag)) return "消费滞后：未知";
  const committed = typeof kafka?.committed_offset === "number"
    ? kafka.committed_offset.toLocaleString()
    : "未知";
  const end = typeof kafka?.end_offset === "number"
    ? kafka.end_offset.toLocaleString()
    : "未知";
  return `消费滞后：${lag.toLocaleString()} 条（已提交 ${committed} / 末尾 ${end}）`;
}

// MonitorSnapshot renders one successfully fetched overview. It is a separate
// component so every section can narrow null queues/pipeline/library/activity
// without repeating guards in the page shell.
function MonitorSnapshot({ overview }: { overview: MonitorOverview }) {
  const { service, dependencies, queues, pipeline, library, activity } = overview;
  const jobs = queues?.document_jobs;
  const outbox = pipeline?.outbox;
  const pipelineJobs = pipeline?.document_jobs;
  const debezium = pipeline?.debezium;
  const kafka = pipeline?.kafka;
  const searchTasks = queues?.search_tasks;
  const uploads = queues?.uploads;
  const stages = Object.entries(jobs?.by_stage || {}).map(([stage, count]) => ({ stage, count }));

  return (
    <Flex vertical gap={16}>
      <Card size="small" title="服务信息">
        <Descriptions
          size="small"
          column={{ xs: 1, sm: 2, lg: 3 }}
          items={[
            { key: "version", label: "版本", children: service?.version || "—" },
            { key: "uptime", label: "运行时长", children: formatDuration(service?.uptime_seconds) },
            { key: "backend", label: "后端", children: service?.backend || "—" },
            { key: "collection", label: "集合", children: service?.collection || "—" },
            { key: "upload", label: "上传模式", children: service?.upload_mode || "—" },
            { key: "outbox", label: "Outbox 模式", children: service?.outbox_bridge || "—" },
            { key: "worker", label: "文档 Worker", children: service?.document_worker || "—" },
            { key: "pdf", label: "PDF 引擎", children: service?.pdf_engine || "—" },
            { key: "started", label: "启动时间", children: formatDateTime(service?.started_at) },
          ]}
        />
      </Card>
      <Card size="small" title="依赖状态">
        <DependencyTable dependencies={dependencies} />
      </Card>
      <Card size="small" title="队列与处理进度">
        <Row gutter={[16, 16]}>
          <Col xs={24} xl={12}>
            <section className="monitor-block" aria-label="文档任务队列">
              <h3>文档任务</h3>
              <QueueStatusTags counts={jobs?.by_status} labels={documentJobStatusLabels} />
              <Descriptions
                size="small"
                column={1}
                items={[
                  { key: "oldest", label: "最老排队", children: queues ? formatDuration(jobs?.oldest_queued_seconds) : "未知" },
                  { key: "stuck", label: "卡死处理中", children: queues ? `${jobs?.stuck_processing ?? 0} 个` : "未知" },
                  { key: "failed", label: "近 24 小时失败", children: queues ? `${jobs?.failed_recent ?? 0} 个` : "未知" },
                ]}
              />
              <h3>阶段分布</h3>
              <Table
                rowKey="stage"
                size="small"
                pagination={false}
                dataSource={stages}
                locale={{ emptyText: queues ? "暂无处理中任务" : "状态库暂不可用" }}
                columns={[
                  {
                    title: "阶段",
                    dataIndex: "stage",
                    render: (stage: string) => uploadJobStageLabels[stage] || stage,
                  },
                  { title: "任务数", dataIndex: "count", width: 100 },
                ]}
              />
            </section>
          </Col>
          <Col xs={24} xl={12}>
            <section className="monitor-block" aria-label="Outbox 记录">
              <h3>Outbox 记录</h3>
              {outbox ? (
                <>
                  <QueueStatusTags counts={outbox.by_status} labels={outboxRecordStatusLabels} />
                  <Descriptions
                    size="small"
                    column={1}
                    items={[
                      { key: "total", label: "记录总数", children: `${outbox.total} 条` },
                      { key: "oldest", label: "最老记录", children: formatDuration(outbox.oldest_record_seconds) },
                    ]}
                  />
                </>
              ) : (
                <Descriptions
                  size="small"
                  column={1}
                  items={[
                    { key: "total", label: "记录总数", children: "未知" },
                    { key: "oldest", label: "最老记录", children: "未知" },
                  ]}
                />
              )}
              <p className="secondary monitor-outbox-note">
                Outbox 记录在保留期内可能仍为 PENDING，由 Debezium 投递；该计数不代表未投递积压。
              </p>
            </section>
            <section className="monitor-block" aria-label="检索任务队列">
              <h3>检索任务</h3>
              <QueueStatusTags counts={searchTasks?.by_status} labels={searchTaskStatusLabels} />
              <Descriptions
                size="small"
                column={1}
                items={[
                  { key: "oldest", label: "最老活跃", children: queues ? formatDuration(searchTasks?.oldest_active_seconds) : "未知" },
                ]}
              />
            </section>
            <section className="monitor-block" aria-label="上传队列">
              <h3>上传</h3>
              {uploads ? (
                <Statistic title="未完成上传" value={uploads.unfinished} />
              ) : (
                <span className="secondary">未完成上传：未知</span>
              )}
            </section>
          </Col>
        </Row>
        <section className="monitor-block" aria-label="投递与消费进度">
          <h3>投递与消费进度</h3>
          <Flex vertical gap={12}>
            <div className="monitor-progress-row">
              <Flex wrap gap={8} align="center">
                <strong>Debezium 采集</strong>
                <PipelineStatusTag status={debezium?.status} />
                <span className="secondary break-text">{debezium?.detail || "未知"}</span>
              </Flex>
              <span>{captureProgressText(debezium?.capture_progress)}</span>
            </div>
            <div className="monitor-progress-row">
              <Flex wrap gap={8} align="center">
                <strong>Kafka 消费</strong>
                <PipelineStatusTag status={kafka?.status} />
                <span className="secondary break-text">{kafka?.detail || "未知"}</span>
              </Flex>
              <span className="secondary break-text">
                Topic：{kafka?.topic || "未知"}　Consumer Group：{kafka?.group || "未知"}
              </span>
              <span>{kafkaLagText(kafka)}</span>
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
      </Card>
      <Row gutter={[16, 16]}>
        <Col xs={24} lg={12}>
          {library ? (
            <Card size="small" title="资料库">
              <Row gutter={[16, 16]}>
                <Col xs={12} sm={6}><Statistic title="文档" value={library.documents} /></Col>
                <Col xs={12} sm={6}><Statistic title="知识库" value={library.knowledge_bases} /></Col>
                <Col xs={12} sm={6}><Statistic title="结果快照" value={library.result_snapshots} /></Col>
                <Col xs={12} sm={6}><Statistic title="Artifact 快照" value={library.artifact_snapshots} /></Col>
              </Row>
            </Card>
          ) : (
            <UnavailableCard title="资料库" />
          )}
        </Col>
        <Col xs={24} lg={12}>
          {activity ? (
            <Card size="small" title={`活动（最近 ${activity.window_hours} 小时）`}>
              <Row gutter={[16, 16]}>
                <Col xs={8}><Statistic title="成功入库" value={activity.jobs_succeeded} /></Col>
                <Col xs={8}><Statistic title="失败入库" value={activity.jobs_failed} /></Col>
                <Col xs={8}><Statistic title="检索次数" value={activity.searches} /></Col>
              </Row>
            </Card>
          ) : (
            <UnavailableCard title="活动" />
          )}
        </Col>
      </Row>
    </Flex>
  );
}

// MonitorPage is the read-only system overview for administrators. It shows the
// backend service facts, the five dependency probes, the document-job queue, the
// outbox record statistics and the delivery/consumption progress (Debezium
// capture, Kafka consumer lag, document task status), auto-refreshing every 15
// seconds while the switch stays on.
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

  const errors = overview?.errors ?? [];

  return (
    <section aria-label="系统监控页面">
      <div className="page-heading">
        <div className="page-heading-title">
          <h1>系统监控</h1>
          {overview && (
            <Tag color={overview.ok ? "success" : "warning"}>
              {overview.ok ? "运行正常" : "存在异常"}
            </Tag>
          )}
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
      {overview && !overview.ok && (
        <Alert
          className="page-alert"
          type="warning"
          showIcon
          title="服务状态异常"
          description={errors.length ? (
            <ul className="monitor-error-list">
              {errors.map((message, index) => <li key={index}>{message}</li>)}
            </ul>
          ) : "后端未返回具体错误，请查看下方依赖状态。"}
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
