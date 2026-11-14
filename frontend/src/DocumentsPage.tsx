import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import {
  Alert,
  App,
  Button,
  Empty,
  Modal,
  Popover,
  Progress,
  Table,
  Tag,
  Tooltip,
  Upload,
  type TableColumnsType,
  type UploadFile,
} from "antd";
import {
  CheckCircleOutlined,
  CloseOutlined,
  DeleteOutlined,
  FileTextOutlined,
  InboxOutlined,
  LoadingOutlined,
  ReloadOutlined,
  UploadOutlined,
} from "@ant-design/icons";
import {
  api,
  chunkLabel,
  errorText,
  fetchAccess,
  formatBytes,
  type AccessView,
  type Connection,
  type DocumentInfo,
  type KnowledgeBase,
  type Strategy,
} from "./api";
import {
  bindNamespace,
  canRetryItem,
  clearFinished,
  getSnapshot,
  isFailedItem,
  isUploadSettled,
  itemStatusText,
  perFileProgress,
  restore,
  retryFailedItem,
  setOnSettled,
  subscribe,
  uploadFiles,
  uploadSummary,
} from "./uploadBatches";

type Props = {
  connection: Connection;
  active: boolean;
  authEnabled: boolean;
  onOpenDocument: (name: string) => void;
  uploading: boolean;
  onUploading: (busy: boolean) => void;
  onHealth: () => Promise<void>;
};

export function DocumentsPage({
  connection,
  active,
  authEnabled,
  onOpenDocument,
  uploading,
  onUploading,
  onHealth,
}: Props) {
  const { message, modal } = App.useApp();
  const [documents, setDocuments] = useState<DocumentInfo[]>([]);
  const [loading, setLoading] = useState(false);
  const [listError, setListError] = useState("");
  const [access, setAccess] = useState<AccessView>();
  const [knowledgeBase, setKnowledgeBase] = useState<KnowledgeBase>();
  const [strategies, setStrategies] = useState<Strategy[]>([]);
  const [settingsError, setSettingsError] = useState("");
  const [open, setOpen] = useState(false);
  const [files, setFiles] = useState<UploadFile[]>([]);
  const [uploadError, setUploadError] = useState("");
  const [bubbleOpen, setBubbleOpen] = useState(false);
  const [deleting, setDeleting] = useState<string>();
  const lifetime = useRef(new AbortController());
  const listRequest = useRef<AbortController | null>(null);
  const listSequence = useRef(0);
  const mutation = useRef(false);
  const confirmDialog = useRef<{ destroy: () => void } | null>(null);
  const restoredBatches = useRef(false);
  const batches = useSyncExternalStore(subscribe, getSnapshot);
  const busy = uploading || batches.unfinishedCount > 0;

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
  // loadAccess resolves the caller's effective scope so read-only users never
  // see upload/delete controls. Legacy API-key mode has no console session, so
  // it keeps the historical controls and the server decides.
  async function loadAccess() {
    if (!authEnabled) return;
    try {
      const data = await fetchAccess(connection.namespace, connection);
      if (!lifetime.current.signal.aborted) setAccess(data);
    } catch {
      // A failed probe must not break document listing; the server still
      // enforces write permissions on every mutation.
    }
  }
  useEffect(() => {
    void loadKnowledgeBase();
    void loadAccess();
    return () => {
      lifetime.current.abort();
      listRequest.current?.abort();
      confirmDialog.current?.destroy();
    };
  }, []);
  useEffect(() => {
    if (active) void loadDocuments();
  }, [active]);
  // Restore unfinished batches once on mount (after a page reload); leaving
  // the page never cancels an upload because the parent keeps this component
  // mounted.
  useEffect(() => {
    // A workspace switch remounts this page: drop the previous workspace's
    // finished rows before restoring the new workspace's pending batches.
    bindNamespace(connection.namespace);
    if (restoredBatches.current) return;
    restoredBatches.current = true;
    void restore(connection);
  }, [connection]);
  // Batch polling lives outside React, so the settled callback refreshes the
  // document list and health summary once every tracked batch finishes.
  useEffect(() => {
    setOnSettled(() => {
      void loadDocuments();
      void onHealth();
    });
    return () => setOnSettled(undefined);
  }, [connection]);
  // The Console locks workspace switching (not page navigation) while a batch
  // is unfinished; this keeps its prop in sync with the upload store.
  useEffect(() => {
    onUploading(batches.unfinishedCount > 0);
  }, [batches.unfinishedCount, onUploading]);
  useEffect(() => () => onUploading(false), [onUploading]);
  useEffect(() => {
    if (!batches.batches.length) setBubbleOpen(false);
  }, [batches.batches.length]);

  function openUploadModal() {
    setFiles([]);
    setUploadError("");
    setOpen(true);
  }

  function closeUploadModal() {
    setOpen(false);
    setFiles([]);
    setUploadError("");
  }

  function startUpload() {
    if (!knowledgeBase || mutation.current) return;
    const selected = files
      .map((file) => file.originFileObj)
      .filter((file): file is NonNullable<typeof file> => !!file);
    if (!selected.length) {
      setUploadError("请选择 Markdown、TXT、PDF、DOCX 或 DOC 文件");
      return;
    }
    if (selected.some((file) => file.size > 10 * 1024 * 1024)) {
      setUploadError("单文件不超过 10 MiB");
      return;
    }
    if (selected.some((file) => !/\.(md|txt|pdf|docx|doc)$/i.test(file.name))) {
      setUploadError("仅支持 Markdown、TXT、PDF、DOCX 和 DOC 文件");
      return;
    }
    setUploadError("");
    // Hand the files to the background store immediately: the modal closes and
    // its selection never comes back, progress lives in the toolbar chip.
    closeUploadModal();
    void uploadFiles(selected, connection);
  }

  async function retryAllFailed() {
    const failed = batches.failedItems.filter((item) => canRetryItem(item));
    for (const item of failed) {
      try {
        await retryFailedItem(item, connection);
      } catch {
        // A restored row has no local file; the next retryable row still runs.
      }
    }
  }

  function removeDocument(document: DocumentInfo) {
    if (mutation.current || busy) return;
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
  // uploadSummary derives the chip label and failure colour from the store;
  // null means no batch has ever been started in this tab.
  const summary = uploadSummary(batches);
  const retryableFailed = batches.failedItems.filter((item) => canRetryItem(item));
  const hasFinished = batches.batches.some((batch) => isUploadSettled(batch));
  const uploadBubble = (
    <div className="upload-bubble">
      <div className="upload-bubble-heading">
        <span className="upload-bubble-title">
          保存 {summary?.settled ?? 0}/{summary?.total ?? 0}
        </span>
        <Button
          type="text"
          size="small"
          aria-label="关闭上传进度"
          icon={<CloseOutlined />}
          onClick={() => setBubbleOpen(false)}
        />
      </div>
      {batches.batches.length ? (
        <>
          <div className="upload-bubble-list">
            {batches.batches.flatMap((batch) =>
              batch.items.map((item) => {
                const failed = isFailedItem(item);
                return (
                  <div
                    className="upload-bubble-item"
                    key={`${batch.id}:${item.position}`}
                  >
                    <div className="upload-bubble-details">
                      <div className="upload-bubble-name break-text">
                        {item.name}
                      </div>
                      <Progress
                        className="upload-bubble-progress"
                        size="small"
                        percent={perFileProgress(item)}
                        status={
                          failed
                            ? "exception"
                            : item.status === "saving"
                              ? "active"
                              : "success"
                        }
                        showInfo
                      />
                      <div
                        className={`upload-bubble-state${failed ? " upload-bubble-state-failed" : ""}`}
                      >
                        {itemStatusText(item)}
                      </div>
                    </div>
                    {failed && canRetryItem(item) && (
                      <Button
                        type="link"
                        size="small"
                        className="upload-bubble-retry"
                        onClick={() => void retryFailedItem(item, connection)}
                      >
                        重试
                      </Button>
                    )}
                  </div>
                );
              }),
            )}
          </div>
          {(batches.failedItems.length > 0 || hasFinished) && (
            <div className="upload-bubble-actions">
              {batches.failedItems.length > 0 && (
                <Button
                  size="small"
                  disabled={!retryableFailed.length}
                  onClick={() => void retryAllFailed()}
                >
                  重试失败项
                </Button>
              )}
              {hasFinished && (
                <Button size="small" onClick={clearFinished}>
                  清空已完成
                </Button>
              )}
            </div>
          )}
        </>
      ) : (
        <div className="upload-bubble-empty secondary">没有进行中的上传</div>
      )}
    </div>
  );

  // Only whole-knowledge-base writers get upload/delete controls. File-level
  // writers can still replace authorized documents through the API, but the
  // Console keeps them read-only (matching the server-side restriction).
  const readOnly = Boolean(authEnabled && access && (!access.can_write || !access.scope.whole_write));
  const documentColumns: TableColumnsType<DocumentInfo> = [
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
  ];
  if (!readOnly) {
    documentColumns.push({
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
            disabled={busy || !!deleting}
            loading={deleting === doc.name}
            onClick={() => removeDocument(doc)}
          />
        </Tooltip>
      ),
    });
  }

  return (
    <>
      <div className="page-heading">
        <div className="page-heading-title">
          <h1>知识文档</h1>
          {readOnly && <Tag color="orange">只读</Tag>}
        </div>
        <div className="toolbar">
          <Tooltip title="刷新文档">
            <Button
              aria-label="刷新文档"
              icon={<ReloadOutlined />}
              loading={loading}
              onClick={() => void loadDocuments()}
            />
          </Tooltip>
          {!readOnly && (summary ? (
            <Popover
              trigger="click"
              placement="bottomRight"
              open={bubbleOpen}
              onOpenChange={setBubbleOpen}
              content={uploadBubble}
            >
              <Button
                size="small"
                className={`upload-chip${summary.hasFailure ? " upload-chip-warning" : ""}`}
                aria-label="上传进度"
                icon={
                  summary.phase === "completed"
                    ? <CheckCircleOutlined />
                    : <LoadingOutlined spin />
                }
              >
                {summary.label}
              </Button>
            </Popover>
          ) : (
            <Button
              aria-label="上传文档"
              type="primary"
              icon={<UploadOutlined />}
              disabled={!!deleting}
              onClick={openUploadModal}
            >
              上传文档
            </Button>
          ))}
        </div>
      </div>
      {readOnly && (
        <Alert
          className="page-alert"
          type="info"
          showIcon
          title="只读知识库"
          description="你对该知识库没有整体写入权限，上传与删除入口已隐藏；文件列表由服务端按授权范围过滤。"
        />
      )}
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
        size="small"
        pagination={{
          defaultPageSize: 10,
          hideOnSinglePage: true,
          showSizeChanger: false,
          size: "small",
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
        columns={documentColumns}
      />
      <Modal
        title="上传知识文档"
        className="knowledge-upload-modal"
        open={open}
        forceRender
        width={640}
        onCancel={closeUploadModal}
        footer={
          <>
            <Button aria-label="关闭" onClick={closeUploadModal}>
              关闭
            </Button>
            <Button
              aria-label="上传并索引"
              type="primary"
              icon={<UploadOutlined />}
              disabled={!knowledgeBase || !files.length || !!deleting}
              onClick={startUpload}
            >
              上传并索引
            </Button>
          </>
        }
      >
        <div className="upload-dialog">
          <div className="upload-selection">
          <Upload.Dragger
            multiple
            accept=".md,.txt,.pdf,.docx,.doc"
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
              MD / TXT / PDF / DOCX / DOC · 单文件 10 MiB · 超过 10 个自动分批
            </p>
          </Upload.Dragger>
          {files.length > 0 && (
            <section className="upload-selected-files" aria-label="待上传文件">
              <div className="upload-files-heading">
                <span>待上传文件 <span className="secondary">{files.length} 个</span></span>
                <Button type="text" size="small" onClick={() => setFiles([])}>清空</Button>
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
                      <Button type="text" danger aria-label={`移除 ${file.name}`} icon={<DeleteOutlined />}
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
          </div>
        </div>
      </Modal>
    </>
  );
}
