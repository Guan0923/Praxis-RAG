import { useEffect, useRef, useState } from "react";
import { Alert, App, Button, Drawer, Spin, Table, Tag, Tooltip } from "antd";
import { DeleteOutlined, EditOutlined, PlusOutlined, ReloadOutlined } from "@ant-design/icons";
import {
  api,
  deleteKnowledgeBases,
  errorText,
  type BatchDeleteFailure,
  type KnowledgeBase,
} from "./api";
import { KnowledgeBaseForm } from "./CreateKnowledgeBase";
import { useDragSelection } from "./useDragSelection";

type Item = Partial<KnowledgeBase> & { namespace: string; configured: boolean; document_count: number };

// rowSelector matches every antd Table body row; rowKey="namespace" writes the
// namespace to data-row-key, which is what the marquee selection reads.
const ROW_SELECTOR = "tbody tr[data-row-key]";

export function KnowledgeBasesPage({ disabled, onCreate, onChanged }: {
  disabled: boolean; onCreate: () => void; onChanged: () => void;
}) {
  const { message, modal } = App.useApp();
  const [items, setItems] = useState<Item[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [revision, setRevision] = useState(0);
  const [selected, setSelected] = useState<string>();
  const [details, setDetails] = useState<KnowledgeBase>();
  const [detailError, setDetailError] = useState("");
  const [saving, setSaving] = useState(false);
  const [selectedKeys, setSelectedKeys] = useState<string[]>([]);
  const [batchFailures, setBatchFailures] = useState<BatchDeleteFailure[]>([]);
  const [deleting, setDeleting] = useState(false);
  const listRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true); setError("");
    void api<{ knowledge_bases: Item[] }>("/v1/admin/knowledge-bases", undefined, { signal: controller.signal, cache: "no-store" })
      .then((data) => {
        if (controller.signal.aborted) return;
        setItems(data.knowledge_bases);
        // Drop selections whose knowledge base vanished while reloading; the
        // failed namespaces of a batch delete still exist and stay selected.
        setSelectedKeys((current) => current.filter((namespace) =>
          data.knowledge_bases.some((item) => item.namespace === namespace)));
      })
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

  const busy = saving || deleting;
  // Marquee selection is only meaningful once the list is interactive; while a
  // request or the configuration drawer is busy the browser keeps normal clicks.
  const dragEnabled = !disabled && !saving && !loading && items.length > 0;
  const { dragging, band } = useDragSelection({
    containerRef: listRef,
    selectedKeys,
    onChange: setSelectedKeys,
    enabled: dragEnabled,
    rowSelector: ROW_SELECTOR,
  });

  // refresh clears the failure alert of the previous batch operation and
  // reloads the list; a batch delete keeps its alert until this manual reload.
  function refresh() {
    setBatchFailures([]);
    setRevision((value) => value + 1);
  }

  // batchRemove purges every checkbox/marquee selected knowledge base in one
  // request. Namespaces the server refused stay selected and are listed until
  // the next manual reload so the operator can retry them.
  function batchRemove() {
    const namespaces = [...selectedKeys];
    if (namespaces.length === 0) return;
    modal.confirm({
      title: "删除知识库",
      content: "删除所选 " + namespaces.length + " 个知识库及其全部文档（不可恢复），并移除配置记录。",
      okText: "删除", cancelText: "取消", okButtonProps: { danger: true },
      onOk: async () => {
        setDeleting(true);
        try {
          const result = await deleteKnowledgeBases(namespaces);
          if (result.deleted.length > 0) void message.success("已删除 " + result.deleted.length + " 个知识库");
          setSelectedKeys(result.failed.map((item) => item.id));
          setBatchFailures(result.failed);
          setRevision((value) => value + 1);
          if (result.deleted.length > 0) onChanged();
        } catch (cause) {
          void message.error(errorText(cause));
        } finally { setDeleting(false); }
      },
    });
  }

  return <section aria-label="知识库页面">
    <div className="page-heading"><h1>知识库</h1><div className="toolbar">
      <Tooltip title="刷新知识库"><Button aria-label="刷新知识库" icon={<ReloadOutlined spin={loading} />} disabled={saving}
        onClick={refresh} /></Tooltip>
      <Tooltip title={selectedKeys.length > 0 ? "删除所选知识库及其全部文档" : "先选择要删除的知识库"}>
        <Button danger icon={<DeleteOutlined />} disabled={disabled || busy || loading || selectedKeys.length === 0}
          onClick={batchRemove}>
          {selectedKeys.length > 0 ? "批量删除（" + selectedKeys.length + "）" : "批量删除"}
        </Button>
      </Tooltip>
      <Button type="primary" icon={<PlusOutlined />} disabled={disabled || busy} onClick={onCreate}>创建知识库</Button>
    </div></div>
    {error && <Alert className="page-alert" type="error" showIcon title="知识库读取失败" description={error} />}
    {batchFailures.length > 0 && (
      <Alert className="page-alert" type="warning" showIcon title="部分知识库删除失败"
        description={<ul className="batch-delete-failures">
          {batchFailures.map((item) => (
            <li key={item.id}>
              <span className="break-text">{item.id}</span>：{item.error || "删除失败"}
            </li>
          ))}
        </ul>} />
    )}
    <div ref={listRef} className="knowledge-bases-table">
      <Table<Item> rowKey="namespace" dataSource={items} loading={loading} size="small" scroll={{ x: 820 }}
        pagination={{ pageSize: 10, hideOnSinglePage: true, size: "small" }}
        rowSelection={{
          selectedRowKeys: selectedKeys,
          onChange: (next) => setSelectedKeys(next.map(String)),
          getCheckboxProps: (item) => ({ "aria-label": "选择 " + item.namespace, disabled: disabled || saving || loading || deleting }),
        }}
        columns={[
          { title: "知识库", dataIndex: "namespace", width: 150, render: (name) => <span className="break-text">{name}</span> },
          { title: "Embedding", width: 220, render: (_, item) => item.configured ? <div className="knowledge-provider-summary">
            <span>{item.embedding_model}</span>
            <span className="secondary break-text">{item.embedding?.base_url || "未配置 URL"} · Key：{item.embedding?.api_key_set ? "已保存" : "未设置"}</span>
          </div> : <Tag>缺少配置记录</Tag> },
          { title: "Reranker", width: 200, render: (_, item) => item.reranker_model ? <div className="knowledge-provider-summary">
            <span>{item.reranker_model}</span>
            <span className="secondary break-text">{item.reranker?.base_url ? item.reranker.base_url + " · " : ""}Key：{item.reranker?.api_key_set ? "已保存" : "未设置"}</span>
          </div> : "未启用" },
          { title: "向量维度", dataIndex: "vector_size", width: 80, render: (value) => value ?? "—" },
          { title: "文档", dataIndex: "document_count", width: 60 },
          { title: "配置", width: 60, render: (_, item) => <Tooltip title="查看与修改配置"><Button
            aria-label={"配置 " + item.namespace} icon={<EditOutlined />} disabled={disabled || saving || !item.configured}
            onClick={() => setSelected(item.namespace)} /></Tooltip> },
        ]} />
      {dragging && band && (
        <div className="drag-band" aria-hidden="true" style={{
          left: band.left,
          top: band.top,
          width: Math.max(0, band.right - band.left),
          height: Math.max(0, band.bottom - band.top),
        }} />
      )}
    </div>
    <Drawer title={selected ? "知识库配置 · " + selected : "知识库配置"} open={!!selected} size={720}
      closable={!saving} maskClosable={!saving} keyboard={!saving} destroyOnHidden onClose={() => setSelected(undefined)}>
      {detailError ? <Alert type="error" showIcon title="配置读取失败" description={detailError} /> : details ?
        <KnowledgeBaseForm key={details.namespace} initial={details} disabled={disabled} onBusyChange={setSaving}
          onSaved={() => { setSelected(undefined); setRevision((value) => value + 1); onChanged(); }} /> : <Spin />}
    </Drawer>
  </section>;
}
