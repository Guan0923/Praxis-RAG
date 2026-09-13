import { useEffect, useRef, useState } from "react";
import { Alert, App, AutoComplete, Badge, Button, Form, Input, Tooltip } from "antd";
import { ApiOutlined, DeleteOutlined, ReloadOutlined, SaveOutlined } from "@ant-design/icons";
import { api, errorText, type Connection } from "./api";

type Kind = "embedding" | "reranker";
type ModelState = {
  base_url: string;
  model: string;
  api_key_set: boolean;
  configured: boolean;
  vector_size: number | null;
  status?: "ok" | "error";
  last_error?: string;
  checked_at?: string;
};
type Settings = { models: Record<Kind, ModelState>; writable: boolean };
type Values = { base_url: string; apikey: string; model: string };

export function ModelSettings({ connection, disabled, onApplied }: {
  connection: Connection;
  disabled: boolean;
  onApplied: () => void;
}) {
  const [data, setData] = useState<Settings>();
  const [error, setError] = useState("");
  const [reload, setReload] = useState(0);
  useEffect(() => {
    const controller = new AbortController();
    setData(undefined);
    setError("");
    void api<Settings>("/v1/model-settings", connection, { signal: controller.signal })
      .then((result) => { if (!controller.signal.aborted) setData(result); })
      .catch((reason) => { if (!controller.signal.aborted) setError(errorText(reason)); });
    return () => controller.abort();
  }, [connection.token, reload]);

  if (!data) return <div className="settings-section">
    <Alert
      type={error ? "error" : "info"}
      title={error || "正在读取模型配置"}
      action={error ? <Button aria-label="重新读取模型配置" icon={<ReloadOutlined />} onClick={() => setReload((value) => value + 1)} /> : undefined}
    />
  </div>;

  return <>
    {!data.writable && <Alert className="page-alert" type="info" title="模型设置只读" />}
    {(["embedding", "reranker"] as const).map((kind) => <ModelForm
      key={kind}
      kind={kind}
      initial={data.models[kind]}
      connection={connection}
      disabled={disabled || !data.writable}
      onApplied={onApplied}
    />)}
  </>;
}

function ModelForm({ kind, initial, connection, disabled, onApplied }: {
  kind: Kind;
  initial: ModelState;
  connection: Connection;
  disabled: boolean;
  onApplied: () => void;
}) {
  const { message } = App.useApp();
  const [form] = Form.useForm<Values>();
  const [saved, setSaved] = useState(initial);
  const [checked, setChecked] = useState<ModelState>(initial);
  const [options, setOptions] = useState<{ value: string }[]>([]);
  const [modelQuery, setModelQuery] = useState("");
  const [busy, setBusy] = useState<"models" | "test" | "save">();
  const [error, setError] = useState("");
  const [keyEdited, setKeyEdited] = useState(false);
  const running = useRef(false);
  const lifetime = useRef(new AbortController());
  const title = kind === "embedding" ? "Embedding" : "Reranker";
  useEffect(() => () => lifetime.current.abort(), []);

  async function run(action: "models" | "test" | "save") {
    if (running.current || disabled) return;
    running.current = true;
    try {
      await form.validateFields(action === "models" ? ["base_url"] : undefined);
      const values = form.getFieldsValue(true);
      const body = {
        base_url: values.base_url.trim(),
        model: (values.model || "").trim(),
        ...(keyEdited ? { apikey: values.apikey || "" } : {}),
      };
      setBusy(action);
      setError("");
      const endpoint = `/v1/model-settings/${kind}`;
      if (action === "models") {
        const result = await api<{ models: string[] }>(`${endpoint}/models`, connection, {
          method: "POST", body: JSON.stringify(body), signal: lifetime.current.signal,
        });
        if (lifetime.current.signal.aborted) return;
        setOptions(result.models.map((model) => ({ value: model })));
        setModelQuery("");
        void message.info(result.models.length ? `已获取 ${result.models.length} 个模型` : "模型列表为空，可手动填写");
      } else {
        const result = await api<ModelState>(action === "save" ? endpoint : `${endpoint}/test`, connection, {
          method: action === "save" ? "PATCH" : "POST",
          body: JSON.stringify(body), signal: lifetime.current.signal,
        });
        if (lifetime.current.signal.aborted) return;
        setChecked(result);
        if (action === "save") {
          setSaved(result);
          setKeyEdited(false);
          form.setFieldsValue({ base_url: result.base_url, model: result.model, apikey: "" });
          onApplied();
        }
        void message.success(action === "save" ? `${title} 配置已生效` : `${title} 连接成功`);
      }
    } catch (reason) {
      if (!lifetime.current.signal.aborted && !(reason && typeof reason === "object" && "errorFields" in reason)) {
        setError(errorText(reason));
        if (action !== "models") setChecked((value) => ({ ...value, status: "error" }));
      }
    } finally {
      running.current = false;
      if (!lifetime.current.signal.aborted) setBusy(undefined);
    }
  }

  const status = checked.status === "ok" ? "连接成功" : checked.status === "error" ? "连接失败"
    : checked.configured ? "未测试" : "未配置";
  return <section className="settings-section" aria-label={`${title} 模型配置`}>
    <div className="section-heading">
      <h2>{title}</h2>
      <Badge status={checked.status === "ok" ? "success" : checked.status === "error" ? "error" : "default"} text={status} />
    </div>
    <Form
      name={`model-${kind}`}
      form={form}
      layout="vertical"
      className="connection-form model-api-form"
      disabled={disabled || !!busy}
      initialValues={{ base_url: initial.base_url, model: initial.model, apikey: "" }}
      onFinish={() => void run("save")}
      onValuesChange={(changed) => {
        setError("");
        setChecked({ ...saved, status: undefined });
        if ("apikey" in changed) setKeyEdited(true);
        if ("base_url" in changed) {
          setOptions([]);
          if ((changed.base_url || "").trim().replace(/\/+$/, "") !== saved.base_url) {
            form.setFieldValue("apikey", "");
            setKeyEdited(true);
          }
        }
      }}
    >
      <Form.Item name="base_url" label="base_url" rules={[
        { required: true, whitespace: true, message: "请输入模型服务地址" },
        { validator: (_, value) => {
          try {
            const url = new URL(value.trim());
            if (!["http:", "https:"].includes(url.protocol) || url.username || url.password || url.search || url.hash) throw new Error();
            return Promise.resolve();
          } catch { return Promise.reject(new Error("请输入不含密钥或查询参数的 HTTP / HTTPS 地址")); }
        } },
      ]}>
        <Input autoComplete="off" maxLength={2048} />
      </Form.Item>
      <div className="model-key-row">
        <Form.Item name="apikey" label="apikey">
          <Input.Password autoComplete="new-password" allowClear placeholder={saved.api_key_set && !keyEdited ? "已保存" : "可留空"} />
        </Form.Item>
        {saved.api_key_set && <Tooltip title="清除已保存的 apikey">
          <Button
            aria-label={`清除 ${title} 已保存的 apikey`}
            icon={<DeleteOutlined />}
            onClick={() => {
              form.setFieldValue("apikey", "");
              setKeyEdited(true);
              setChecked({ ...saved, status: undefined });
            }}
          />
        </Tooltip>}
      </div>
      <Form.Item name="model" label="model" rules={[{ required: true, whitespace: true, message: "请选择或填写模型名称" }]}>
        <AutoComplete
          virtual={false}
          options={options}
          maxLength={512}
          allowClear
          onFocus={() => setModelQuery("")}
          onSearch={setModelQuery}
          onSelect={() => setModelQuery("")}
          filterOption={(_, option) => (option?.value || "").toLowerCase().includes(modelQuery.toLowerCase())}
        />
      </Form.Item>
      <div className="model-api-actions">
        <Button aria-label="获取模型列表" icon={<ReloadOutlined />} loading={busy === "models"} onClick={() => void run("models")}>获取模型列表</Button>
        <Button aria-label="测试连接" icon={<ApiOutlined />} loading={busy === "test"} onClick={() => void run("test")}>测试连接</Button>
        <Button aria-label="保存并应用" icon={<SaveOutlined />} loading={busy === "save"} type="primary" htmlType="submit">保存并应用</Button>
      </div>
      {checked.checked_at && <div className="secondary model-check-time">最近测试：{new Date(checked.checked_at).toLocaleString()}</div>}
      {error && <Alert className="model-api-error" showIcon type="error" title={error} />}
    </Form>
  </section>;
}
