import { useEffect, useRef, useState } from "react";
import {
  Alert,
  App,
  Button,
  Empty,
  Form,
  InputNumber,
  Modal,
  Select,
  Table,
  Tooltip,
  Upload,
  type UploadFile,
} from "antd";
import {
  DeleteOutlined,
  FileTextOutlined,
  InboxOutlined,
  ReloadOutlined,
  UploadOutlined,
} from "@ant-design/icons";
import {
  api,
  chunkLabel,
  errorText,
  formatBytes,
  type Chunking,
  type Connection,
  type DocumentInfo,
  type Strategy,
  type UploadResult,
} from "./api";

type Props = {
  connection: Connection;
  active: boolean;
  onOpenDocument: (name: string) => void;
  uploading: boolean;
  onUploading: (busy: boolean) => void;
  onHealth: () => Promise<void>;
};

export function DocumentsPage({
  connection,
  active,
  onOpenDocument,
  uploading,
  onUploading,
  onHealth,
}: Props) {
  const { message, modal } = App.useApp();
  const [documents, setDocuments] = useState<DocumentInfo[]>([]);
  const [loading, setLoading] = useState(false);
  const [listError, setListError] = useState("");
  const [strategies, setStrategies] = useState<Strategy[]>([]);
  const [strategyError, setStrategyError] = useState("");
  const [open, setOpen] = useState(false);
  const [files, setFiles] = useState<UploadFile[]>([]);
  const [uploadError, setUploadError] = useState("");
  const [report, setReport] = useState<UploadResult>();
  const [stage, setStage] = useState("");
  const [deleting, setDeleting] = useState<string>();
  const [form] = Form.useForm<Chunking>();
  const strategyId = Form.useWatch("strategy", form);
  const unit =
    strategies.find((item) => item.id === strategyId)?.unit === "characters"
      ? "字符"
      : "token";
  const lifetime = useRef(new AbortController());
  const listRequest = useRef<AbortController | null>(null);
  const listSequence = useRef(0);
  const mutation = useRef(false);
  const confirmDialog = useRef<{ destroy: () => void } | null>(null);

  async function loadDocuments() {
    listRequest.current?.abort();
    const controller = new AbortController();
    listRequest.current = controller;
    const sequence = ++listSequence.current;
    setLoading(true);
    setListError("");
    try {
      const data = await api<{ documents: DocumentInfo[] }>(
        "/v1/documents",
        connection,
        { signal: controller.signal },
      );
      if (!lifetime.current.signal.aborted && sequence === listSequence.current)
        setDocuments(data.documents);
    } catch (error) {
      if (!controller.signal.aborted && !lifetime.current.signal.aborted) {
        setDocuments([]);
        setListError(errorText(error));
      }
    } finally {
      if (!lifetime.current.signal.aborted && sequence === listSequence.current)
        setLoading(false);
    }
  }
  async function loadStrategies() {
    setStrategyError("");
    try {
      const data = await api<{ default: string; strategies: Strategy[] }>(
        "/v1/chunking/strategies",
        connection,
        { signal: lifetime.current.signal },
      );
      if (lifetime.current.signal.aborted) return;
      setStrategies(data.strategies);
      const initial = data.strategies.find((item) => item.id === data.default);
      if (initial)
        form.setFieldsValue({
          strategy: initial.id,
          chunk_size: initial.chunk_size,
          chunk_overlap: initial.chunk_overlap,
        });
    } catch (error) {
      if (!lifetime.current.signal.aborted) setStrategyError(errorText(error));
    }
  }
  useEffect(() => {
    void loadStrategies();
    return () => {
      lifetime.current.abort();
      listRequest.current?.abort();
      confirmDialog.current?.destroy();
    };
  }, []);
  useEffect(() => {
    if (active) void loadDocuments();
  }, [active]);

  async function upload() {
    if (mutation.current || !strategies.length) return;
    mutation.current = true;
    setUploadError("");
    setReport(undefined);
    try {
      const config = await form.validateFields();
      const selected = files
        .map((file) => file.originFileObj)
        .filter((file): file is NonNullable<typeof file> => !!file);
      if (!selected.length)
        throw new Error("请选择 Markdown、TXT、PDF 或 DOCX 文件");
      if (
        selected.length > 10 ||
        selected.some((file) => !file.size || file.size > 10 * 1024 * 1024)
      )
        throw new Error("每批最多 10 个非空文件，单文件不超过 10 MiB");
      if (selected.some((file) => !/\.(md|txt|pdf|docx)$/i.test(file.name)))
        throw new Error("仅支持 Markdown、TXT、PDF 和 DOCX 文件");
      if (lifetime.current.signal.aborted) return;
      onUploading(true);
      setStage("正在检查文件");
      if (!globalThis.crypto?.subtle)
        throw new Error("文件检查需要 HTTPS 或 localhost 安全连接");
      // Preserve the existing server preflight: one digest per selected file.
      const fingerprints = [];
      for (const file of selected) {
        const digest = await crypto.subtle.digest(
          "SHA-256",
          await file.arrayBuffer(),
        );
        fingerprints.push({
          name: file.name,
          sha256: Array.from(new Uint8Array(digest), (byte) =>
            byte.toString(16).padStart(2, "0"),
          ).join(""),
        });
      }
      const options = { signal: lifetime.current.signal, method: "POST" };
      const checked = await api<{
        files: { name: string; document?: DocumentInfo }[];
      }>("/v1/documents/check", connection, {
        ...options,
        body: JSON.stringify({ files: fingerprints, ...config }),
      });
      const skipped = checked.files
        .filter((item) => item.document)
        .map((item) => ({ ...item.document!, requested_name: item.name }));
      const pending = selected.filter(
        (_, index) => !checked.files[index].document,
      );
      let result: UploadResult = { imported: [], skipped: [], failed: [] };
      if (pending.length) {
        setStage("正在生成索引");
        const body = new FormData();
        pending.forEach((file) => body.append("files", file));
        Object.entries(config).forEach(([key, value]) =>
          body.append(key, String(value)),
        );
        result = await api<UploadResult>("/v1/documents", connection, {
          ...options,
          body,
        });
      }
      if (lifetime.current.signal.aborted) return;
      const complete = {
        ...result,
        skipped: [...skipped, ...(result.skipped || [])],
      };
      setReport(complete);
      if (!complete.failed.length) setFiles([]);
      await loadDocuments();
      void onHealth();
    } catch (error) {
      if (!lifetime.current.signal.aborted)
        setUploadError(
          error && typeof error === "object" && "errorFields" in error
            ? "请检查切分参数"
            : errorText(error),
        );
    } finally {
      mutation.current = false;
      if (!lifetime.current.signal.aborted) {
        onUploading(false);
        setStage("");
      }
    }
  }

  function removeDocument(document: DocumentInfo) {
    if (mutation.current || uploading) return;
    confirmDialog.current = modal.confirm({
      title: "删除文档",
      content: (
        <span className="break-text">
          确定删除「{document.name}」及其全部向量？
        </span>
      ),
      okText: "删除",
      cancelText: "取消",
      okButtonProps: { danger: true },
      onOk: async () => {
        if (lifetime.current.signal.aborted || mutation.current) return;
        mutation.current = true;
        setDeleting(document.name);
        try {
          await api(
            `/v1/documents/${encodeURIComponent(document.name)}`,
            connection,
            { method: "DELETE", signal: lifetime.current.signal },
          );
          if (lifetime.current.signal.aborted) return;
          void message.success(`已删除 ${document.name}`);
          await loadDocuments();
        } catch (error) {
          if (!lifetime.current.signal.aborted) {
            void message.error(errorText(error));
            throw error;
          }
        } finally {
          mutation.current = false;
          if (!lifetime.current.signal.aborted) setDeleting(undefined);
        }
      },
    });
  }
  const summary = report
    ? `新增 ${report.imported.length} 个，跳过 ${report.skipped.length} 个，失败 ${report.failed.length} 个`
    : "";

  return (
    <>
      <div className="page-heading">
        <h1>知识文档</h1>
        <div className="toolbar">
          <Tooltip title="刷新文档">
            <Button
              aria-label="刷新文档"
              icon={<ReloadOutlined />}
              loading={loading}
              onClick={() => void loadDocuments()}
            />
          </Tooltip>
          <Button
            aria-label="上传文档"
            type="primary"
            icon={<UploadOutlined />}
            disabled={!!deleting}
            onClick={() => setOpen(true)}
          >
            上传文档
          </Button>
        </div>
      </div>
      <div className="section-heading">
        <h2>已索引文档</h2>
        <span className="secondary">{documents.length} 个文档</span>
      </div>
      {listError && (
        <Alert
          className="page-alert"
          type="error"
          showIcon
          title="文档读取失败"
          description={listError}
        />
      )}
      <Table<DocumentInfo>
        className="documents-table"
        rowKey="name"
        dataSource={documents}
        loading={loading}
        size="middle"
        pagination={{
          defaultPageSize: 10,
          hideOnSinglePage: true,
          showSizeChanger: false,
        }}
        locale={{
          emptyText: (
            <Empty
              image={Empty.PRESENTED_IMAGE_SIMPLE}
              description={
                listError ? "无法读取文档" : "当前工作区暂无知识文档"
              }
            />
          ),
        }}
        columns={[
          {
            title: "文档",
            key: "name",
            render: (_, doc) => (
              <div className="document-name">
                <FileTextOutlined />
                <div>
                  <button
                    type="button"
                    className="document-link"
                    onClick={() => onOpenDocument(doc.name)}
                  >
                    {doc.name}
                  </button>
                  <div className="secondary">
                    {doc.chunk_count} 个片段 · {formatBytes(doc.size)}
                    {!!doc.skipped_object_count && <div>已跳过 {doc.skipped_object_count} 个对象</div>}
                  </div>
                  <div className="mobile-document-details secondary">
                    {chunkLabel(doc.chunking, strategies)}
                    <br />
                    {doc.collection}
                  </div>
                </div>
              </div>
            ),
          },
          {
            title: "切分设置",
            key: "chunking",
            responsive: ["md"],
            render: (_, doc) => (
              <span className="break-text">
                {chunkLabel(doc.chunking, strategies)}
              </span>
            ),
          },
          {
            title: "集合",
            dataIndex: "collection",
            responsive: ["lg"],
            render: (value) => (
              <span className="secondary break-text">{value}</span>
            ),
          },
          {
            title: "操作",
            key: "actions",
            width: 64,
            render: (_, doc) => (
              <Tooltip title="删除文档">
                <Button
                  type="text"
                  danger
                  aria-label={`删除 ${doc.name}`}
                  icon={<DeleteOutlined />}
                  disabled={uploading || !!deleting}
                  loading={deleting === doc.name}
                  onClick={() => removeDocument(doc)}
                />
              </Tooltip>
            ),
          },
        ]}
      />
      <Modal
        title="上传知识文档"
        open={open}
        forceRender
        width={640}
        onCancel={() => {
          if (!uploading) setOpen(false);
        }}
        closable={!uploading}
        mask={{ closable: !uploading }}
        keyboard={!uploading}
        footer={
          <>
            <Button
              aria-label="关闭"
              disabled={uploading}
              onClick={() => setOpen(false)}
            >
              关闭
            </Button>
            <Button
              aria-label="上传并索引"
              type="primary"
              icon={<UploadOutlined />}
              loading={uploading}
              disabled={uploading || !strategies.length || !!deleting}
              onClick={() => void upload()}
            >
              {stage || "上传并索引"}
            </Button>
          </>
        }
      >
        <div className="upload-dialog">
          <Upload.Dragger
            multiple
            accept=".md,.txt,.pdf,.docx,.doc"
            disabled={uploading}
            beforeUpload={() => false}
            fileList={files}
            onChange={({ fileList }) => setFiles(fileList)}
          >
            <p className="upload-icon">
              <InboxOutlined />
            </p>
            <p>选择文档</p>
            <p className="secondary">
              MD / TXT / PDF / DOCX / DOC · 每批最多 10 个 · 单文件 10 MiB
            </p>
          </Upload.Dragger>
          {strategyError && (
            <Alert
              type="error"
              showIcon
              title="切分策略加载失败"
              description={strategyError}
              action={
                <Button size="small" onClick={() => void loadStrategies()}>
                  重试
                </Button>
              }
            />
          )}
          <Form form={form} layout="vertical" disabled={uploading}>
            <Form.Item
              name="strategy"
              label="切分策略"
              rules={[{ required: true, message: "请选择切分策略" }]}
            >
              <Select
                virtual={false}
                loading={!strategies.length && !strategyError}
                options={strategies.map((item) => ({
                  value: item.id,
                  label: item.label,
                }))}
                onChange={(id) => {
                  const item = strategies.find((item) => item.id === id)!;
                  form.setFieldsValue({
                    chunk_size: item.chunk_size,
                    chunk_overlap: item.chunk_overlap,
                  });
                }}
              />
            </Form.Item>
            <div className="chunk-controls">
              <Form.Item
                name="chunk_size"
                label={`块长度（${unit}）`}
                dependencies={["strategy"]}
                rules={[
                  {
                    required: true,
                    type: "integer",
                    min: strategyId === "character" ? 100 : 1,
                    max: 100000,
                    message: "请输入有效的整数块长度",
                  },
                ]}
              >
                <InputNumber
                  min={strategyId === "character" ? 100 : 1}
                  max={100000}
                />
              </Form.Item>
              <Form.Item
                name="chunk_overlap"
                label={`重叠上限（${unit}）`}
                dependencies={["chunk_size"]}
                rules={[
                  {
                    required: true,
                    type: "integer",
                    min: 0,
                    message: "请输入非负整数",
                  },
                  ({ getFieldValue }) => ({
                    validator: (_, value) =>
                      value >= getFieldValue("chunk_size")
                        ? Promise.reject(new Error("重叠上限必须小于块长度"))
                        : Promise.resolve(),
                  }),
                ]}
              >
                <InputNumber min={0} />
              </Form.Item>
            </div>
          </Form>
          <div aria-live="polite">
            {uploadError && (
              <Alert
                type="error"
                showIcon
                title="上传失败"
                description={uploadError}
              />
            )}
            {report && (
              <Alert
                type={report.failed.length || report.imported.some((item) => item.skipped_object_count) ? "warning" : "success"}
                showIcon
                title={summary}
                description={
                  <div className="upload-report">
                    {report.imported.map((item, index) => (
                      <div key={`import-${index}`}>{item.name}：已索引
                        {!!item.skipped_object_count && <div>已跳过 {item.skipped_object_count} 个对象</div>}
                        {item.diagnostics?.map((diagnostic, d) => <div key={d}>
                          第 {diagnostic.location?.paragraph} 段：{diagnostic.text || diagnostic.message}
                        </div>)}
                      </div>
                    ))}
                    {report.skipped.map((item, index) => (
                      <div key={`skip-${index}`}>
                        {item.requested_name || item.name}：已存在 {item.name}
                      </div>
                    ))}
                    {report.failed.map((item, index) => (
                      <div key={`fail-${index}`}>
                        {item.name}：{item.error}
                      </div>
                    ))}
                  </div>
                }
              />
            )}
          </div>
        </div>
      </Modal>
    </>
  );
}
