import { useEffect, useRef, useState } from "react";
import { Alert, App, Button, Form, Input, InputNumber, Select } from "antd";
import { PlusOutlined, ReloadOutlined } from "@ant-design/icons";
import { api, errorText, type Chunking, type KnowledgeBase, type Strategy } from "./api";
import { namespacePattern } from "./navigation";

type Defaults = { vector_size: number; embedding_model: string; embedding_configured: boolean; tokenizer: string; default: string; strategies: Strategy[] };
type Values = Chunking & { namespace: string; vector_size: number };

export function CreateKnowledgeBase({ disabled, onCreated }: { disabled: boolean; onCreated: (namespace: string) => void }) {
  const { message } = App.useApp();
  const [form] = Form.useForm<Values>();
  const [defaults, setDefaults] = useState<Defaults>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const controller = useRef<AbortController | null>(null);
  const strategy = Form.useWatch("strategy", form);
  const unit = defaults?.strategies.find((item) => item.id === strategy)?.unit === "characters" ? "字符" : "token";

  async function loadDefaults() {
    controller.current?.abort();
    const request = new AbortController();
    controller.current = request;
    setLoading(true);
    setDefaults(undefined);
    setError("");
    try {
      const data = await api<Defaults>("/v1/admin/knowledge-bases/defaults", undefined, { signal: request.signal });
      if (request.signal.aborted) return;
      setDefaults(data);
      const initial = data.strategies.find((item) => item.id === data.default);
      form.setFieldsValue({ vector_size: data.vector_size, strategy: initial?.id,
        chunk_size: initial?.chunk_size, chunk_overlap: initial?.chunk_overlap });
    } catch (reason) {
      if (!request.signal.aborted) setError(errorText(reason));
    } finally {
      if (!request.signal.aborted) setLoading(false);
    }
  }
  useEffect(() => { void loadDefaults(); return () => controller.current?.abort(); }, []);

  async function create(values: Values) {
    setSaving(true);
    setError("");
    try {
      const result = await api<KnowledgeBase>("/v1/admin/knowledge-bases", undefined,
        { method: "POST", body: JSON.stringify(values) });
      void message.success("知识库已创建");
      form.setFieldValue("namespace", "");
      onCreated(result.namespace);
    } catch (reason) { setError(errorText(reason)); }
    finally { setSaving(false); }
  }

  return <div className="settings-section knowledge-base-create">
    <h2>创建新知识库</h2>
    <div className="key-auth-form">
      <Button icon={<ReloadOutlined />} loading={loading} disabled={disabled || saving} onClick={() => void loadDefaults()}>读取配置</Button>
    </div>
    {error && <Alert type="error" showIcon className="page-alert" title="知识库配置失败" description={error} />}
    {defaults && !defaults.embedding_configured && <Alert type="warning" showIcon className="page-alert" title="请先在模型配置中保存并验证 Embedding 模型" />}
    <Form form={form} layout="vertical" disabled={disabled || saving || !defaults} onFinish={(values) => void create(values)}>
      <Form.Item name="namespace" label="知识库标识（Namespace）" rules={[
        { required: true, message: "请输入知识库标识" },
        { pattern: namespacePattern, message: "使用 1–64 个字母、数字、点、下划线或横线，首位为字母或数字" },
      ]}><Input maxLength={64} autoComplete="off" /></Form.Item>
      <div className="chunk-controls">
        <Form.Item label="Embedding 模型"><Input value={defaults?.embedding_model || ""} readOnly /></Form.Item>
        <Form.Item name="vector_size" label="向量维度（vector len）" rules={[
          { required: true, type: "integer", min: 1, max: 65536, message: "请输入有效的向量维度" },
          { validator: (_, value) => value === defaults?.vector_size ? Promise.resolve() : Promise.reject(new Error("必须与当前 Embedding 模型输出维度一致：" + defaults?.vector_size)) },
        ]}><InputNumber min={1} max={65536} /></Form.Item>
      </div>
      <Form.Item name="strategy" label="切分方式" rules={[{ required: true, message: "请选择切分方式" }]}>
        <Select virtual={false} options={defaults?.strategies.map((item) => ({ value: item.id, label: item.label }))}
          onChange={(id) => {
            const selected = defaults?.strategies.find((item) => item.id === id);
            if (selected) form.setFieldsValue({ chunk_size: selected.chunk_size, chunk_overlap: selected.chunk_overlap });
          }} />
      </Form.Item>
      <div className="chunk-controls">
        <Form.Item name="chunk_size" label={"块长度（" + unit + "）"} dependencies={["strategy"]} rules={[
          { required: true, type: "integer", min: strategy === "character" ? 100 : 1, max: 100000, message: "请输入有效的块长度" },
        ]}><InputNumber min={strategy === "character" ? 100 : 1} max={100000} /></Form.Item>
        <Form.Item name="chunk_overlap" label={"重叠长度（" + unit + "）"} dependencies={["chunk_size"]} rules={[
          { required: true, type: "integer", min: 0, message: "请输入非负整数" },
          ({ getFieldValue }) => ({ validator: (_, value) => value < getFieldValue("chunk_size") ? Promise.resolve() : Promise.reject(new Error("重叠长度必须小于块长度")) }),
        ]}><InputNumber min={0} /></Form.Item>
      </div>
      <Button htmlType="submit" type="primary" icon={<PlusOutlined />} loading={saving} disabled={disabled || !defaults?.embedding_configured || loading}>创建知识库</Button>
    </Form>
  </div>;
}
