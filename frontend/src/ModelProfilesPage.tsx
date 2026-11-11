import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Alert, App, AutoComplete, Button, Drawer, Empty, Form, Input, Popconfirm, Select, Table,
  Tabs, Tag, Tooltip, type TableProps,
} from "antd";
import { DeleteOutlined, EditOutlined, PlusOutlined, ReloadOutlined } from "@ant-design/icons";
import {
  api,
  createModelProfile,
  deleteModelProfile,
  errorText,
  fetchModelProfileModels,
  fetchModelProfiles,
  formatDateTime,
  modelKindLabels,
  modelKinds,
  revealModelProfileKey,
  updateModelProfile,
  type ModelKind,
  type ModelProfile,
} from "./api";

type ProfileValues = {
  kind: ModelKind;
  name: string;
  base_url: string;
  model: string;
  apikey?: string;
};

const cleanUrl = (value: string) => value.trim().replace(/\/+$/, "");
const cleanText = (value: string) => value.trim();

// OCR 引擎在模型配置里保存为 model 字段；它没有服务地址与 API Key，实际推理由
// 解析服务执行，因此这里只提供固定引擎列表而不是自由输入。
const ocrEngines = [
  { value: "auto", label: "自动" },
  { value: "easyocr", label: "EasyOCR" },
  { value: "tesseract", label: "Tesseract" },
  { value: "rapidocr", label: "RapidOCR" },
];
const defaultOcrEngine = "easyocr";
const isOcrEngine = (value: unknown) => ocrEngines.some((engine) => engine.value === value);

// ModelProfilesPage 维护可复用的 Embedding / Reranker / VLM / OCR 连接：创建和
// 修改知识库时只从这里选择，页面本身不保存任何明文 Key。
export function ModelProfilesPage({ disabled, onChanged }: {
  disabled: boolean;
  onChanged?: () => void;
}) {
  const { message } = App.useApp();
  const [profiles, setProfiles] = useState<ModelProfile[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [kind, setKind] = useState<ModelKind>("embedding");
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<ModelProfile>();
  const [formError, setFormError] = useState("");
  const [saving, setSaving] = useState(false);
  const [deletingId, setDeletingId] = useState("");
  const [models, setModels] = useState<string[]>([]);
  const [modelsLoading, setModelsLoading] = useState(false);
  const [modelHint, setModelHint] = useState("");
  const [keyVisible, setKeyVisible] = useState(false);
  const [revealing, setRevealing] = useState(false);
  // clearKey 标记编辑时「保存后清除已保存的 Key」：PATCH 需要显式发送 apikey: ""。
  const [clearKey, setClearKey] = useState(false);
  const [form] = Form.useForm<ProfileValues>();
  // 表单内当前选择的模型类型：OCR 需要隐藏服务地址/Key 并换成引擎下拉框。
  const watchedKind = Form.useWatch("kind", form) as ModelKind | undefined;
  const formKind: ModelKind = watchedKind || editing?.kind || kind;
  const isOcrForm = formKind === "ocr";
  const listRequest = useRef<AbortController | null>(null);
  const modelsRequest = useRef<AbortController | null>(null);
  const keyRequest = useRef<AbortController | null>(null);
  const savingRef = useRef(false);

  const load = useCallback(async () => {
    listRequest.current?.abort();
    const controller = new AbortController();
    listRequest.current = controller;
    setLoading(true);
    setError("");
    try {
      const rows = await fetchModelProfiles(controller.signal);
      if (!controller.signal.aborted) setProfiles(rows);
    } catch (cause) {
      if (!controller.signal.aborted) setError(errorText(cause));
    } finally {
      if (!controller.signal.aborted) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
    return () => {
      listRequest.current?.abort();
      modelsRequest.current?.abort();
      keyRequest.current?.abort();
    };
  }, [load]);

  const counts = useMemo(() => {
    const result: Record<ModelKind, number> = { embedding: 0, reranker: 0, vlm: 0, ocr: 0 };
    for (const profile of profiles) {
      if (profile.kind in result) result[profile.kind] += 1;
    }
    return result;
  }, [profiles]);
  const visible = profiles.filter((profile) => profile.kind === kind);

  function resetDrawer() {
    modelsRequest.current?.abort();
    keyRequest.current?.abort();
    setFormError("");
    setModels([]);
    setModelsLoading(false);
    setModelHint("");
    setKeyVisible(false);
    setRevealing(false);
    setClearKey(false);
  }

  function openCreate() {
    resetDrawer();
    setEditing(undefined);
    setOpen(true);
  }

  function openEdit(profile: ModelProfile) {
    resetDrawer();
    setEditing(profile);
    setOpen(true);
  }

  function close() {
    if (saving) return;
    modelsRequest.current?.abort();
    keyRequest.current?.abort();
    setClearKey(false);
    setOpen(false);
  }

  async function save(values: ProfileValues) {
    if (savingRef.current) return;
    savingRef.current = true;
    setSaving(true);
    setFormError("");
    const ocr = values.kind === "ocr";
    const payload = {
      name: cleanText(values.name),
      // OCR 不访问远端服务：base_url 与 apikey 固定提交空串。
      base_url: ocr ? "" : cleanUrl(values.base_url || ""),
      model: cleanText(values.model),
      apikey: ocr ? "" : values.apikey?.trim() || "",
    };
    try {
      if (editing) {
        const { apikey, ...rest } = payload;
        // 留空表示沿用服务端已保存的 Key；标记清除时必须显式发送空字符串。
        // OCR 没有 Key，PATCH 时同样显式发送空串。
        const update = ocr || clearKey
          ? { ...rest, apikey: "" }
          : apikey ? { ...rest, apikey } : rest;
        await updateModelProfile(editing.id, update);
        void message.success("模型配置已更新");
      } else {
        await createModelProfile({ kind: values.kind, ...payload });
        void message.success("模型配置已创建");
      }
    } catch (cause) {
      setFormError(errorText(cause));
      return;
    } finally {
      savingRef.current = false;
      setSaving(false);
    }
    setClearKey(false);
    setOpen(false);
    onChanged?.();
    void load();
  }

  async function remove(profile: ModelProfile) {
    setDeletingId(profile.id);
    try {
      await deleteModelProfile(profile.id);
      setProfiles((current) => current.filter((item) => item.id !== profile.id));
      void message.success("模型配置已删除");
      onChanged?.();
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setDeletingId("");
    }
  }

  async function loadModels() {
    const values = form.getFieldsValue();
    // OCR 引擎是固定列表，不存在远端 /models 探测。
    if (values.kind === "ocr") return;
    const baseUrl = cleanUrl(values.base_url || "");
    if (!baseUrl) {
      setModelHint("请先填写服务地址，再获取模型列表");
      return;
    }
    modelsRequest.current?.abort();
    const controller = new AbortController();
    modelsRequest.current = controller;
    setModelsLoading(true);
    setModelHint("");
    try {
      // 已保存且地址、Key 都没改时复用服务端保存的 Key；否则按未保存配置调用
      // 原有的 provider 模型列表接口。
      const reuseSaved = Boolean(
        editing?.api_key_set &&
        !values.apikey?.trim() &&
        baseUrl === cleanUrl(editing.base_url),
      );
      const rows = reuseSaved
        ? await fetchModelProfileModels(editing!.id, controller.signal)
        : await api<{ models: string[] }>(
            "/v1/admin/knowledge-bases/models/" + values.kind,
            undefined,
            {
              method: "POST",
              signal: controller.signal,
              body: JSON.stringify({ base_url: baseUrl, apikey: values.apikey?.trim() || "" }),
            },
          ).then((payload) => payload.models || []);
      if (controller.signal.aborted) return;
      setModels(rows);
      setModelHint(rows.length ? "" : "该地址未返回模型列表，可直接输入模型名称");
    } catch {
      // 部分 provider 没有 /models 接口：模型列表只是便利功能，仍允许手动输入。
      if (!controller.signal.aborted) {
        setModelHint("无法从该地址获取模型列表，可直接输入模型名称");
      }
    } finally {
      if (!controller.signal.aborted) setModelsLoading(false);
    }
  }

  async function showKey(visible: boolean) {
    // 新建配置没有已保存的 Key；编辑时首次点开才向服务端读取。已标记清除时
    // 不再回读已保存的 Key。OCR 没有 Key，任何情况下都不读取。
    if (editing?.kind === "ocr") return;
    if (!visible || !editing?.api_key_set || form.getFieldValue("apikey") || clearKey) {
      setKeyVisible(visible);
      return;
    }
    keyRequest.current?.abort();
    const controller = new AbortController();
    keyRequest.current = controller;
    setRevealing(true);
    try {
      const apikey = await revealModelProfileKey(editing.id, controller.signal);
      if (controller.signal.aborted) return;
      form.setFieldValue("apikey", apikey);
      setKeyVisible(true);
    } catch (cause) {
      if (!controller.signal.aborted) void message.error(errorText(cause));
    } finally {
      if (!controller.signal.aborted) setRevealing(false);
    }
  }

  // markKeyForRemoval 清空密码框并标记「保存后清除已保存的 Key」。
  function markKeyForRemoval() {
    keyRequest.current?.abort();
    form.setFieldValue("apikey", "");
    setKeyVisible(false);
    setRevealing(false);
    setClearKey(true);
    setFormError("");
  }

  const columns: TableProps<ModelProfile>["columns"] = [
    {
      title: "名称",
      dataIndex: "name",
      width: 180,
      render: (name: string) => <span className="break-text">{name}</span>,
    },
    {
      title: kind === "ocr" ? "OCR 引擎" : "模型",
      dataIndex: "model",
      width: 200,
      render: (model: string) => <span className="break-text">{model}</span>,
    },
    ...(kind === "ocr" ? [] : [
      {
        title: "服务地址",
        dataIndex: "base_url",
        render: (url: string) => <span className="break-text">{url}</span>,
      },
      {
        title: "Key",
        dataIndex: "api_key_set",
        width: 90,
        render: (saved: boolean) => saved ? <Tag color="green">已保存</Tag> : <Tag>未设置</Tag>,
      },
    ]),
    ...(kind === "embedding"
      ? [{
          title: "向量维度",
          dataIndex: "vector_size",
          width: 100,
          render: (value: ModelProfile["vector_size"]) => value ?? "—",
        }]
      : []),
    {
      title: "更新时间",
      dataIndex: "updated_at",
      width: 180,
      render: (value: string | undefined, profile: ModelProfile) =>
        formatDateTime(value || profile.created_at),
    },
    {
      title: "操作",
      width: 100,
      render: (_: unknown, profile: ModelProfile) => (
        <div className="toolbar">
          <Tooltip title="编辑模型配置">
            <Button
              type="text"
              size="small"
              aria-label={"编辑 " + profile.name}
              icon={<EditOutlined />}
              disabled={disabled || saving || deletingId === profile.id}
              onClick={() => openEdit(profile)}
            />
          </Tooltip>
          <Popconfirm
            title="删除模型配置"
            description={`确定删除 ${profile.name} 吗？已保存连接的知识库不受影响，但创建或修改知识库时将无法再选择该配置。`}
            okText="删除"
            cancelText="取消"
            okButtonProps={{ danger: true }}
            disabled={disabled || saving || deletingId === profile.id}
            onConfirm={() => void remove(profile)}
          >
            <Tooltip title="删除模型配置">
              <Button
                type="text"
                size="small"
                danger
                aria-label={"删除 " + profile.name}
                icon={<DeleteOutlined />}
                loading={deletingId === profile.id}
                disabled={disabled || saving}
              />
            </Tooltip>
          </Popconfirm>
        </div>
      ),
    },
  ];

  const emptyText = loading ? undefined : profiles.length === 0
    ? (
      <Empty description="还没有模型配置，请先添加 Embedding / Reranker / VLM / OCR 模型；创建知识库时只能从该列表选择">
        <Button type="primary" icon={<PlusOutlined />} disabled={disabled} onClick={openCreate}>
          添加模型
        </Button>
      </Empty>
    )
    : <Empty description={`还没有 ${modelKindLabels[kind]} 模型，点击「添加模型」创建`} />;

  return <section aria-label="模型配置页面">
    <div className="page-heading">
      <h1>模型配置</h1>
      <div className="toolbar">
        <Tooltip title="刷新模型配置">
          <Button
            aria-label="刷新模型配置"
            icon={<ReloadOutlined spin={loading} />}
            disabled={saving}
            onClick={() => void load()}
          />
        </Tooltip>
        <Button type="primary" icon={<PlusOutlined />} disabled={disabled || saving} onClick={openCreate}>
          添加模型
        </Button>
      </div>
    </div>
    <Alert
      className="page-alert"
      type="info"
      showIcon
      title="模型列表说明"
      description="创建或修改知识库时只能从该列表选择 Embedding / Reranker / VLM / OCR 模型；API Key 仅保存在服务端，表格只显示是否已设置。OCR 配置只需选择引擎，无需服务地址与 Key。"
    />
    {error && (
      <Alert
        className="page-alert"
        type="error"
        showIcon
        title="模型配置读取失败"
        description={error}
        action={<Button icon={<ReloadOutlined />} aria-label="重新读取模型配置" loading={loading} onClick={() => void load()} />}
      />
    )}
    <Tabs
      activeKey={kind}
      onChange={(key) => setKind(key as ModelKind)}
      items={modelKinds.map((value) => ({
        key: value,
        label: `${modelKindLabels[value]}（${counts[value]}）`,
      }))}
    />
    <Table<ModelProfile>
      rowKey="id"
      dataSource={visible}
      loading={loading}
      scroll={{ x: 820 }}
      locale={{ emptyText }}
      pagination={{ pageSize: 10, hideOnSinglePage: true }}
      columns={columns}
    />
    <Drawer
      title={editing ? "编辑模型配置" : "添加模型"}
      open={open}
      size={640}
      closable={!saving}
      maskClosable={!saving}
      keyboard={!saving}
      destroyOnHidden
      onClose={close}
      footer={<div className="toolbar" style={{ justifyContent: "flex-end" }}>
        <Button onClick={close} disabled={saving}>取消</Button>
        <Button type="primary" loading={saving} onClick={() => form.submit()}>保存</Button>
      </div>}
    >
      {formError && (
        <Alert className="page-alert" type="error" showIcon title="保存失败" description={formError} />
      )}
      <Form<ProfileValues>
        key={editing?.id ?? "new"}
        form={form}
        layout="vertical"
        disabled={saving}
        scrollToFirstError
        initialValues={editing ? {
          kind: editing.kind,
          name: editing.name,
          base_url: editing.base_url,
          model: editing.model,
          apikey: "",
        } : { kind, model: kind === "ocr" ? defaultOcrEngine : "" }}
        onFinish={(values) => void save(values)}
        onValuesChange={(changed) => {
          setFormError("");
          // 重新输入 Key 会取消「保存后清除」标记，新 Key 优先生效。
          if ("apikey" in changed && changed.apikey) setClearKey(false);
          if ("kind" in changed) {
            const next = changed.kind as ModelKind;
            const model = form.getFieldValue("model");
            // 切到 OCR 默认选择 EasyOCR；切回远端模型时清掉残留的引擎 id。
            if (next === "ocr" && !isOcrEngine(model))
              form.setFieldsValue({ model: defaultOcrEngine });
            if (next !== "ocr" && isOcrEngine(model)) form.setFieldsValue({ model: "" });
          }
          if ("kind" in changed || "base_url" in changed || "apikey" in changed) {
            modelsRequest.current?.abort();
            setModels([]);
            setModelsLoading(false);
            setModelHint("");
          }
        }}
      >
        <Form.Item name="kind" label="模型类型" rules={[{ required: true, message: "请选择模型类型" }]}>
          <Select
            virtual={false}
            disabled={Boolean(editing)}
            options={modelKinds.map((value) => ({ value, label: modelKindLabels[value] }))}
          />
        </Form.Item>
        <Form.Item
          name="name"
          label="配置名称"
          rules={[
            { required: true, whitespace: true, message: "请输入配置名称" },
            { max: 80, message: "配置名称最多 80 个字符" },
          ]}
        >
          <Input maxLength={80} autoComplete="off" placeholder="例如 生产环境 Embedding" />
        </Form.Item>
        {!isOcrForm && (
          <Form.Item
            name="base_url"
            label="服务地址"
            rules={[
              { required: true, whitespace: true, message: "请输入服务地址" },
              { pattern: /^https?:\/\/\S+$/i, message: "服务地址需以 http:// 或 https:// 开头" },
            ]}
          >
            <Input maxLength={2048} autoComplete="off" placeholder="https://api.example.com/v1" />
          </Form.Item>
        )}
        {!isOcrForm && (
          <div className="model-key-row">
            <Form.Item
              name="apikey"
              label="API Key"
              extra={clearKey
                ? "保存后将清除已保存的 Key"
                : editing?.api_key_set ? "已保存 Key，留空保持不变" : "无鉴权服务可留空"}
            >
              <Input.Password
                autoComplete="new-password"
                disabled={revealing}
                placeholder={clearKey
                  ? "保存后清除已保存的 Key"
                  : editing?.api_key_set ? "已保存（留空保持不变）" : "可留空"}
                visibilityToggle={{ visible: keyVisible, onVisibleChange: (next) => void showKey(next) }}
              />
            </Form.Item>
            {editing?.api_key_set && (
              <Tooltip title={clearKey ? "保存后将清除已保存的 Key" : "清除已保存的 Key"}>
                <Button
                  aria-label="清除已保存的 Key"
                  icon={<DeleteOutlined />}
                  disabled={saving || revealing || clearKey}
                  onClick={markKeyForRemoval}
                />
              </Tooltip>
            )}
          </div>
        )}
        {isOcrForm ? (
          <Form.Item
            name="model"
            label="OCR 引擎"
            rules={[{ required: true, message: "请选择 OCR 引擎" }]}
            extra="OCR 由解析服务在本地执行，无需服务地址与 API Key"
          >
            <Select virtual={false} options={ocrEngines} placeholder="请选择 OCR 引擎" />
          </Form.Item>
        ) : (
          <div className="model-key-row">
            <Form.Item
              name="model"
              label="模型名称"
              rules={[{ required: true, whitespace: true, message: "请输入或获取模型名称" }]}
              extra={modelHint || undefined}
            >
              <AutoComplete
                options={models.map((value) => ({ value }))}
                placeholder={modelsLoading ? "正在获取模型列表…" : "输入或从模型列表中选取"}
                allowClear
                filterOption={(input, option) =>
                  String(option?.value ?? "").toLowerCase().includes(input.toLowerCase())}
              />
            </Form.Item>
            <Button
              icon={<ReloadOutlined />}
              loading={modelsLoading}
              disabled={saving}
              onClick={() => void loadModels()}
            >
              获取模型列表
            </Button>
          </div>
        )}
      </Form>
    </Drawer>
  </section>;
}
