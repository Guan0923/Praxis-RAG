import { useEffect, useRef, useState } from "react";
import {
  Alert,
  App,
  Button,
  Empty,
  Modal,
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
  type KnowledgeBase,
  type Strategy,
  type Connection,
  type DocumentInfo,
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
  const [knowledgeBase, setKnowledgeBase] = useState<KnowledgeBase>();
  const [strategies, setStrategies] = useState<Strategy[]>([]);
  const [settingsError, setSettingsError] = useState("");
  const [open, setOpen] = useState(false);
  const [files, setFiles] = useState<UploadFile[]>([]);
  const [uploadError, setUploadError] = useState("");
  const [report, setReport] = useState<UploadResult>();
  const [stage, setStage] = useState("");
  const [deleting, setDeleting] = useState<string>();
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
  async function loadKnowledgeBase() {
    setSettingsError("");
    try {
      const data = await api<KnowledgeBase & { strategies: Strategy[] }>(
        "/v1/knowledge-base",
        connection,
        { signal: lifetime.current.signal },
      );
      if (lifetime.current.signal.aborted) return;
      setKnowledgeBase(data);
      setStrategies(data.strategies);
    } catch (error) {
      if (!lifetime.current.signal.aborted) setSettingsError(errorText(error));
    }
  }
  useEffect(() => {
    void loadKnowledgeBase();
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
    if (mutation.current || !knowledgeBase) return;
    mutation.current = true;
    setUploadError("");
    setReport(undefined);
    try {
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
        body: JSON.stringify({ files: fingerprints }),
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
        setUploadError(errorText(error));
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
        className="knowledge-upload-modal"
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
              disabled={uploading || !knowledgeBase || !files.length || !!deleting}
              onClick={() => void upload()}
            >
              {stage || "上传并索引"}
            </Button>
          </>
        }
      >
        <div className="upload-dialog">
          <div className="upload-selection">
          <Upload.Dragger
            multiple
            accept=".md,.txt,.pdf,.docx,.doc"
            disabled={uploading}
            beforeUpload={() => false}
            fileList={files}
            showUploadList={false}
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
          {files.length > 0 && (
            <section className="upload-selected-files" aria-label="待上传文件">
              <div className="upload-files-heading">
                <span>待上传文件 <span className="secondary">{files.length} / 10</span></span>
                <Button type="text" size="small" disabled={uploading} onClick={() => setFiles([])}>清空</Button>
              </div>
              <ul className="upload-file-list">
                {files.map((file) => (
                  <li className="upload-file-row" key={file.uid}>
                    <FileTextOutlined className="upload-file-icon" />
                    <div className="upload-file-details">
                      <div className="upload-file-name">{file.name}</div>
                      <div className="secondary">{formatBytes(file.size ?? 0)}</div>
                    </div>
                    <Tooltip title="移除文件">
                      <Button type="text" danger aria-label={`移除 ${file.name}`} icon={<DeleteOutlined />} disabled={uploading}
                        onClick={() => setFiles((current) => current.filter((item) => item.uid !== file.uid))} />
                    </Tooltip>
                  </li>
                ))}
              </ul>
            </section>
          )}
          </div>
          {settingsError && (
            <Alert type="error" showIcon title="知识库配置未就绪" description={settingsError}
              action={<Button size="small" onClick={() => void loadKnowledgeBase()}>重试</Button>} />
          )}
          {knowledgeBase && (
            <div className="upload-settings-summary">
              <span>{chunkLabel(knowledgeBase, strategies)}</span>
              <span>向量维度 {knowledgeBase.vector_size}</span>
            </div>
          )}
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
