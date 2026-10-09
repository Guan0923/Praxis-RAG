import { useEffect, useState } from "react";
import { Alert, Button, Drawer, Spin, Table, Tag, Tooltip } from "antd";
import { EditOutlined, PlusOutlined, ReloadOutlined } from "@ant-design/icons";
import { api, errorText, type KnowledgeBase } from "./api";
import { KnowledgeBaseForm } from "./CreateKnowledgeBase";

type Item = Partial<KnowledgeBase> & { namespace: string; configured: boolean; document_count: number };

export function KnowledgeBasesPage({ disabled, onCreate, onChanged }: {
  disabled: boolean; onCreate: () => void; onChanged: () => void;
}) {
  const [items, setItems] = useState<Item[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [revision, setRevision] = useState(0);
  const [selected, setSelected] = useState<string>();
  const [details, setDetails] = useState<KnowledgeBase>();
  const [detailError, setDetailError] = useState("");
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true); setError("");
    void api<{ knowledge_bases: Item[] }>("/v1/admin/knowledge-bases", undefined, { signal: controller.signal, cache: "no-store" })
      .then((data) => { if (!controller.signal.aborted) setItems(data.knowledge_bases); })
      .catch((reason) => { if (!controller.signal.aborted) setError(errorText(reason)); })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [revision]);
  useEffect(() => {
    setDetails(undefined); setDetailError("");
    if (!selected) return;
    const controller = new AbortController();
    void api<KnowledgeBase>("/v1/admin/knowledge-bases/" + selected, undefined, { signal: controller.signal, cache: "no-store" })
      .then((data) => { if (!controller.signal.aborted) setDetails(data); })
      .catch((reason) => { if (!controller.signal.aborted) setDetailError(errorText(reason)); });
    return () => controller.abort();
  }, [selected]);
  return <section aria-label="知识库页面">
    <div className="page-heading"><h1>知识库</h1><div className="toolbar">
      <Tooltip title="刷新知识库"><Button aria-label="刷新知识库" icon={<ReloadOutlined spin={loading} />} disabled={saving}
        onClick={() => setRevision((value) => value + 1)} /></Tooltip>
      <Button type="primary" icon={<PlusOutlined />} disabled={disabled || saving} onClick={onCreate}>创建知识库</Button>
    </div></div>
    {error && <Alert className="page-alert" type="error" showIcon title="知识库读取失败" description={error} />}
    <Table<Item> rowKey="namespace" dataSource={items} loading={loading} scroll={{ x: 820 }} pagination={{ pageSize: 10, hideOnSinglePage: true }}
      columns={[
        { title: "知识库", dataIndex: "namespace", width: 170, render: (name) => <span className="break-text">{name}</span> },
        { title: "Embedding", width: 250, render: (_, item) => item.configured ? <div className="knowledge-provider-summary">
          <span>{item.embedding_model}</span><span className="secondary break-text">{item.embedding?.base_url || "未配置 URL"}</span>
          <span className="secondary">Key：{item.embedding?.api_key_set ? "已保存" : "未设置"}</span>
        </div> : <Tag>缺少配置记录</Tag> },
        { title: "Reranker", width: 230, render: (_, item) => item.reranker_model ? <div className="knowledge-provider-summary">
          <span>{item.reranker_model}</span><span className="secondary break-text">{item.reranker?.base_url}</span>
          <span className="secondary">Key：{item.reranker?.api_key_set ? "已保存" : "未设置"}</span>
        </div> : "未启用" },
        { title: "向量维度", dataIndex: "vector_size", width: 90, render: (value) => value ?? "—" },
        { title: "文档", dataIndex: "document_count", width: 70 },
        { title: "配置", width: 70, render: (_, item) => <Tooltip title="查看与修改配置"><Button
          aria-label={"配置 " + item.namespace} icon={<EditOutlined />} disabled={disabled || saving || !item.configured}
          onClick={() => setSelected(item.namespace)} /></Tooltip> },
      ]} />
    <Drawer title={selected ? "知识库配置 · " + selected : "知识库配置"} open={!!selected} size={720}
      closable={!saving} maskClosable={!saving} keyboard={!saving} destroyOnHidden onClose={() => setSelected(undefined)}>
      {detailError ? <Alert type="error" showIcon title="配置读取失败" description={detailError} /> : details ?
        <KnowledgeBaseForm key={details.namespace} initial={details} disabled={disabled} onBusyChange={setSaving}
          onSaved={() => { setSelected(undefined); setRevision((value) => value + 1); onChanged(); }} /> : <Spin />}
    </Drawer>
  </section>;
}
