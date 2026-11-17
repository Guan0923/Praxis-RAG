import { useCallback, useEffect, useRef, useState } from "react";
import { Alert, App, Button, Descriptions, Form, Input, InputNumber, Select, Switch, Tooltip } from "antd";
import { PlusOutlined, ReloadOutlined, SaveOutlined } from "@ant-design/icons";
import {
  api,
  apiResponse,
  errorText,
  fetchModelProfiles,
  knowledgeBaseModelKinds,
  modelKindLabels,
  revealModelProfileKey,
  type Chunking,
  type Connection,
  type KnowledgeBase,
  type KnowledgeBaseModelKind,
  type ModelProfile,
  type Role,
  type Strategy,
} from "./api";
import { namespacePattern } from "./navigation";

type Kind = KnowledgeBaseModelKind;
type ProviderValues = { base_url: string; apikey?: string };
type ProfileField = "embedding_profile" | "reranker_profile" | "vlm_profile";
type Values = Chunking & {
  namespace: string;
  embedding_profile: string;
  reranker_profile?: string;
  vlm_profile?: string;
  embedding_billing_source?: "platform" | "customer";
};
type TenantValues = Chunking & { namespace: string };
type Defaults = { default: string; strategies: Strategy[] };
// 创建草稿只保存 Namespace、切分参数、所选模型配置和开关，绝不保存 API Key。
type Draft = {
  namespace?: string;
  strategy?: string;
  chunk_size?: number;
  chunk_overlap?: number;
  embedding_profile?: string;
  reranker_profile?: string;
  vlm_profile?: string;
  reranker_enabled?: boolean;
  vlm_enabled?: boolean;
  // 旧版草稿没有版本号：其开关状态来自「默认关闭」时代，恢复时忽略，
  // 统一按当前的默认开启处理。
  draft_version?: number;
};
// 知识库表单只支持知识库模型类型：JEV 仅用于 Console 配置，绝不出现在这里。
const kinds: Kind[] = knowledgeBaseModelKinds;
// currentProfile 是编辑知识库时的兜底选项：沿用知识库已保存的连接。
const currentProfile = ":current";
const profileField: Record<Kind, ProfileField> = {
  embedding: "embedding_profile",
  reranker: "reranker_profile",
  vlm: "vlm_profile",
};
const cleanUrl = (value: string) => value.trim().replace(/\/+$/, "");

// matchingProfile 找出与知识库当前连接（服务地址 + 模型）完全一致的配置。
// 比较时忽略服务地址尾部的斜杠。
export function matchingProfile(
  profiles: ModelProfile[],
  kind: Kind,
  baseUrl: string,
  model: string,
): ModelProfile | undefined {
  const url = cleanUrl(baseUrl);
  return profiles.find((profile) =>
    profile.kind === kind &&
    cleanUrl(profile.base_url) === url &&
    profile.model === model);
}

// profileNeedsReembed 判断切换 Embedding 配置后是否需要重新向量化：沿用
// 「当前配置」或选择与当前连接一致的配置时不需要。
export function profileNeedsReembed(
  initial: Pick<KnowledgeBase, "embedding" | "embedding_model"> | undefined,
  selection: ModelProfile | undefined,
): boolean {
  if (!initial || !selection || selection.kind !== "embedding") return false;
  return cleanUrl(selection.base_url) !== cleanUrl(initial.embedding.base_url) ||
    selection.model !== initial.embedding_model;
}

// The create form keeps an unsaved draft (without API keys) so a failed save or
// an accidental page switch never loses the other inputs.
const draftKey = "rag.knowledge-base-draft";
// 草稿版本：2 起记录「默认全部开启」时代的显式开关状态。
const draftVersion = 2;

export function CreateKnowledgeBase({ active, disabled, onCreated, connection, role, onOpenModelProfiles }: {
  active: boolean; disabled: boolean; onCreated: (namespace: string) => void;
  connection: Connection; role?: Role; onOpenModelProfiles?: () => void;
}) {
  // Keep the form mounted after the first visit so switching pages does not
  // discard what was typed.
  const [mounted, setMounted] = useState(active);
  useEffect(() => {
    if (active) setMounted(true);
  }, [active]);
  if (!mounted) return null;
  // Admins pick saved model profiles; VIPs and ordinary users only choose the
  // namespace and chunking because the server clones the model configuration
  // from the administrator's default knowledge-base template.
  const simple = role === "vip" || role === "user";
  return <div hidden={!active}>
    {simple
      ? <TenantKnowledgeBaseForm disabled={disabled} onCreated={onCreated} connection={connection} />
      : <KnowledgeBaseForm disabled={disabled} active={active} onOpenModelProfiles={onOpenModelProfiles}
          onSaved={(item) => onCreated(item.namespace)} />}
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

  return <div className="knowledge-base-form knowledge-base-form-wide">
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
      <div className="knowledge-form-columns">
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

type ResolvedProvider = { provider: ProviderValues; model: string };

export function KnowledgeBaseForm({ initial, disabled, onSaved, onBusyChange, active, onOpenModelProfiles }: {
  initial?: KnowledgeBase; disabled: boolean; onSaved: (item: KnowledgeBase) => void;
  onBusyChange?: (busy: boolean) => void;
  // active 为 false 时页面被隐藏；重新激活时刷新模型列表，避免使用过期选项。
  active?: boolean;
  onOpenModelProfiles?: () => void;
}) {
  const { message, modal } = App.useApp();
  const [form] = Form.useForm<Values>();
  const [defaults, setDefaults] = useState<Defaults>();
  const [profiles, setProfiles] = useState<ModelProfile[]>([]);
  const [profilesLoaded, setProfilesLoaded] = useState(false);
  const [profilesLoading, setProfilesLoading] = useState(true);
  const [profilesError, setProfilesError] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  // 创建页默认开启全部可选模型（Reranker / VLM 开关默认打开，模型仍需
  // 逐个选择）；编辑页沿用知识库当前是否配置了该类模型。
  const [rerankerEnabled, setRerankerEnabled] = useState(initial ? Boolean(initial.reranker_model) : true);
  const [vlmEnabled, setVlmEnabled] = useState(initial ? Boolean(initial.vlm_model) : true);
  const lifetime = useRef(new AbortController());
  const profilesRequest = useRef<AbortController | null>(null);
  const running = useRef(false);
  const strategy = Form.useWatch("strategy", form);
  const embeddingSelection = Form.useWatch("embedding_profile", form);
  const locked = disabled || saving;
  const unit = defaults?.strategies.find((item) => item.id === strategy)?.unit === "characters" ? "字符" : "token";
  const embeddingProfile = profiles.find((profile) => profile.id === embeddingSelection);
  const reembedRequired = profileNeedsReembed(initial, embeddingProfile);

  function currentUrl(kind: Kind) {
    if (!initial) return "";
    if (kind === "embedding") return initial.embedding.base_url;
    if (kind === "reranker") return initial.reranker.base_url;
    return initial.vlm?.base_url || "";
  }

  function currentModel(kind: Kind) {
    if (!initial) return "";
    if (kind === "embedding") return initial.embedding_model;
    if (kind === "reranker") return initial.reranker_model;
    return initial.vlm_model || "";
  }

  // matchedProfile 找出与知识库当前连接（服务地址 + 模型）完全一致的配置。
  function matchedProfile(kind: Kind) {
    if (!initial) return undefined;
    return matchingProfile(profiles, kind, currentUrl(kind), currentModel(kind));
  }

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
      profilesRequest.current?.abort();
    };
  }, []);

  const loadProfiles = useCallback(async () => {
    profilesRequest.current?.abort();
    const controller = new AbortController();
    profilesRequest.current = controller;
    setProfilesLoading(true);
    setProfilesError("");
    try {
      const rows = await fetchModelProfiles(controller.signal);
      if (controller.signal.aborted) return;
      setProfiles(rows);
      setProfilesLoaded(true);
    } catch (reason) {
      if (!controller.signal.aborted) setProfilesError(errorText(reason));
    } finally {
      if (!controller.signal.aborted) setProfilesLoading(false);
    }
  }, []);
  // 创建页会保持挂载：每次重新激活都刷新模型列表，以便立刻看到刚添加的模型。
  useEffect(() => {
    if (active === false) return;
    void loadProfiles();
    return () => profilesRequest.current?.abort();
  }, [active, loadProfiles]);

  // Restore an unsaved create draft (API keys are never written to storage).
  useEffect(() => {
    if (initial) return;
    try {
      const raw = sessionStorage.getItem(draftKey);
      if (!raw) return;
      const draft = JSON.parse(raw) as Draft | null;
      if (!draft || typeof draft !== "object") return;
      form.setFieldsValue({
        namespace: draft.namespace,
        strategy: draft.strategy,
        chunk_size: draft.chunk_size,
        chunk_overlap: draft.chunk_overlap,
        embedding_profile: draft.embedding_profile,
        reranker_profile: draft.reranker_profile,
        vlm_profile: draft.vlm_profile,
      });
      if (draft.draft_version === draftVersion) {
        if (draft.reranker_enabled !== undefined) setRerankerEnabled(draft.reranker_enabled);
        if (draft.vlm_enabled !== undefined) setVlmEnabled(draft.vlm_enabled);
      }
    } catch {
      // An unreadable draft must never block the form.
    }
  }, []);

  // 模型列表到达后：编辑表单预选匹配配置；创建表单丢弃已不存在的草稿选择。
  useEffect(() => {
    if (!profilesLoaded) return;
    kinds.forEach((kind) => {
      const field = profileField[kind];
      const value = form.getFieldValue(field);
      if (initial) {
        if (value && value !== currentProfile) return;
        const match = matchedProfile(kind);
        if (match) form.setFieldValue(field, match.id);
        return;
      }
      if (value && !profiles.some((profile) => profile.id === value))
        form.setFieldValue(field, undefined);
    });
  }, [profiles, profilesLoaded]);

  // persistDraft 保存除 API Key 以外的创建输入。
  function persistDraft(overrides: Partial<Draft> = {}) {
    if (initial) return;
    try {
      const values = form.getFieldsValue(true) as Partial<Values>;
      const draft: Draft = {
        namespace: values.namespace,
        strategy: values.strategy,
        chunk_size: values.chunk_size,
        chunk_overlap: values.chunk_overlap,
        embedding_profile: values.embedding_profile,
        reranker_profile: values.reranker_profile,
        vlm_profile: values.vlm_profile,
        reranker_enabled: rerankerEnabled,
        vlm_enabled: vlmEnabled,
        draft_version: draftVersion,
        ...overrides,
      };
      sessionStorage.setItem(draftKey, JSON.stringify(draft));
    } catch {
      // Draft persistence is best effort only.
    }
  }

  // resolveSelection 把下拉框的配置 id 解析为提交所需的连接信息。当前连接
  // 兜底选项不发送 apikey，服务端保留已保存的 Key。
  async function resolveSelection(kind: Kind, profileId: string | undefined): Promise<ResolvedProvider> {
    const title = modelKindLabels[kind];
    if (!profileId) throw new Error(`请选择 ${title} 模型`);
    if (profileId === currentProfile) {
      if (!initial) throw new Error(`请选择 ${title} 模型`);
      return { provider: { base_url: currentUrl(kind) }, model: currentModel(kind) };
    }
    const profile = profiles.find((item) => item.id === profileId);
    if (!profile) throw new Error("所选模型配置已不存在，请刷新页面后重新选择");
    const base_url = cleanUrl(profile.base_url);
    // 没有保存 Key 的配置不提交 apikey：服务端在服务地址不变时保留知识库已有的
    // Key，避免把已保存的凭据清空。
    if (!profile.api_key_set) return { provider: { base_url }, model: profile.model };
    const apikey = await revealModelProfileKey(profile.id);
    return { provider: { base_url, apikey }, model: profile.model };
  }

  async function save(values: Values, reembed: boolean) {
    if (running.current) return;
    running.current = true;
    setSaving(true);
    onBusyChange?.(true);
    setError("");
    const blank: ResolvedProvider = { provider: { base_url: "", apikey: "" }, model: "" };
    try {
      // 三个模型配置的 Key 并行解析后再一次性提交。
      const [embedding, reranker, vlm] = await Promise.all([
        resolveSelection("embedding", values.embedding_profile),
        rerankerEnabled ? resolveSelection("reranker", values.reranker_profile) : Promise.resolve(blank),
        vlmEnabled ? resolveSelection("vlm", values.vlm_profile) : Promise.resolve(blank),
      ]);
      const body = {
        embedding: embedding.provider,
        embedding_model: embedding.model,
        reranker: rerankerEnabled ? reranker.provider : { base_url: "", apikey: "" },
        reranker_model: rerankerEnabled ? reranker.model : "",
        vlm: vlmEnabled ? vlm.provider : { base_url: "", apikey: "" },
        vlm_model: vlmEnabled ? vlm.model : "",
        embedding_billing_source: values.embedding_billing_source
          ?? initial?.embedding_billing_source ?? "platform",
      };
      const result = await api<KnowledgeBase>(
        "/v1/admin/knowledge-bases" + (initial ? "/" + initial.namespace : ""),
        undefined,
        {
          method: initial ? "PATCH" : "POST",
          body: JSON.stringify(initial ? { ...body, reembed } : {
            ...body,
            namespace: values.namespace,
            strategy: values.strategy,
            chunk_size: values.chunk_size,
            chunk_overlap: values.chunk_overlap,
          }),
        },
      );
      if (!lifetime.current.signal.aborted) {
        void message.success(initial ? reembed ? "重新向量化完成，配置已切换" : "知识库配置已保存" : "知识库已创建");
        if (!initial) {
          try { sessionStorage.removeItem(draftKey); } catch { /* Draft cleanup is best effort. */ }
          form.resetFields();
          // 创建成功后回到「全部模型默认开启」的初始状态。
          setRerankerEnabled(true);
          setVlmEnabled(true);
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

  function toggleKind(kind: Kind, enabled: boolean) {
    if (kind === "reranker") {
      setRerankerEnabled(enabled);
      persistDraft({ reranker_enabled: enabled });
    } else if (kind === "vlm") {
      setVlmEnabled(enabled);
      persistDraft({ vlm_enabled: enabled });
    }
  }

  // 创建知识库页面把 Embedding / Reranker / VLM 三张卡片排成自适应网格（宽屏
  // 一行三列，窄屏逐级降为一列），文本切分排成一行；编辑知识库的抽屉仍保持纵向布局。
  const providerSections = kinds.map((kind) => {
    const title = modelKindLabels[kind];
    const enabled = kind === "embedding"
      || (kind === "reranker" ? rerankerEnabled : vlmEnabled);
    const field = profileField[kind];
    const kindProfiles = profiles.filter((profile) => profile.kind === kind);
    const options = kindProfiles.map((profile) => ({
      value: profile.id,
      label: <div className="model-profile-option">
        <span className="break-text">{profile.name} · {profile.model}</span>
        {profile.base_url && <span className="secondary break-text">{profile.base_url}</span>}
      </div>,
    }));
    // 知识库当前连接不在模型列表里时保留「当前配置」选项：保存时沿用原连接，
    // 不会触发重新向量化。未启用的可选类型没有当前连接，不提供该选项。
    const showCurrent = Boolean(initial && enabled && currentModel(kind) &&
      (!profilesLoaded || !matchedProfile(kind)));
    if (showCurrent) {
      const currentLabel = `当前配置 · ${currentModel(kind)}（${currentUrl(kind)}）`;
      options.unshift({
        value: currentProfile,
        label: <span className="break-text">{currentLabel}</span>,
      });
    }
    return <section className="knowledge-provider" aria-label={title + " 配置"} key={kind}>
      <div className="section-heading"><h2>{title}</h2>{kind === "reranker" &&
        <Switch aria-label="启用 Reranker" checked={rerankerEnabled} disabled={locked}
          onChange={(checked) => toggleKind("reranker", checked)} />}
        {kind === "vlm" &&
        <Switch aria-label="启用 VLM" checked={vlmEnabled} disabled={locked}
          onChange={(checked) => toggleKind("vlm", checked)} />}</div>
      {enabled && kindProfiles.length === 0 && !profilesLoading && !profilesError &&
        <Alert className="page-alert" type="warning" showIcon title={`暂无 ${title} 模型`}
          description={initial
            ? `当前知识库使用的 ${title} 连接不在模型列表中，保存时沿用当前配置；可在「模型配置」页面添加后再切换。`
            : `创建知识库时只能选择已保存的 ${title} 模型，请先在「模型配置」页面添加。`}
          action={!initial && onOpenModelProfiles
            ? <Button type="link" size="small" onClick={onOpenModelProfiles}>前往模型配置</Button>
            : undefined} />}
      <Form.Item name={field} label={title + " 模型"}
        rules={enabled ? [{ required: true, message: `请选择 ${title} 模型` }] : []}>
        <Select
          virtual={false}
          disabled={locked || !enabled || !profilesLoaded}
          placeholder={profilesLoading
            ? "正在读取模型列表…"
            : profilesError
              ? "模型列表读取失败"
              : kindProfiles.length === 0 ? `暂无可用 ${title} 模型` : `请选择 ${title} 模型`}
          options={options}
        />
      </Form.Item>
    </section>;
  });

  return <div className={"knowledge-base-form" + (initial ? "" : " knowledge-base-form-wide")}>
    {error && <Alert type="error" showIcon className="page-alert" title="知识库配置失败" description={error}
      action={!defaults ? <Button icon={<ReloadOutlined />} aria-label="重新读取切分配置" onClick={() => void loadDefaults()} /> : undefined} />}
    {profilesError && <Alert type="error" showIcon className="page-alert" title="模型列表读取失败" description={profilesError}
      action={<Button icon={<ReloadOutlined />} aria-label="重新读取模型列表" loading={profilesLoading} onClick={() => void loadProfiles()} />} />}
    {initial && <Descriptions column={1} size="small" className="knowledge-base-summary" items={[
      { key: "namespace", label: "知识库", children: initial.namespace },
      { key: "dimension", label: "向量维度", children: initial.vector_size },
      { key: "collection", label: "索引集合", children: <span className="break-text">{initial.collection}</span> },
    ]} />}
    <Form form={form} layout="vertical" disabled={locked} scrollToFirstError onFinish={(values) => submit(values)}
      initialValues={{ namespace: initial?.namespace || "",
        embedding_profile: initial ? currentProfile : undefined,
        reranker_profile: initial && initial.reranker_model ? currentProfile : undefined,
        vlm_profile: initial && initial.vlm_model ? currentProfile : undefined,
        embedding_billing_source: initial?.embedding_billing_source || "platform",
        strategy: initial?.strategy, chunk_size: initial?.chunk_size, chunk_overlap: initial?.chunk_overlap }}
      onValuesChange={() => {
        setError("");
        persistDraft();
      }}>
      {!initial && <Form.Item className="knowledge-base-namespace" name="namespace" label="知识库标识（Namespace）" rules={[
        { required: true, message: "请输入知识库标识" }, { pattern: namespacePattern, message: "使用字母、数字、点、下划线或横线，首位为字母或数字" },
      ]}><Input maxLength={64} autoComplete="off" /></Form.Item>}
      {initial
        ? providerSections
        : <div className="knowledge-provider-grid" role="group" aria-label="模型配置">{providerSections}</div>}
      <section className="knowledge-provider" aria-label="Embedding 计费归属">
        <div className="section-heading"><h2>Embedding 计费归属</h2></div>
        <Form.Item name="embedding_billing_source" label="文档 Embedding 费用"
          extra="platform：使用平台赠额并计入会员额度；customer：客户自付连接，不扣平台赠额。仅管理员可修改。">
          <Select virtual={false} disabled={locked} options={[
            { value: "platform", label: "平台供给（默认，计入赠额）" },
            { value: "customer", label: "客户自付（不扣平台赠额）" },
          ]} />
        </Form.Item>
      </section>
      <section className="knowledge-provider" aria-label="切分配置"><h2>文本切分</h2>
        <div className="knowledge-chunk-row">
          <Form.Item name="strategy" label="切分方式" rules={[{ required: true }]}>
            <Select disabled={locked || !!initial || !defaults} virtual={false}
              options={defaults?.strategies.map((item) => ({ value: item.id, label: item.label }))}
              onChange={(id) => { const selected = defaults?.strategies.find((item) => item.id === id);
                form.setFieldsValue({ chunk_size: selected?.chunk_size, chunk_overlap: selected?.chunk_overlap });
                persistDraft(); }} />
          </Form.Item>
          <div className="chunk-controls">
            <Form.Item name="chunk_size" label={"块长度（" + unit + "）"} rules={[{ required: true, type: "integer", min: strategy === "character" ? 100 : 1, max: 100000 }]}>
              <InputNumber disabled={locked || !!initial} min={strategy === "character" ? 100 : 1} max={100000} />
            </Form.Item>
            <Form.Item name="chunk_overlap" label={"重叠长度（" + unit + "）"} dependencies={["chunk_size"]} rules={[
              { required: true, type: "integer", min: 0 }, ({ getFieldValue }) => ({ validator: (_, value) => value < getFieldValue("chunk_size") ? Promise.resolve() : Promise.reject(new Error("重叠长度必须小于块长度")) }),
            ]}><InputNumber disabled={locked || !!initial} min={0} /></Form.Item>
          </div>
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
