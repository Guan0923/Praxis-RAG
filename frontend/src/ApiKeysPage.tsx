import { useEffect, useRef, useState } from "react";
import { Alert, App, Button, Form, Input, Modal, Select, Switch, Table, Tag, Tooltip } from "antd";
import { CopyOutlined, DeleteOutlined, EditOutlined, PlusOutlined, ReloadOutlined } from "@ant-design/icons";
import { api, apiResponse, errorText } from "./api";
import { namespacePattern } from "./navigation";

type ApiKey = { id: string; name: string; prefix: string; namespaces: string[]; enabled: boolean; exportable: boolean; created_at: string };
type KeyValues = { name: string; namespaces: string[]; enabled: boolean };

export function ApiKeysPage({ namespaces }: { namespaces: string[] }) {
  const { message, modal } = App.useApp();
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState(false);
  const [authorized, setAuthorized] = useState(false);
  const [editing, setEditing] = useState<ApiKey>();
  const [open, setOpen] = useState(false);
  const [secret, setSecret] = useState("");
  const [form] = Form.useForm<KeyValues>();
  const request = useRef<AbortController | null>(null);

  async function load() {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setLoading(true);
    setError("");
    setKeys([]);
    setAuthorized(false);
    try {
      const result = await api<{ keys: ApiKey[] }>("/v1/admin/api-keys", undefined, { signal: controller.signal });
      if (!controller.signal.aborted) { setKeys(result.keys); setAuthorized(true); }
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

  const options = [...new Set([...namespaces, ...keys.flatMap((key) => key.namespaces)])].map((value) => ({ value, label: value }));
  return (
    <section aria-label="API Key 页面">
      <div className="page-heading">
        <h1>API Key</h1>
        <Button type="primary" icon={<PlusOutlined />} disabled={!authorized || busy || loading} onClick={() => edit()}>创建密钥</Button>
      </div>
      <Button icon={<ReloadOutlined />} loading={loading} disabled={busy} onClick={() => void load()}>刷新</Button>
      {error && <Alert className="page-alert" type="error" showIcon title="无法管理密钥" description={error} />}
      <Table<ApiKey> rowKey="id" dataSource={keys} loading={loading} scroll={{ x: 720 }}
        locale={{ emptyText: authorized ? "暂无 API Key" : "尚未连接" }} pagination={{ pageSize: 10, hideOnSinglePage: true }}
        columns={[
          { title: "名称", dataIndex: "name", width: 180, render: (name: string) => <span className="break-text">{name}</span> },
          { title: "密钥", dataIndex: "prefix", width: 150, render: (prefix: string) => <code>{prefix}…</code> },
          { title: "授权 Namespace", dataIndex: "namespaces", render: (values: string[]) => <div className="key-namespaces">{values.map((value) => <Tag key={value}>{value}</Tag>)}</div> },
          { title: "启用", width: 80, render: (_, key) => <Switch aria-label={"启用 " + key.name} checked={key.enabled} disabled={busy || loading} onChange={(enabled) => void setEnabled(key, enabled)} /> },
          { title: "操作", width: 136, render: (_, key) => <div className="toolbar">
            <Tooltip title={!key.exportable ? "旧密钥无法恢复，请重新创建" : !key.enabled ? "密钥已停用" : "复制 MCP JSON"}><Button type="text" aria-label={"复制 MCP JSON " + key.name} icon={<CopyOutlined />} disabled={busy || loading || !key.enabled || !key.exportable} onClick={() => void copyMcp(key)} /></Tooltip>
            <Tooltip title="编辑"><Button type="text" aria-label={"编辑 " + key.name} icon={<EditOutlined />} disabled={busy || loading} onClick={() => edit(key)} /></Tooltip>
            <Tooltip title="删除"><Button type="text" danger aria-label={"删除 " + key.name} icon={<DeleteOutlined />} disabled={busy || loading} onClick={() => remove(key)} /></Tooltip>
          </div> },
        ]} />
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
