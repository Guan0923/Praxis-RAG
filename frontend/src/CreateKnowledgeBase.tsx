import { useEffect, useRef, useState } from "react";
import { Alert, App, Button, Descriptions, Form, Input, InputNumber, Select, Switch, Tooltip } from "antd";
import { DeleteOutlined, PlusOutlined, ReloadOutlined, SaveOutlined } from "@ant-design/icons";
import { api, errorText, type Chunking, type KnowledgeBase, type Strategy } from "./api";
import { namespacePattern } from "./navigation";

type Kind = "embedding" | "reranker";
type ProviderValues = { base_url: string; apikey?: string };
type Values = Chunking & { namespace: string; embedding_model: string; reranker_model?: string;
  embedding: ProviderValues; reranker: ProviderValues };
type Defaults = { default: string; strategies: Strategy[] };
const kinds: Kind[] = ["embedding", "reranker"];
const cleanUrl = (value: string) => value.trim().replace(/\/+$/, "");

export function CreateKnowledgeBase({ active, disabled, onCreated }: {
  active: boolean; disabled: boolean; onCreated: (namespace: string) => void;
}) {
  return active ? <KnowledgeBaseForm disabled={disabled} onSaved={(item) => onCreated(item.namespace)} /> : null;
}

export function KnowledgeBaseForm({ initial, disabled, onSaved, onBusyChange }: {
  initial?: KnowledgeBase; disabled: boolean; onSaved: (item: KnowledgeBase) => void;
  onBusyChange?: (busy: boolean) => void;
}) {
  const { message, modal } = App.useApp();
  const [form] = Form.useForm<Values>();
  const [defaults, setDefaults] = useState<Defaults>();
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [rerankerEnabled, setRerankerEnabled] = useState(Boolean(initial?.reranker_model));
  const [models, setModels] = useState<Record<Kind, string[]>>({ embedding: [], reranker: [] });
  const [loading, setLoading] = useState({ embedding: false, reranker: false });
  const [visibleKeys, setVisibleKeys] = useState({ embedding: false, reranker: false });
  const [editedKeys, setEditedKeys] = useState({ embedding: false, reranker: false });
  const modelRequests = useRef<Partial<Record<Kind, AbortController>>>({});
  const keyRequests = useRef<Partial<Record<Kind, AbortController>>>({});
  const lifetime = useRef(new AbortController());
  const running = useRef(false);
  const embeddingModel = Form.useWatch("embedding_model", form);
  const embeddingUrl = Form.useWatch(["embedding", "base_url"], form);
  const strategy = Form.useWatch("strategy", form);
  const reembedRequired = !!initial && (embeddingModel !== initial.embedding_model ||
    cleanUrl(embeddingUrl || "") !== initial.embedding.base_url);
  const locked = disabled || saving;
  const unit = defaults?.strategies.find((item) => item.id === strategy)?.unit === "characters" ? "字符" : "token";

  async function loadDefaults() {
    setError("");
    try {
      const data = await api<Defaults>("/v1/admin/knowledge-bases/defaults", undefined, { signal: lifetime.current.signal });
      if (lifetime.current.signal.aborted) return;
      setDefaults(data);
      if (!initial && !form.getFieldValue("strategy")) {
        const selected = data.strategies.find((item) => item.id === data.default);
        form.setFieldsValue({ strategy: selected?.id, chunk_size: selected?.chunk_size, chunk_overlap: selected?.chunk_overlap });
      }
    } catch (reason) { if (!lifetime.current.signal.aborted) setError(errorText(reason)); }
  }
  useEffect(() => {
    void loadDefaults();
    return () => {
      lifetime.current.abort();
      kinds.forEach((kind) => { modelRequests.current[kind]?.abort(); keyRequests.current[kind]?.abort(); });
    };
  }, []);

  function providerValues(kind: Kind): ProviderValues {
    const value = form.getFieldValue(kind) as ProviderValues;
    return { base_url: value.base_url.trim(), ...(!initial || editedKeys[kind] ? { apikey: value.apikey || "" } : {}) };
  }

  async function loadModels(kind: Kind) {
    try { await form.validateFields([[kind, "base_url"]]); } catch { return; }
    modelRequests.current[kind]?.abort();
    const controller = new AbortController();
    modelRequests.current[kind] = controller;
    setLoading((value) => ({ ...value, [kind]: true }));
    setModels((value) => ({ ...value, [kind]: [] }));
    setError("");
    try {
      const result = await api<{ models: string[] }>(
        "/v1/admin/knowledge-bases/models/" + kind, undefined,
        { method: "POST", body: JSON.stringify({ ...providerValues(kind), namespace: initial?.namespace }), signal: controller.signal });
      if (!controller.signal.aborted) setModels((value) => ({ ...value, [kind]: result.models }));
    } catch (reason) { if (!controller.signal.aborted) setError(errorText(reason)); }
    finally { if (!controller.signal.aborted) setLoading((value) => ({ ...value, [kind]: false })); }
  }

  async function showKey(kind: Kind, visible: boolean) {
    if (visible && initial?.[kind].api_key_set && !editedKeys[kind] && !form.getFieldValue([kind, "apikey"])) {
      keyRequests.current[kind]?.abort();
      const controller = new AbortController();
      keyRequests.current[kind] = controller;
      try {
        const result = await api<{ apikey: string }>("/v1/admin/knowledge-bases/" + initial.namespace + "/key/" + kind,
          undefined, { signal: controller.signal, cache: "no-store" });
        if (controller.signal.aborted) return;
        form.setFieldValue([kind, "apikey"], result.apikey);
      } catch (reason) { if (!controller.signal.aborted) setError(errorText(reason)); return; }
    }
    setVisibleKeys((value) => ({ ...value, [kind]: visible }));
  }

  async function save(values: Values, reembed: boolean) {
    if (running.current) return;
    running.current = true;
    setSaving(true);
    onBusyChange?.(true);
    setError("");
    const body = { embedding: providerValues("embedding"), embedding_model: values.embedding_model,
      reranker: rerankerEnabled ? providerValues("reranker") : { base_url: "", apikey: "" },
      reranker_model: rerankerEnabled ? values.reranker_model || "" : "" };
    try {
      const result = await api<KnowledgeBase>("/v1/admin/knowledge-bases" + (initial ? "/" + initial.namespace : ""), undefined,
        { method: initial ? "PATCH" : "POST", body: JSON.stringify(initial ? { ...body, reembed } : {
          ...body, namespace: values.namespace, strategy: values.strategy, chunk_size: values.chunk_size, chunk_overlap: values.chunk_overlap,
        }) });
      if (!lifetime.current.signal.aborted) {
        void message.success(initial ? reembed ? "重新向量化完成，配置已切换" : "知识库配置已保存" : "知识库已创建");
        onSaved(result);
      }
    } catch (reason) { if (!lifetime.current.signal.aborted) setError(errorText(reason)); }
    finally { running.current = false; setSaving(false); onBusyChange?.(false); }
  }

  function submit(values: Values, reembed = reembedRequired) {
    if (reembed) modal.confirm({ title: "重新向量化并保存", content: "将使用新配置重新向量化该知识库的全部文档。全部成功后才切换；失败保留原配置和索引。",
      okText: "开始重新向量化", cancelText: "取消", onOk: () => save(values, true) });
    else void save(values, false);
  }

  return <div className="knowledge-base-form">
    {error && <Alert type="error" showIcon className="page-alert" title="知识库配置失败" description={error}
      action={!defaults ? <Button icon={<ReloadOutlined />} aria-label="重新读取切分配置" onClick={() => void loadDefaults()} /> : undefined} />}
    {initial && <Descriptions column={1} size="small" className="knowledge-base-summary" items={[
      { key: "namespace", label: "知识库", children: initial.namespace },
      { key: "dimension", label: "向量维度", children: initial.vector_size },
      { key: "collection", label: "索引集合", children: <span className="break-text">{initial.collection}</span> },
    ]} />}
    <Form form={form} layout="vertical" disabled={locked} onFinish={(values) => submit(values)}
      initialValues={{ namespace: initial?.namespace || "", embedding_model: initial?.embedding_model || "", reranker_model: initial?.reranker_model || "",
        embedding: { base_url: initial?.embedding.base_url || "", apikey: "" }, reranker: { base_url: initial?.reranker.base_url || "", apikey: "" },
        strategy: initial?.strategy, chunk_size: initial?.chunk_size, chunk_overlap: initial?.chunk_overlap }}
      onValuesChange={(changed) => {
        setError("");
        kinds.forEach((kind) => {
          if (!changed[kind]) return;
          if ("apikey" in changed[kind]) {
            keyRequests.current[kind]?.abort();
            setEditedKeys((value) => ({ ...value, [kind]: true }));
          }
          if ("base_url" in changed[kind]) {
            modelRequests.current[kind]?.abort(); keyRequests.current[kind]?.abort();
            setLoading((value) => ({ ...value, [kind]: false }));
            setModels((value) => ({ ...value, [kind]: [] }));
            form.setFieldValue([kind, "apikey"], "");
            setEditedKeys((value) => ({ ...value, [kind]: true }));
            setVisibleKeys((value) => ({ ...value, [kind]: false }));
          }
        });
      }}>
      {!initial && <Form.Item name="namespace" label="知识库标识（Namespace）" rules={[
        { required: true, message: "请输入知识库标识" }, { pattern: namespacePattern, message: "使用字母、数字、点、下划线或横线，首位为字母或数字" },
      ]}><Input maxLength={64} autoComplete="off" /></Form.Item>}
      {kinds.map((kind) => {
        const title = kind === "embedding" ? "Embedding" : "Reranker";
        const enabled = kind === "embedding" || rerankerEnabled;
        return <section className="knowledge-provider" aria-label={title + " 配置"} key={kind}>
          <div className="section-heading"><h2>{title}</h2>{kind === "reranker" &&
            <Switch aria-label="启用 Reranker" checked={rerankerEnabled} disabled={locked} onChange={setRerankerEnabled} />}</div>
          <Form.Item name={[kind, "base_url"]} label={title + " URL"} rules={enabled ? [{ required: true, whitespace: true, message: "请输入服务 URL" }] : []}>
            <Input disabled={locked || !enabled} maxLength={2048} autoComplete="off" />
          </Form.Item>
          <div className="model-key-row">
            <Form.Item name={[kind, "apikey"]} label={title + " Key"}>
              <Input.Password disabled={locked || !enabled} autoComplete="new-password"
                placeholder={initial?.[kind].api_key_set && !editedKeys[kind] ? "已保存" : "可留空"}
                visibilityToggle={{ visible: visibleKeys[kind], onVisibleChange: (visible) => void showKey(kind, visible) }} />
            </Form.Item>
            {initial?.[kind].api_key_set && <Tooltip title="清除已保存的 Key"><Button aria-label={"清除 " + title + " Key"}
              disabled={locked || !enabled} icon={<DeleteOutlined />} onClick={() => {
                keyRequests.current[kind]?.abort(); form.setFieldValue([kind, "apikey"], "");
                setEditedKeys((value) => ({ ...value, [kind]: true }));
              }} /></Tooltip>}
          </div>
          <Form.Item name={kind + "_model"} label={title + " 模型"} rules={enabled ? [{ required: true, message: "请选择模型" }] : []}>
            <Select showSearch virtual={false} disabled={locked || !enabled} loading={loading[kind]}
              options={models[kind].map((value) => ({ value, label: value }))}
              onOpenChange={(open) => { if (open) void loadModels(kind); }} />
          </Form.Item>
        </section>;
      })}
      <section className="knowledge-provider" aria-label="切分配置"><h2>文本切分</h2>
        <Form.Item name="strategy" label="切分方式" rules={[{ required: true }]}>
          <Select disabled={locked || !!initial || !defaults} virtual={false}
            options={defaults?.strategies.map((item) => ({ value: item.id, label: item.label }))}
            onChange={(id) => { const selected = defaults?.strategies.find((item) => item.id === id);
              form.setFieldsValue({ chunk_size: selected?.chunk_size, chunk_overlap: selected?.chunk_overlap }); }} />
        </Form.Item>
        <div className="chunk-controls">
          <Form.Item name="chunk_size" label={"块长度（" + unit + "）"} rules={[{ required: true, type: "integer", min: strategy === "character" ? 100 : 1, max: 100000 }]}>
            <InputNumber disabled={locked || !!initial} min={strategy === "character" ? 100 : 1} max={100000} />
          </Form.Item>
          <Form.Item name="chunk_overlap" label={"重叠长度（" + unit + "）"} dependencies={["chunk_size"]} rules={[
            { required: true, type: "integer", min: 0 }, ({ getFieldValue }) => ({ validator: (_, value) => value < getFieldValue("chunk_size") ? Promise.resolve() : Promise.reject(new Error("重叠长度必须小于块长度")) }),
          ]}><InputNumber disabled={locked || !!initial} min={0} /></Form.Item>
        </div>
      </section>
      <div className="toolbar">
        <Button htmlType="submit" type="primary" loading={saving} disabled={disabled || !defaults}
          icon={!initial ? <PlusOutlined /> : reembedRequired ? <ReloadOutlined /> : <SaveOutlined />}>
          {!initial ? "创建知识库" : reembedRequired ? "重新向量化并保存" : "保存配置"}
        </Button>
        {initial && !reembedRequired && <Tooltip title="重新向量化并保存"><Button aria-label="重新向量化并保存"
          icon={<ReloadOutlined />} disabled={locked} onClick={() => void form.validateFields().then((values) => submit(values, true)).catch(() => {})} /></Tooltip>}
      </div>
    </Form>
  </div>;
}
