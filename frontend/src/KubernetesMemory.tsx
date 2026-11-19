import { useState } from "react";
import { Alert, Flex, Select, Table, Tag } from "antd";
import type { K8sMemoryView, K8sNodeMemory, K8sPodMemory } from "./api";
import { formatDateTime } from "./api";

// Kubernetes quantities use both decimal SI and binary SI suffixes.
const factors: Record<string, number> = {
  "": 1, n: 1e-9, u: 1e-6, m: 1e-3, k: 1e3, M: 1e6, G: 1e9, T: 1e12, P: 1e15, E: 1e18,
  Ki: 2 ** 10, Mi: 2 ** 20, Gi: 2 ** 30, Ti: 2 ** 40, Pi: 2 ** 50, Ei: 2 ** 60,
};

function bytes(quantity?: string) {
  if (!quantity) return undefined;
  const match = /^([+]?(?:[0-9]+\.?[0-9]*|\.[0-9]+))([eE][+-]?\d+|[a-zA-Z]*)$/.exec(quantity);
  if (!match) return undefined;
  const suffix = match[2];
  const value = suffix in factors ? Number(match[1]) * factors[suffix] : Number(quantity);
  return Number.isFinite(value) ? value : undefined;
}
function memoryText(value: number | undefined) {
  if (value === undefined) return "无数据";
  if (value < 1024) return `${value} B`;
  if (value < 2 ** 20) return `${(value / 1024).toFixed(1)} KiB`;
  if (value < 2 ** 30) return `${(value / 2 ** 20).toFixed(1)} MiB`;
  return `${(value / 2 ** 30).toFixed(1)} GiB`;
}
function estimatedAvailable(row: K8sNodeMemory) {
  const capacity = bytes(row.capacity);
  const usage = bytes(row.usage);
  return memoryText(capacity === undefined || usage === undefined ? undefined : Math.max(0, capacity - usage));
}
function sampledTotal(pod: K8sPodMemory) {
  const values = pod.containers.map(c => bytes(c.usage)).filter((v): v is number => v !== undefined);
  return memoryText(values.length ? values.reduce((sum, v) => sum + v, 0) : undefined);
}

export function KubernetesMemory({ view }: { view: K8sMemoryView }) {
  const [node, setNode] = useState<string>();
  const names = [...new Set([...view.nodes.map(n => n.name), ...view.pods.map(p => p.node).filter(Boolean)])].sort();
  return (
    <div className="monitor-stack">
      {view.warnings.map(message => <Alert key={message} type="warning" showIcon title={message} />)}
      <section aria-label="节点内存">
        <h3>节点内存</h3>
        <Table<K8sNodeMemory> size="small" rowKey="name" dataSource={view.nodes} scroll={{ x: 900 }}
          pagination={{ pageSize: 10, hideOnSinglePage: true }} columns={[
            { title: "节点", dataIndex: "name" },
            { title: "内存压力", dataIndex: "pressure", render: (v: string) =>
              v === "True" ? <Tag color="error">有压力</Tag> : v === "False" ? <Tag color="success">正常</Tag> : "未知" },
            { title: "总容量", render: (_, row) => memoryText(bytes(row.capacity)) },
            { title: "可分配", render: (_, row) => memoryText(bytes(row.allocatable)) },
            { title: "工作集用量", render: (_, row) => memoryText(bytes(row.usage)) },
            { title: "估算可用", render: (_, row) => estimatedAvailable(row) },
            { title: "用量采样时间", render: (_, row) => row.timestamp ? formatDateTime(row.timestamp) : "无数据" },
          ]} />
        <p className="secondary monitor-note">估算可用内存按节点容量减工作集用量计算。内存压力取自节点的 MemoryPressure 状态。</p>
      </section>
      <section aria-label="Pod 内存与优先级">
        <Flex align="center" wrap gap={12}>
          <h3>Pod 内存与优先级</h3>
          <Select placeholder="全部节点" aria-label="按节点筛选 Pod" allowClear showSearch value={node}
            options={names.map(name => ({ value: name, label: name }))} onChange={setNode} style={{ minWidth: 180 }} />
        </Flex>
        <Table<K8sPodMemory> size="small" rowKey={p => p.namespace + "/" + p.name}
          dataSource={view.pods.filter(p => !node || p.node === node)} scroll={{ x: 1080 }}
          pagination={{ pageSize: 20, hideOnSinglePage: true }} columns={[
            { title: "命名空间 / Pod", render: (_, p) => <span className="break-text">{p.namespace}/{p.name}</span> },
            { title: "节点", dataIndex: "node", render: v => v || "未调度" },
            { title: "已采样容器用量合计", render: (_, p) => sampledTotal(p) },
            { title: "内存请求 / 上限", render: (_, p) => <Flex vertical gap={2}>
              {(p.request || p.limit) && <span>Pod：{p.request || "未设置"} / {p.limit || "未设置"}</span>}
              {p.containers.map(c => <span className="break-text" key={c.name}>{c.name}：{c.request || "未设置"} / {c.limit || "未设置"}</span>)}
            </Flex> },
            { title: "优先级", render: (_, p) => [p.priority ?? "未知", p.priority_class].filter(v => v !== "").join(" · ") },
            { title: "用量采样时间", render: (_, p) => p.timestamp ? formatDateTime(p.timestamp) : "无数据" },
            { title: "状态原因", render: (_, p) => <span className="break-text">{[p.reason, p.message].filter(Boolean).join(" · ") || "—"}</span> },
          ]} expandable={{ expandedRowRender: p => (
            <div>
              {(p.request || p.limit) && <p>Pod 级内存配置：请求 {p.request || "未设置"}，上限 {p.limit || "未设置"}</p>}
              <Table size="small" rowKey="name" dataSource={p.containers} pagination={false} columns={[
                { title: "容器", dataIndex: "name" }, { title: "类型", dataIndex: "kind" },
                { title: "工作集用量", render: (_, c) => memoryText(bytes(c.usage)) },
                { title: "内存请求", render: (_, c) => c.request || "未设置" },
                { title: "最近终止原因", render: (_, c) => c.termination || "—" },
                { title: "内存上限", render: (_, c) => c.limit || "未设置" },
              ]} />
            </div>
          ) }} />
        <p className="secondary monitor-note">展开 Pod 可查看各容器的内存请求和上限。节点发生内存压力驱逐时，kubelet 先看 Pod 用量是否超过请求量，再看优先级和超出请求量的程度。</p>
      </section>
    </div>
  );
}
