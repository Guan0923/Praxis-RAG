import { useEffect, useState } from "react";
import { Alert, Button, Input, Select, Space, Table } from "antd";
import { ReloadOutlined } from "@ant-design/icons";
import { api, errorText } from "./api";

const actions: Record<string, string> = {
  create_kb: "创建知识库", update_kb: "修改知识库", delete_kb: "删除知识库",
  read_kb: "查看知识库", reembed_kb: "重新向量化", list_documents: "查看文档列表",
  add_document: "提交文档", import_document: "飞书导入", update_document: "替换文档",
  delete_document: "删除文档", read_document: "阅读文档", search: "检索",
  read_artifact: "读取产物", download_artifact: "下载产物", read_job: "查看文档任务",
};
const outcomes: Record<string, string> = {
  accepted: "已受理", skipped: "已跳过", failed: "失败", success: "成功",
  partial: "部分失败", returned: "已返回来源", pending: "待受理",
};
type Entry = {
  id: string; created_at: string; actor: string; channel: string;
  action: string; namespace: string; target: string; outcome: string;
};
type Page = { items: Entry[]; next_cursor: string };
const emptyFilter = { actor: "", namespace: "", target: "", action: "" };

export function AuditEventsPage() {
  const [draft, setDraft] = useState(emptyFilter);
  const [filter, setFilter] = useState(emptyFilter);
  const [cursors, setCursors] = useState([""]);
  const [page, setPage] = useState<Page>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    const controller = new AbortController();
    const query = new URLSearchParams();
    for (const [key, value] of Object.entries(filter)) {
      if (value.trim()) query.set(key, value.trim());
    }
    const before = cursors[cursors.length - 1];
    if (before) query.set("before", before);
    setPage(undefined);
    setError("");
    setLoading(true);
    void api<Page>(`/v1/admin/audit-events?${query}`, undefined, { signal: controller.signal, cache: "no-store" })
      .then(data => { if (!controller.signal.aborted) setPage(data); })
      .catch(cause => { if (!controller.signal.aborted) setError(errorText(cause)); })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [filter, cursors]);

  return <section aria-label="操作审计">
    <div className="page-heading">
      <h1>操作审计</h1>
      <Button icon={<ReloadOutlined />} aria-label="刷新操作审计" onClick={() => setCursors([""])} />
    </div>
    <form onSubmit={event => { event.preventDefault(); setFilter({ ...draft }); setCursors([""]); }}>
      <Space wrap>
        <Input aria-label="操作人" placeholder="用户名、账号或密钥 ID" value={draft.actor}
          onChange={e => setDraft({ ...draft, actor: e.target.value })} />
        <Input aria-label="知识库" placeholder="知识库名称" value={draft.namespace}
          onChange={e => setDraft({ ...draft, namespace: e.target.value })} />
        <Input aria-label="操作对象" placeholder="完整文件名或产物 ID" value={draft.target}
          onChange={e => setDraft({ ...draft, target: e.target.value })} />
        <Select aria-label="操作类型" placeholder="全部操作" allowClear style={{ minWidth: 140 }}
          value={draft.action || undefined} onChange={value => setDraft({ ...draft, action: value || "" })}
          options={Object.entries(actions).map(([value, label]) => ({ value, label }))} />
        <Button htmlType="submit" type="primary">查询</Button>
      </Space>
    </form>
    {error && <Alert type="error" showIcon title="审计记录加载失败" description={error} />}
    <Table<Entry> size="small" rowKey="id" loading={loading} dataSource={page?.items || []}
      pagination={false} scroll={{ x: 850 }} locale={{ emptyText: "暂无审计记录" }}
      columns={[
        { title: "时间", dataIndex: "created_at", render: (v: string) => new Date(v).toLocaleString("zh-CN", { hour12: false }) },
        { title: "操作人", dataIndex: "actor" }, { title: "入口", dataIndex: "channel" },
        { title: "操作", dataIndex: "action", render: (v: string) => actions[v] || v },
        { title: "知识库", dataIndex: "namespace" }, { title: "对象", dataIndex: "target", render: (v: string) => v || "—" },
        { title: "结果", dataIndex: "outcome", render: (v: string) => outcomes[v] || v },
      ]} />
    <Space>
      <Button disabled={cursors.length < 2 || loading} onClick={() => setCursors(cursors.slice(0, -1))}>上一页</Button>
      <span>第 {cursors.length} 页</span>
      <Button disabled={!page?.next_cursor || loading} onClick={() => page?.next_cursor && setCursors([...cursors, page.next_cursor])}>下一页</Button>
    </Space>
  </section>;
}
