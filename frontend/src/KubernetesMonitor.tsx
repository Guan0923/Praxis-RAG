import { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  App,
  Button,
  Card,
  Descriptions,
  Flex,
  Input,
  Modal,
  Segmented,
  Select,
  Spin,
  Table,
  Tag,
  Typography,
} from "antd";
import { KubernetesMemory } from "./KubernetesMemory";
import type { Key, ReactNode } from "react";
import {
  errorText,
  fetchKubernetesMemory,
  type K8sMemoryView,
  fetchKubernetesPods,
  fetchKubernetesWorkloads,
  restartKubernetesWorkload,
  type K8sPod,
  type K8sPodView,
  type K8sRestartState,
  type K8sWorkload,
  type K8sWorkloadView,
} from "./api";

// KubernetesMonitor is the administrator-only Kubernetes tab of the monitor
// page. It owns its data loading (the overview snapshot may already have
// failed) and follows the page-level per-second tick: only the selected
// subview polls, a request that is still running makes the panel skip the tick
// instead of stacking another one, and the tab does nothing while it is not the
// active top-level tab.
//
// Two list endpoints back the two subviews:
//   - GET /v1/admin/k8s/workloads: controllers with their nested pods;
//   - GET /v1/admin/k8s/pods: every pod of the cluster.
// Both accept the same server-side namespace/name filter. Filter edits take
// effect with the next tick (the request is built from the current input at
// tick time), which avoids one request per keystroke.

type KubernetesSubview = "workloads" | "pods" | "memory" | "scale";

const subviewOptions: Array<{ value: KubernetesSubview; label: string }> = [
  { value: "workloads", label: "工作负载" },
  { value: "pods", label: "全部 Pod" },
  { value: "memory", label: "节点与内存" },
  { value: "scale", label: "文档 Worker 扩缩容" },
];

// phaseColors maps the standard pod phases to the tag palette the rest of the
// Console uses; an unknown future phase stays neutral instead of vanishing.
const phaseColors: Record<string, string> = {
  Running: "success",
  Succeeded: "processing",
  Pending: "warning",
  Failed: "error",
  Unknown: "default",
};

type KubernetesMonitorProps = {
  workerScale: ReactNode;
  // tick is the page-level per-second counter: every panel of the page shares
  // one timer instead of running its own interval.
  tick: number;
  // active is true only while the Kubernetes tab is the selected top-level tab.
  active: boolean;
  // onUpdated reports the generated_at of every successful snapshot so the
  // page header can show the most recent successful refresh time.
  onUpdated?: (generatedAt: string) => void;
};

function podReadyText(pod: K8sPod) {
  return `${pod.ready_containers}/${pod.total_containers}`;
}

function podStateText(pod: K8sPod) {
  return pod.reason || pod.message || "";
}

// PodStateCell renders phase plus the waiting/termination reason. The reason is
// the actionable half (CrashLoopBackOff, ImagePullBackOff, ...), so it stays
// visible next to the phase tag rather than hiding in a tooltip.
function PodStateCell({ pod }: { pod: K8sPod }) {
  const note = podStateText(pod);
  return (
    <Flex wrap gap={6} align="center">
      <Tag color={phaseColors[pod.phase] || "default"}>{pod.phase || "未知"}</Tag>
      {note && <span className="secondary break-text">{note}</span>}
    </Flex>
  );
}

// podsColumns builds the shared pod columns. The nested variant (expanded
// workload row) drops the namespace and owner because both are implied by the
// workload that owns the row.
function podsColumns(scope: "all" | "nested") {
  const columns = [
    ...(scope === "all"
      ? [{ title: "命名空间", dataIndex: "namespace", width: 140, render: (value: string) => <span className="break-text">{value || "—"}</span> }]
      : []),
    { title: "名称", dataIndex: "name", render: (value: string) => <span className="break-text">{value || "—"}</span> },
    { title: "就绪容器", dataIndex: "ready_containers", width: 96, render: (_value: number, pod: K8sPod) => podReadyText(pod) },
    { title: "状态/原因", dataIndex: "phase", width: 220, render: (_value: string, pod: K8sPod) => <PodStateCell pod={pod} /> },
    { title: "重启次数", dataIndex: "restarts", width: 96 },
    { title: "节点", dataIndex: "node", width: 160, render: (value: string) => <span className="break-text">{value || "—"}</span> },
    ...(scope === "all"
      ? [{
        title: "所属资源",
        dataIndex: "owner",
        width: 200,
        render: (_value: unknown, pod: K8sPod) => (
          <span className="break-text">{pod.owner ? `${pod.owner.kind}/${pod.owner.name}` : "—"}</span>
        ),
      }]
      : []),
  ];
  return columns;
}

function podRowKey(pod: K8sPod) {
  return `${pod.namespace}/${pod.name}`;
}

// PodsTable is the flat pod table: reused by the 全部 Pod subview and by the
// expanded workload row (where the backend already nested the owned pods).
function PodsTable({ pods, scope, emptyText }: {
  pods: K8sPod[];
  scope: "all" | "nested";
  emptyText: string;
}) {
  return (
    <Table<K8sPod>
      rowKey={podRowKey}
      size="small"
      pagination={false}
      dataSource={pods}
      scroll={{ x: scope === "all" ? 1040 : 760 }}
      locale={{ emptyText }}
      columns={podsColumns(scope)}
    />
  );
}

// workloadCountText renders the kind-specific counters the backend reports.
// Deployment-style controllers show ready/desired, a Job shows its completion
// state, and a CronJob shows its schedule plus the suspended flag.
function workloadCountText(workload: K8sWorkload) {
  switch (workload.kind) {
    case "Job":
      return `成功 ${workload.succeeded}/${workload.completions} · 失败 ${workload.failed} · 运行 ${workload.active}`;
    case "CronJob":
      return `${workload.schedule || "—"} · 运行 ${workload.active}${workload.suspended ? " · 已暂停" : ""}`;
    default:
      return `${workload.ready}/${workload.desired}`;
  }
}

function workloadRowKey(workload: K8sWorkload) {
  // uid is stable across refreshes; fall back to the namespaced name when an
  // older backend omits it, so the expanded row still survives the per-second
  // snapshot replacement.
  return workload.uid || `${workload.namespace}/${workload.kind}/${workload.name}`;
}

// workloadStatusColor keeps the Chinese status summary readable: the backend
// reports 运行中/已完成/失败/已暂停 (with the English equivalents kept for
// older snapshots).
function workloadStatusColor(status: string) {
  if (["运行中", "已完成", "Running", "Complete", "Succeeded"].includes(status)) return "success";
  if (["失败", "Failed"].includes(status)) return "error";
  if (["已暂停", "Paused"].includes(status)) return "warning";
  return "default";
}

// RestartStateText renders the observed restart progress. submitted/rolling
// disable the row button; completed and failed are terminal and leave the row
// operable again, failed carrying the backend message in red.
function RestartStateText({ restart }: { restart: K8sRestartState }) {
  switch (restart.status) {
    case "submitted":
      return <span className="secondary">已提交</span>;
    case "rolling":
      return (
        <Flex vertical gap={2}>
          <span className="secondary">
            滚动中：已更新 {restart.updated}/{restart.desired}，就绪 {restart.ready}
          </span>
          {restart.message && <span className="secondary break-text">{restart.message}</span>}
        </Flex>
      );
    case "completed":
      return <span className="secondary">已完成</span>;
    case "failed":
      return <Typography.Text type="danger">{restart.message || "滚动重启失败"}</Typography.Text>;
    default:
      return <span className="secondary">{restart.message || restart.status || "未知"}</span>;
  }
}

export function KubernetesMonitor({ tick, active, onUpdated, workerScale }: KubernetesMonitorProps) {
  const { message } = App.useApp();
  const [subview, setSubview] = useState<KubernetesSubview>("workloads");
  const [namespace, setNamespace] = useState("");
  const [nameDraft, setNameDraft] = useState("");
  const [workloadView, setWorkloadView] = useState<K8sWorkloadView>();
  const [memory, setMemory] = useState<K8sMemoryView>();
  const [podView, setPodView] = useState<K8sPodView>();
  const [namespaces, setNamespaces] = useState<string[]>([]);
  const [error, setError] = useState("");
  const [expandedRowKeys, setExpandedRowKeys] = useState<readonly Key[]>([]);
  const [restartTarget, setRestartTarget] = useState<K8sWorkload | null>(null);
  const [restarting, setRestarting] = useState(false);

  // The applied filter lives in a ref: the input/selection update it at once,
  // but requests are only built on a tick, so an edit takes effect with the
  // next refresh instead of firing one request per keystroke.
  const filter = useRef({ namespace: "", name: "" });
  const request = useRef<AbortController | null>(null);
  // lastTick remembers the tick that already produced a request, so re-renders
  // and subview switches do not fetch twice for one tick.
  const lastTick = useRef(-1);
  const previousSubview = useRef<KubernetesSubview>(subview);
  const onUpdatedRef = useRef(onUpdated);

  useEffect(() => {
    onUpdatedRef.current = onUpdated;
  }, [onUpdated]);

  const load = useCallback(async (target: KubernetesSubview, force = false) => {
    // Single-flight: a slow request makes this panel skip the tick (the next
    // tick picks the newest filter); only a subview switch (force) replaces an
    // in-flight request for the view the operator just left.
    if (target === "scale") {
      request.current?.abort();
      return;
    }
    if (request.current && !force) return;
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    try {
      if (target === "workloads") {
        const view = await fetchKubernetesWorkloads(filter.current, controller.signal);
        if (!controller.signal.aborted) {
          setWorkloadView(view);
          setNamespaces(Array.isArray(view.namespaces) ? view.namespaces : []);
          setError("");
          onUpdatedRef.current?.(view.generated_at);
        }
      } else if (target === "memory") {
        const view = await fetchKubernetesMemory(controller.signal);
        if (!controller.signal.aborted) {
          setMemory(view);
          setError("");
          onUpdatedRef.current?.(view.generated_at);
        }
      } else {
        const view = await fetchKubernetesPods(filter.current, controller.signal);
        if (!controller.signal.aborted) {
          setPodView(view);
          setNamespaces(Array.isArray(view.namespaces) ? view.namespaces : []);
          setError("");
          onUpdatedRef.current?.(view.generated_at);
        }
      }
    } catch (cause) {
      if (!controller.signal.aborted) setError(errorText(cause));
    } finally {
      if (request.current === controller) request.current = null;
    }
  }, []);

  // A tick refresh only while this tab is active. The initial mount fetches
  // once (lastTick starts at -1), and a tab that becomes active again fetches
  // immediately because ticks kept advancing while it was inactive.
  useEffect(() => {
    if (!active) return;
    if (tick === lastTick.current) return;
    lastTick.current = tick;
    void load(subview, false);
  }, [active, subview, tick, load]);

  // Switching the subview starts the new one at once and abandons the request
  // of the old one; the old subview must not keep polling afterwards.
  useEffect(() => {
    if (!active) return;
    if (previousSubview.current === subview) return;
    previousSubview.current = subview;
    void load(subview, true);
  }, [active, subview, load]);

  // Stop touching the cluster when the tab is hidden and when the page unmounts.
  useEffect(() => {
    if (active) return;
    request.current?.abort();
  }, [active]);

  useEffect(() => () => request.current?.abort(), []);

  const submitRestart = useCallback(async () => {
    const target = restartTarget;
    if (!target) return;
    setRestarting(true);
    try {
      const payload = await restartKubernetesWorkload(target.resource, target.namespace, target.name);
      // Mark the row submitted immediately: the POST response is authoritative
      // for "the restart was accepted", so the button must be disabled before
      // the next per-second snapshot arrives. The following poll replaces this
      // optimistic state with the controller-observed progress.
      setWorkloadView((previous) => {
        if (!previous) return previous;
        const key = workloadRowKey(target);
        return {
          ...previous,
          workloads: previous.workloads.map((workload) => (
            workloadRowKey(workload) === key ? { ...workload, restart: payload.restart } : workload
          )),
        };
      });
      message.success("已提交");
      setRestartTarget(null);
    } catch (cause) {
      message.error(`滚动重启失败：${errorText(cause)}`);
    } finally {
      setRestarting(false);
    }
  }, [message, restartTarget]);

  const namespaceOptions = [
    { value: "", label: "全部命名空间" },
    ...namespaces.map((item) => ({ value: item, label: item })),
  ];
  const cluster = subview === "workloads" ? workloadView?.cluster : undefined;
  const hasData = subview === "workloads" ? Boolean(workloadView) : subview === "memory" ? Boolean(memory) : Boolean(podView);

  const workloadsTable = (
    <section aria-label="Kubernetes 工作负载">
      <Table<K8sWorkload>
        rowKey={workloadRowKey}
        size="small"
        dataSource={workloadView?.workloads ?? []}
        scroll={{ x: 1120 }}
        locale={{ emptyText: "暂无工作负载" }}
        pagination={{ pageSize: 20, size: "small", hideOnSinglePage: true, showSizeChanger: false }}
        expandable={{
          expandedRowKeys,
          onExpandedRowsChange: (keys) => setExpandedRowKeys(keys),
          rowExpandable: (workload) => (workload.pods?.length ?? 0) > 0,
          expandedRowRender: (workload) => (
            <PodsTable pods={workload.pods ?? []} scope="nested" emptyText="暂无归属 Pod" />
          ),
        }}
        columns={[
          {
            title: "命名空间",
            dataIndex: "namespace",
            width: 140,
            render: (value: string) => <span className="break-text">{value || "—"}</span>,
          },
          {
            title: "类型",
            dataIndex: "kind",
            width: 130,
            render: (value: string) => <span className="break-text">{value || "—"}</span>,
          },
          {
            title: "名称",
            dataIndex: "name",
            render: (value: string) => <span className="break-text">{value || "—"}</span>,
          },
          {
            title: "状态",
            dataIndex: "status",
            width: 110,
            render: (value: string) => (
              <Tag color={workloadStatusColor(value)}>{value || "未知"}</Tag>
            ),
          },
          {
            title: "就绪/完成计数",
            dataIndex: "ready",
            width: 220,
            render: (_value: number, workload) => <span className="break-text">{workloadCountText(workload)}</span>,
          },
          {
            title: "操作",
            dataIndex: "restartable",
            width: 240,
            render: (_value: boolean, workload) => {
              const restart = workload.restart;
              const busy = restart?.status === "submitted" || restart?.status === "rolling";
              return (
                <Flex vertical gap={4}>
                  {workload.restartable ? (
                    <Button
                      size="small"
                      disabled={busy}
                      onClick={() => setRestartTarget(workload)}
                    >
                      滚动重启
                    </Button>
                  ) : (
                    // 不可重启的原因直接写在行内：操作员不必悬停才能知道为什么没有按钮。
                    <span className="secondary break-text">
                      {workload.restart_blocked_reason || "当前资源不可重启"}
                    </span>
                  )}
                  {restart && <RestartStateText restart={restart} />}
                </Flex>
              );
            },
          },
        ]}
      />
    </section>
  );

  return (
    <div className="monitor-stack">
      <Card
        size="small"
        title="Kubernetes 集群"
        extra={cluster ? (
          <span className="secondary">
            集群：{cluster.name}（{cluster.source}）
          </span>
        ) : undefined}
      >
        <Flex wrap gap={8} align="center" className="monitor-k8s-filters">
          <Segmented
            size="small"
            options={subviewOptions}
            value={subview}
            onChange={(value) => setSubview(value as KubernetesSubview)}
          />
          {(subview === "workloads" || subview === "pods") && (
            <>
              <Select
                size="small"
                virtual={false}
                showSearch
                optionFilterProp="label"
                aria-label="Kubernetes 命名空间"
                value={namespace}
                onChange={(value: string) => {
                  setNamespace(value);
                  filter.current = { ...filter.current, namespace: value };
                }}
                options={namespaceOptions}
                style={{ minWidth: 180 }}
              />
              <Input
                size="small"
                allowClear
                aria-label="Kubernetes 名称过滤"
                placeholder={subview === "pods" ? "按 Pod 名称筛选" : "按工作负载名称筛选"}
                value={nameDraft}
                onChange={(event) => {
                  const value = event.target.value;
                  setNameDraft(value);
                  filter.current = { ...filter.current, name: value.trim() };
                }}
                style={{ maxWidth: 220, minWidth: 150 }}
              />
            </>
          )}
        </Flex>

        {subview !== "scale" && error && (
          <Alert
            className="page-alert"
            type="error"
            showIcon
            title="Kubernetes 数据加载失败"
            description={<span className="preserve-lines">{error}</span>}
          />
        )}

        {subview === "scale" ? workerScale : !error && !hasData ? (
          <Spin description="正在读取 Kubernetes 快照…" />
        ) : subview === "workloads" ? (
          workloadsTable
        ) : subview === "memory" ? (
          memory && <KubernetesMemory view={memory} />
        ) : (
          <section aria-label="Kubernetes Pod">
            <PodsTable pods={podView?.pods ?? []} scope="all" emptyText="暂无 Pod" />
          </section>
        )}
      </Card>

      <Modal
        open={restartTarget !== null}
        title="滚动重启"
        okText="确认重启"
        cancelText="取消"
        confirmLoading={restarting}
        onOk={() => void submitRestart()}
        onCancel={() => {
          if (!restarting) setRestartTarget(null);
        }}
      >
        {restartTarget && (
          <>
            <Descriptions
              size="small"
              column={1}
              items={[
                { key: "kind", label: "资源类型", children: restartTarget.kind || "未知" },
                { key: "namespace", label: "命名空间", children: restartTarget.namespace || "—" },
                { key: "name", label: "名称", children: restartTarget.name || "—" },
              ]}
            />
            <p className="secondary monitor-note">
              控制器将按工作负载的更新策略替换 Pod，期望副本数保持不变。
            </p>
          </>
        )}
      </Modal>
    </div>
  );
}
