import { useEffect, useRef, useState } from "react";
import { Alert, App, Button, Form, Input, Modal, Select, Switch, Table, Tag, Tooltip } from "antd";
import { CopyOutlined, DeleteOutlined, EditOutlined, PlusOutlined, ReloadOutlined } from "@ant-design/icons";
import {
  api,
  apiResponse,
  deleteApiKeys,
  errorText,
  type BatchDeleteResult,
  type Role,
} from "./api";
import { apiKeyNamespaceOptions, apiKeyOwnerText, isAdminLike } from "./apiKeys";
import { namespacePattern } from "./navigation";
import { useDragSelection } from "./useDragSelection";

// ApiKey mirrors one key entry of GET /v1/admin/api-keys. The owner fields are
// optional: legacy keys created before per-account ownership have no owner.
type ApiKey = {
  id: string;
  name: string;
  prefix: string;
  namespaces: string[];
  enabled: boolean;
  exportable: boolean;
  created_at: string;
  owner_user_id?: string;
  owner_username?: string;
};
type KeyValues = { name: string; namespaces: string[]; enabled: boolean };

// rowSelector matches every antd Table body row; the rowKey="id" table writes
// the key id to data-row-key, which is what the marquee selection reads.
const ROW_SELECTOR = "tbody tr[data-row-key]";

export function ApiKeysPage({
  namespaces,
  ownedNamespaces,
  role,
}: {
  namespaces: string[];
  ownedNamespaces?: string[];
  role?: Role;
}) {
  const { message, modal } = App.useApp();
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [authorized, setAuthorized] = useState(false);
  const [editing, setEditing] = useState<ApiKey>();
  const [open, setOpen] = useState(false);
  const [secret, setSecret] = useState("");
  const [selectedKeys, setSelectedKeys] = useState<string[]>([]);
  const [batchFailures, setBatchFailures] = useState<BatchDeleteResult["failed"]>([]);
  const [form] = Form.useForm<KeyValues>();
  const request = useRef<AbortController | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);

  async function load(keepFailures = false) {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setLoading(true);
    setError("");
    setKeys([]);
    setAuthorized(false);
    if (!keepFailures) setBatchFailures([]);
    try {
      const result = await api<{ keys: ApiKey[] }>("/v1/admin/api-keys", undefined, { signal: controller.signal });
      if (!controller.signal.aborted) {
        setKeys(result.keys);
        setAuthorized(true);
        // Drop selections whose key vanished while the list was reloaded.
        setSelectedKeys((current) => current.filter((id) => result.keys.some((key) => key.id === id)));
      }
    } catch (e) {
      if (!controller.signal.aborted) setError(errorText(e));
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }
  useEffect(() => { void load(); return () => request.current?.abort(); }, []);

  function edit(key?: ApiKey) {
    setEditing(key);
    form.setFieldsValue(key ?? { name: "", namespaces: [], enabled: true });
    setOpen(true);
  }

  async function save(values: KeyValues) {
    setBusy(true);
    try {
      if (editing) {
        await api("/v1/admin/api-keys/" + editing.id, undefined, { method: "PUT", body: JSON.stringify(values) });
      } else {
        const result = await api<ApiKey & { key: string }>("/v1/admin/api-keys", undefined,
          { method: "POST", body: JSON.stringify({ name: values.name, namespaces: values.namespaces }) });
        setSecret(result.key);
      }
      setOpen(false);
      await load();
    } catch (e) { void message.error(errorText(e)); }
    finally { setBusy(false); }
  }

  async function copyMcp(key: ApiKey) {
    setBusy(true);
    try {
      const configs = await Promise.all(key.namespaces.map(async (namespace) => {
        const config = await api<{ url: string; headers: Record<string, string> }>(
          "/v1/admin/api-keys/" + encodeURIComponent(key.id) + "/mcp-config", undefined,
          { headers: { "X-RAG-Namespace": namespace }, cache: "no-store" });
        return ["praxis-" + namespace, config] as const;
      }));
      await navigator.clipboard.writeText(JSON.stringify({ mcpServers: Object.fromEntries(configs) }, null, 2));
      void message.success("MCP JSON 已复制");
    } catch (e) { void message.error(errorText(e)); }
    finally { setBusy(false); }
  }

  async function setEnabled(key: ApiKey, enabled: boolean) {
    setBusy(true);
    try {
      await api("/v1/admin/api-keys/" + key.id, undefined,
        { method: "PUT", body: JSON.stringify({ name: key.name, namespaces: key.namespaces, enabled }) });
      await load();
    } catch (e) { void message.error(errorText(e)); }
    finally { setBusy(false); }
  }

  function remove(key: ApiKey) {
    modal.confirm({ title: "删除 API Key", content: "删除后，使用此密钥的应用将无法继续访问：" + key.name,
      okText: "删除", cancelText: "取消", okButtonProps: { danger: true },
      onOk: async () => {
        setBusy(true);
        try {
          await apiResponse("/v1/admin/api-keys/" + key.id, undefined, { method: "DELETE" });
          await load();
        } catch (e) { void message.error(errorText(e)); throw e; }
        finally { setBusy(false); }
      },
    });
  }

  // batchRemove deletes every marquee/checkbox selected key in one request. The
  // server answers with the ids it deleted, the ids it could not find (already
  // gone, still reported as success) and the ids it refused; refusals stay
  // selected and are surfaced until the next reload so the operator can retry.
  function batchRemove() {
    const ids = [...selectedKeys];
    if (ids.length === 0) return;
    modal.confirm({
      title: "删除 API Key",
      content: "删除所选 " + ids.length + " 个 API Key 后使用这些密钥的应用将无法继续访问。",
      okText: "删除", cancelText: "取消", okButtonProps: { danger: true },
      onOk: async () => {
        setBusy(true);
        setBatchFailures([]);
        try {
          const result: BatchDeleteResult = await deleteApiKeys(ids);
          const removed = result.deleted.length + result.missing.length;
          if (removed > 0) {
            void message.success(result.missing.length > 0
              ? "已删除 " + removed + " 个 API Key（" + result.missing.length + " 个已不存在）"
              : "已删除 " + removed + " 个 API Key");
          }
          if (result.failed.length > 0)
            void message.warning(result.failed.length + " 个 API Key 删除失败");
          setBatchFailures(result.failed);
          await load(true);
          setSelectedKeys(result.failed.map((item) => item.id));
        } catch (e) {
          void message.error(errorText(e));
          throw e;
        } finally { setBusy(false); }
      },
    });
  }

  // Without a successful list load nothing may be selected or deleted, so the
  // marquee stays disabled until the page is authorized and idle.
  const dragEnabled = authorized && !busy && !loading && keys.length > 0;
  const { dragging, band } = useDragSelection({
    containerRef: listRef,
    selectedKeys,
    onChange: setSelectedKeys,
    enabled: dragEnabled,
    rowSelector: ROW_SELECTOR,
  });

  // Non-admin accounts may only bind knowledge bases they own, so their picker
  // stays limited to ownedNamespaces; admins and legacy local mode see all.
  const options = apiKeyNamespaceOptions(namespaces, ownedNamespaces, role);
  const showOwner = role === "admin";
  const keyLabel = (id: string) => keys.find((key) => key.id === id)?.name ?? id;
  return (
    <section aria-label="API Key 页面">
      <div className="page-heading">
        <h1>API Key</h1>
        <Button type="primary" icon={<PlusOutlined />} disabled={!authorized || busy || loading} onClick={() => edit()}>创建密钥</Button>
      </div>
      <div className="toolbar">
        <Button icon={<ReloadOutlined />} loading={loading} disabled={busy} onClick={() => void load()}>刷新</Button>
        <Button danger icon={<DeleteOutlined />} disabled={!authorized || busy || loading || selectedKeys.length === 0}
          onClick={batchRemove}>
          {selectedKeys.length > 0 ? "批量删除（" + selectedKeys.length + "）" : "批量删除"}
        </Button>
      </div>
      {!isAdminLike(role) && (
        <Alert className="page-alert" type="info" showIcon
          description="API Key 只能为本人创建的知识库创建，且只能查看/管理自己的密钥。" />
      )}
      {error && <Alert className="page-alert" type="error" showIcon title="无法管理密钥" description={error} />}
      {batchFailures.length > 0 && (
        <Alert className="page-alert" type="warning" showIcon title="部分 API Key 删除失败"
          description={<ul className="batch-delete-failures">
            {batchFailures.map((item) => (
              <li key={item.id}>
                <span className="break-text">{keyLabel(item.id)}</span>（<code>{item.id}</code>）：<span className="batch-failure-error">{item.error}</span>
              </li>
            ))}
          </ul>} />
      )}
      <div ref={listRef} className="api-keys-table" aria-label="API Key 列表">
        <Table<ApiKey> rowKey="id" dataSource={keys} loading={loading} scroll={{ x: showOwner ? 920 : 760 }}
          rowSelection={{
            selectedRowKeys: selectedKeys,
            onChange: (next) => setSelectedKeys(next.map(String)),
            getCheckboxProps: (key) => ({ "aria-label": "选择 " + key.name, disabled: busy || loading }),
          }}
          locale={{ emptyText: authorized ? "暂无 API Key" : "尚未连接" }} pagination={{ pageSize: 10, hideOnSinglePage: true }}
          columns={[
            { title: "名称", dataIndex: "name", width: 180, render: (name: string) => <span className="break-text">{name}</span> },
            ...(showOwner ? [{
              title: "所有者", dataIndex: "owner_username", width: 160,
              render: (owner: string | undefined) => <span className="break-text">{apiKeyOwnerText(owner)}</span>,
            }] : []),
            { title: "密钥", dataIndex: "prefix", width: 150, render: (prefix: string) => <code>{prefix}…</code> },
            { title: "授权 Namespace", dataIndex: "namespaces", render: (values: string[]) => <div className="key-namespaces">{values.map((value) => <Tag key={value}>{value}</Tag>)}</div> },
            { title: "启用", width: 80, render: (_, key) => <Switch aria-label={"启用 " + key.name} checked={key.enabled} disabled={busy || loading} onChange={(enabled) => void setEnabled(key, enabled)} /> },
            { title: "操作", width: 136, render: (_, key) => <div className="toolbar">
              <Tooltip title={!key.exportable ? "旧密钥无法恢复，请重新创建" : !key.enabled ? "密钥已停用" : "复制 MCP JSON"}><Button type="text" aria-label={"复制 MCP JSON " + key.name} icon={<CopyOutlined />} disabled={busy || loading || !key.enabled || !key.exportable} onClick={() => void copyMcp(key)} /></Tooltip>
              <Tooltip title="编辑"><Button type="text" aria-label={"编辑 " + key.name} icon={<EditOutlined />} disabled={busy || loading} onClick={() => edit(key)} /></Tooltip>
              <Tooltip title="删除"><Button type="text" danger aria-label={"删除 " + key.name} icon={<DeleteOutlined />} disabled={busy || loading} onClick={() => remove(key)} /></Tooltip>
            </div> },
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
      <Modal title={editing ? "编辑 API Key" : "创建 API Key"} open={open} onCancel={() => setOpen(false)}
        closable={!busy} mask={{ closable: !busy }} keyboard={!busy} onOk={() => form.submit()} okText="保存" cancelText="取消" confirmLoading={busy} cancelButtonProps={{ disabled: busy }}>
        <Form form={form} layout="vertical" onFinish={(values) => void save(values)} disabled={busy}>
          <Form.Item name="name" label="名称" rules={[{ required: true, whitespace: true, message: "请输入名称" }]}><Input maxLength={80} /></Form.Item>
          <Form.Item name="namespaces" label="授权 Namespace" rules={[
            { required: true, type: "array", min: 1, message: "至少选择一个 Namespace" },
            { validator: (_, values: string[]) => !values?.some((value) => !namespacePattern.test(value)) ? Promise.resolve() : Promise.reject(new Error("Namespace 只能包含字母、数字、点、下划线和横线，最长 64 个字符")) },
          ]}><Select mode="tags" options={options} tokenSeparators={[","]} maxCount={100} /></Form.Item>
          {editing && <Form.Item name="enabled" label="启用" valuePropName="checked"><Switch /></Form.Item>}
        </Form>
      </Modal>
      <Modal title="API Key 已创建" open={!!secret} onCancel={() => setSecret("")} footer={<Button type="primary" onClick={() => setSecret("")}>完成</Button>}>
        <div className="created-key">
          <code>{secret}</code>
          <Tooltip title="复制密钥"><Button aria-label="复制密钥" icon={<CopyOutlined />} onClick={async () => {
            try { await navigator.clipboard.writeText(secret); void message.success("已复制"); }
            catch { void message.error("复制失败，请手动选择密钥"); }
          }} /></Tooltip>
        </div>
      </Modal>
    </section>
  );
}
