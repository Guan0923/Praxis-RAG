import { useEffect, useRef, useState } from "react";
import { Alert, App, AutoComplete, Button, Descriptions, Form, Input, InputNumber, Select, Switch, Tooltip } from "antd";
import { DeleteOutlined, PlusOutlined, ReloadOutlined, SaveOutlined } from "@ant-design/icons";
import { api, apiResponse, errorText, type Chunking, type Connection, type KnowledgeBase, type Role, type Strategy } from "./api";
import { namespacePattern } from "./navigation";

type Kind = "embedding" | "reranker" | "vlm";
type ProviderValues = { base_url: string; apikey?: string };
type Values = Chunking & { namespace: string; embedding_model: string; reranker_model?: string; vlm_model?: string;
  embedding: ProviderValues; reranker: ProviderValues; vlm: ProviderValues };
type TenantValues = Chunking & { namespace: string };
type Defaults = { default: string; strategies: Strategy[] };
const kinds: Kind[] = ["embedding", "reranker", "vlm"];
const cleanUrl = (value: string) => value.trim().replace(/\/+$/, "");
// The create form keeps an unsaved draft (without API keys) so a failed save or
// an accidental page switch never loses the other inputs.
const draftKey = "rag.knowledge-base-draft";

export function CreateKnowledgeBase({ active, disabled, onCreated, connection, role }: {
  active: boolean; disabled: boolean; onCreated: (namespace: string) => void;
  connection: Connection; role?: Role;
}) {
  // Keep the form mounted after the first visit so switching pages does not
  // discard what was typed.
  const [mounted, setMounted] = useState(active);
  useEffect(() => {
    if (active) setMounted(true);
  }, [active]);
  if (!mounted) return null;
  // Admins keep the full provider configuration; VIPs and ordinary users only
  // choose the namespace and chunking because the server clones the model
  // configuration from the administrator's default knowledge-base template.
  const simple = role === "vip" || role === "user";
  return <div hidden={!active}>
    {simple
      ? <TenantKnowledgeBaseForm disabled={disabled} onCreated={onCreated} connection={connection} />
      : <KnowledgeBaseForm disabled={disabled} onSaved={(item) => onCreated(item.namespace)} />}
  </div>;
}

function TenantKnowledgeBaseForm({ disabled, onCreated, connection }: {
  disabled: boolean; onCreated: (namespace: string) => void; connection: Connection;
}) {
  const { message } = App.useApp();
  const [form] = Form.useForm<TenantValues>();
  const [catalog, setCatalog] = useState<Defaults>();
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [loading, setLoading] = useState(false);
  const lifetime = useRef(new AbortController());
  const strategy = Form.useWatch("strategy", form);
  const unit = catalog?.strategies.find((item) => item.id === strategy)?.unit === "characters" ? "字符" : "token";
  const locked = disabled || saving;

  async function loadCatalog() {
    setLoading(true);
    setError("");
    try {
      const data = await api<Defaults>("/v1/chunking/strategies", connection, { signal: lifetime.current.signal });
      if (lifetime.current.signal.aborted) return;
      setCatalog(data);
      if (!form.getFieldValue("strategy")) {
        const selected = data.strategies.find((item) => item.id === data.default) || data.strategies[0];
        form.setFieldsValue({ strategy: selected?.id, chunk_size: selected?.chunk_size, chunk_overlap: selected?.chunk_overlap });
      }
    } catch (reason) {
      if (!lifetime.current.signal.aborted) setError(errorText(reason));
    } finally {
      if (!lifetime.current.signal.aborted) setLoading(false);
    }
  }
  useEffect(() => {
    void loadCatalog();
    return () => lifetime.current.abort();
  }, []);

  async function save(values: TenantValues) {
    if (saving) return;
    setSaving(true);
    setError("");
    try {
      const response = await apiResponse("/v1/knowledge-bases", connection, {
        method: "POST",
        body: JSON.stringify({
          namespace: values.namespace.trim(),
          strategy: values.strategy,
          chunk_size: values.chunk_size,
          chunk_overlap: values.chunk_overlap,
        }),
      });
      // The endpoint normally returns the created knowledge base; tolerate an
      // empty 201 body as well.
      let created: Partial<KnowledgeBase> = {};
      try {
        created = await response.json() as Partial<KnowledgeBase>;
      } catch {
        created = {};
      }
      if (lifetime.current.signal.aborted) return;
      const namespace = created.namespace || values.namespace.trim();
      void message.success("知识库已创建");
      form.resetFields();
      onCreated(namespace);
      void loadCatalog();
    } catch (reason) {
      if (!lifetime.current.signal.aborted) setError(errorText(reason));
    } finally {
      if (!lifetime.current.signal.aborted) setSaving(false);
    }
  }

  return <div className="knowledge-base-form">
    <Alert
      className="page-alert"
      type="info"
      showIcon
      title="模型配置沿用管理员模板"
      description="无需填写 Embedding / Reranker / VLM 配置：服务端会从管理员配置的默认知识库模板克隆模型、服务地址与向量维度。"
    />
    {error && <Alert type="error" showIcon className="page-alert" title="知识库创建失败" description={error}
      action={!catalog ? <Button icon={<ReloadOutlined />} aria-label="重新读取切分配置" loading={loading} onClick={() => void loadCatalog()} /> : undefined} />}
    <Form<TenantValues>
      form={form}
      layout="vertical"
      disabled={locked}
      scrollToFirstError
      onFinish={(values) => void save(values)}
      initialValues={{ namespace: "", strategy: undefined, chunk_size: undefined, chunk_overlap: undefined }}
    >
      <Form.Item name="namespace" label="知识库标识（Namespace）" rules={[
        { required: true, whitespace: true, message: "请输入知识库标识" },
        { pattern: namespacePattern, message: "使用字母、数字、点、下划线或横线，首位为字母或数字" },
      ]}>
        <Input maxLength={64} autoComplete="off" />
      </Form.Item>
      <Form.Item name="strategy" label="切分方式" rules={[{ required: true, message: "请选择切分方式" }]}>
        <Select
          disabled={locked || !catalog}
          virtual={false}
          options={catalog?.strategies.map((item) => ({ value: item.id, label: item.label }))}
          onChange={(id) => {
            const selected = catalog?.strategies.find((item) => item.id === id);
            form.setFieldsValue({ chunk_size: selected?.chunk_size, chunk_overlap: selected?.chunk_overlap });
          }}
        />
      </Form.Item>
      <div className="chunk-controls">
        <Form.Item name="chunk_size" label={"块长度（" + unit + "）"} rules={[
          { required: true, type: "integer", min: strategy === "character" ? 100 : 1, max: 100000 },
        ]}>
          <InputNumber disabled={locked || !catalog} min={strategy === "character" ? 100 : 1} max={100000} />
        </Form.Item>
        <Form.Item name="chunk_overlap" label={"重叠长度（" + unit + "）"} dependencies={["chunk_size"]} rules={[
          { required: true, type: "integer", min: 0 },
          ({ getFieldValue }) => ({
            validator: (_, value) => value < getFieldValue("chunk_size")
              ? Promise.resolve()
              : Promise.reject(new Error("重叠长度必须小于块长度")),
          }),
        ]}>
          <InputNumber disabled={locked || !catalog} min={0} />
        </Form.Item>
      </div>
      <div className="toolbar">
        <Button htmlType="submit" type="primary" loading={saving} disabled={disabled || !catalog}
          icon={<PlusOutlined />}>
          创建知识库
        </Button>
      </div>
    </Form>
  </div>;
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
  const [vlmEnabled, setVlmEnabled] = useState(Boolean(initial?.vlm_model));
  const [models, setModels] = useState<Record<Kind, string[]>>({ embedding: [], reranker: [], vlm: [] });
  const [modelsLoaded, setModelsLoaded] = useState<Record<Kind, boolean>>({ embedding: false, reranker: false, vlm: false });
  const [modelHints, setModelHints] = useState<Record<Kind, string>>({ embedding: "", reranker: "", vlm: "" });
  const [loading, setLoading] = useState({ embedding: false, reranker: false, vlm: false });
  const [visibleKeys, setVisibleKeys] = useState({ embedding: false, reranker: false, vlm: false });
  const [editedKeys, setEditedKeys] = useState({ embedding: false, reranker: false, vlm: false });
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

  // Restore an unsaved create draft (API keys are never written to storage).
  useEffect(() => {
    if (initial) return;
    try {
      const raw = sessionStorage.getItem(draftKey);
      if (!raw) return;
      const draft = JSON.parse(raw) as Partial<Values> | null;
      if (!draft || typeof draft !== "object") return;
      form.setFieldsValue(draft as Values);
      if (typeof draft.reranker_model === "string" && draft.reranker_model) setRerankerEnabled(true);
      if (typeof draft.vlm_model === "string" && draft.vlm_model) setVlmEnabled(true);
    } catch {
      // An unreadable draft must never block the form.
    }
  }, []);

  // saveDraft persists the create form between navigation and page reloads so a
  // validation error never discards the other inputs.
  function saveDraft() {
    if (initial) return;
    try {
      const values = form.getFieldsValue(true) as Values;
      const draft = {
        ...values,
        embedding: { base_url: values.embedding?.base_url || "" },
        reranker: { base_url: values.reranker?.base_url || "" },
        vlm: { base_url: values.vlm?.base_url || "" },
      };
      sessionStorage.setItem(draftKey, JSON.stringify(draft));
    } catch {
      // Draft persistence is best effort only.
    }
  }

  function providerValues(kind: Kind): ProviderValues {
    const value = form.getFieldValue(kind) as ProviderValues;
    return { base_url: value.base_url.trim(), ...(!initial || editedKeys[kind] ? { apikey: value.apikey || "" } : {}) };
  }

  async function loadModels(kind: Kind) {
    if (modelsLoaded[kind] || loading[kind]) return;
    try { await form.validateFields([[kind, "base_url"]]); } catch { return; }
    modelRequests.current[kind]?.abort();
    const controller = new AbortController();
    modelRequests.current[kind] = controller;
    setLoading((value) => ({ ...value, [kind]: true }));
    setModels((value) => ({ ...value, [kind]: [] }));
    try {
      const result = await api<{ models: string[] }>(
        "/v1/admin/knowledge-bases/models/" + kind, undefined,
        { method: "POST", body: JSON.stringify({ ...providerValues(kind), namespace: initial?.namespace }), signal: controller.signal });
      if (!controller.signal.aborted) {
        setModels((value) => ({ ...value, [kind]: result.models }));
        setModelsLoaded((value) => ({ ...value, [kind]: true }));
        setModelHints((value) => ({
          ...value,
          [kind]: result.models.length > 0 ? "" : "该地址未返回模型列表，可直接输入模型名称",
        }));
      }
    } catch {
      // A provider without a /models endpoint is normal for rerankers. Model
      // listing is only a convenience: keep the field editable and show a
      // neutral hint instead of failing the form.
      if (!controller.signal.aborted) {
        setModelsLoaded((value) => ({ ...value, [kind]: true }));
        setModelHints((value) => ({ ...value, [kind]: "无法从该地址获取模型列表，可直接输入模型名称" }));
      }
    } finally { if (!controller.signal.aborted) setLoading((value) => ({ ...value, [kind]: false })); }
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
      reranker_model: rerankerEnabled ? values.reranker_model || "" : "",
      vlm: vlmEnabled ? providerValues("vlm") : { base_url: "", apikey: "" },
      vlm_model: vlmEnabled ? values.vlm_model || "" : "" };
    try {
      const result = await api<KnowledgeBase>("/v1/admin/knowledge-bases" + (initial ? "/" + initial.namespace : ""), undefined,
        { method: initial ? "PATCH" : "POST", body: JSON.stringify(initial ? { ...body, reembed } : {
          ...body, namespace: values.namespace, strategy: values.strategy, chunk_size: values.chunk_size, chunk_overlap: values.chunk_overlap,
        }) });
      if (!lifetime.current.signal.aborted) {
        void message.success(initial ? reembed ? "重新向量化完成，配置已切换" : "知识库配置已保存" : "知识库已创建");
        if (!initial) {
          try { sessionStorage.removeItem(draftKey); } catch { /* Draft cleanup is best effort. */ }
          form.resetFields();
          setRerankerEnabled(false);
          setVlmEnabled(false);
          setModelsLoaded({ embedding: false, reranker: false, vlm: false });
          setModelHints({ embedding: "", reranker: "", vlm: "" });
          void loadDefaults();
        }
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
    <Form form={form} layout="vertical" disabled={locked} scrollToFirstError onFinish={(values) => submit(values)}
      initialValues={{ namespace: initial?.namespace || "", embedding_model: initial?.embedding_model || "", reranker_model: initial?.reranker_model || "",
        vlm_model: initial?.vlm_model || "",
        embedding: { base_url: initial?.embedding.base_url || "", apikey: "" }, reranker: { base_url: initial?.reranker.base_url || "", apikey: "" },
        vlm: { base_url: initial?.vlm?.base_url || "", apikey: "" },
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
            setModelsLoaded((value) => ({ ...value, [kind]: false }));
            setModelHints((value) => ({ ...value, [kind]: "" }));
            form.setFieldValue([kind, "apikey"], "");
            setEditedKeys((value) => ({ ...value, [kind]: true }));
            setVisibleKeys((value) => ({ ...value, [kind]: false }));
          }
        });
        saveDraft();
      }}>
      {!initial && <Form.Item name="namespace" label="知识库标识（Namespace）" rules={[
        { required: true, message: "请输入知识库标识" }, { pattern: namespacePattern, message: "使用字母、数字、点、下划线或横线，首位为字母或数字" },
      ]}><Input maxLength={64} autoComplete="off" /></Form.Item>}
      {kinds.map((kind) => {
        const title = kind === "embedding" ? "Embedding" : kind === "reranker" ? "Reranker" : "VLM";
        const enabled = kind === "embedding" || (kind === "reranker" ? rerankerEnabled : vlmEnabled);
        return <section className="knowledge-provider" aria-label={title + " 配置"} key={kind}>
          <div className="section-heading"><h2>{title}</h2>{kind === "reranker" &&
            <Switch aria-label="启用 Reranker" checked={rerankerEnabled} disabled={locked} onChange={setRerankerEnabled} />}
            {kind === "vlm" &&
            <Switch aria-label="启用 VLM" checked={vlmEnabled} disabled={locked} onChange={setVlmEnabled} />}</div>
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
          {kind === "vlm"
            ? <Form.Item name="vlm_model" label={title + " 模型"} rules={enabled ? [{ required: true, whitespace: true, message: "请输入 VLM 模型名称" }] : []}>
                <Input disabled={locked || !enabled} maxLength={512} autoComplete="off" placeholder="例如 qwen2.5-vl-7b，可直接输入" />
              </Form.Item>
            : <Form.Item name={kind + "_model"} label={title + " 模型"}
                rules={enabled ? [{ required: true, whitespace: true, message: "请输入或选择模型" }] : []}
                extra={enabled && modelHints[kind] ? modelHints[kind] : undefined}>
                <AutoComplete
                  disabled={locked || !enabled}
                  options={models[kind].map((value) => ({ value }))}
                  placeholder={loading[kind] ? "正在获取模型列表…" : "选择或手动输入模型名称"}
                  allowClear
                  onFocus={() => { if (enabled) void loadModels(kind); }}
                  filterOption={(input, option) => String(option?.value ?? "").toLowerCase().includes(input.toLowerCase())}
                />
              </Form.Item>}
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
