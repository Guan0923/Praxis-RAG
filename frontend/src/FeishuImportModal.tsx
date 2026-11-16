import { useEffect, useMemo, useRef, useState } from "react";
import {
  Alert,
  App,
  Breadcrumb,
  Button,
  Checkbox,
  Empty,
  Input,
  Modal,
  Popconfirm,
  Space,
  Spin,
  Table,
  Tabs,
  Tag,
  Tooltip,
  type TableColumnsType,
} from "antd";
import {
  CloudDownloadOutlined,
  FileTextOutlined,
  FolderOpenOutlined,
  LinkOutlined,
  ReloadOutlined,
  SearchOutlined,
} from "@ant-design/icons";
import {
  errorText,
  formatDateTime,
  type Connection,
  type DocumentBatchJob,
  type KnowledgeBase,
} from "./api";
import {
  disconnectFeishu,
  feishuImportReason,
  feishuImportStatusColors,
  feishuImportStatusLabels,
  feishuJobLabel,
  fetchFeishuStatus,
  importFeishuDocuments,
  listFeishuFiles,
  refreshFeishuImportJobs,
  searchFeishuDocuments,
  startFeishuOAuth,
  type FeishuFile,
  type FeishuImportDocument,
  type FeishuImportResult,
  type FeishuSearchItem,
  type FeishuStatus,
} from "./feishu";

const maxImportDocuments = 10;
const pageSize = 50;
const searchPageSize = 20;

type Folder = { token: string; name: string };

type Props = {
  open: boolean;
  connection: Connection;
  knowledgeBase?: KnowledgeBase;
  canWrite: boolean;
  onClose: () => void;
  onImported: () => void;
};

// FeishuImportModal connects one Console account to Feishu, browses the
// account's own folders/search results and imports selected documents through
// the ordinary asynchronous upload path.
export function FeishuImportModal({
  open,
  connection,
  knowledgeBase,
  canWrite,
  onClose,
  onImported,
}: Props) {
  const { message } = App.useApp();
  const [status, setStatus] = useState<FeishuStatus>();
  const [statusError, setStatusError] = useState("");
  const [statusLoading, setStatusLoading] = useState(false);
  const [connecting, setConnecting] = useState(false);
  const [activeTab, setActiveTab] = useState("browse");

  const [folderPath, setFolderPath] = useState<Folder[]>([]);
  const [files, setFiles] = useState<FeishuFile[]>([]);
  const [filesError, setFilesError] = useState("");
  const [filesLoading, setFilesLoading] = useState(false);
  const [nextPageToken, setNextPageToken] = useState<string>();
  const [loadingMore, setLoadingMore] = useState(false);

  const [keyword, setKeyword] = useState("");
  const [searchQuery, setSearchQuery] = useState("");
  const [searchItems, setSearchItems] = useState<FeishuSearchItem[]>([]);
  const [searchError, setSearchError] = useState("");
  const [searchLoading, setSearchLoading] = useState(false);
  const [searchHasMore, setSearchHasMore] = useState(false);

  const [selected, setSelected] = useState<Map<string, FeishuImportDocument>>(
    new Map(),
  );
  const [importing, setImporting] = useState(false);
  const [importError, setImportError] = useState("");
  const [result, setResult] = useState<FeishuImportResult>();
  const [jobs, setJobs] = useState<Map<string, DocumentBatchJob>>(new Map());
  const lifetime = useRef(new AbortController());
  const folderRequest = useRef(0);
  const searchRequest = useRef(0);

  const namespace = knowledgeBase?.namespace || connection.namespace;
  const selectedList = useMemo(
    () => Array.from(selected.values()),
    [selected],
  );

  function resetWorkspaceState() {
    folderRequest.current += 1;
    searchRequest.current += 1;
    setStatusError("");
    setStatusLoading(false);
    setConnecting(false);
    setActiveTab("browse");
    setFolderPath([]);
    setFiles([]);
    setFilesError("");
    setFilesLoading(false);
    setNextPageToken(undefined);
    setLoadingMore(false);
    setKeyword("");
    setSearchQuery("");
    setSearchItems([]);
    setSearchError("");
    setSearchLoading(false);
    setSearchHasMore(false);
    setSelected(new Map());
    setImporting(false);
    setImportError("");
    setResult(undefined);
    setJobs(new Map());
  }

  useEffect(() => {
    if (!open) return;
    resetWorkspaceState();
    lifetime.current.abort();
    lifetime.current = new AbortController();
    setStatus(undefined);
    void loadStatus();
    return () => lifetime.current.abort();
  }, [open, connection.namespace]);

  async function loadStatus() {
    const controller = lifetime.current;
    setStatusLoading(true);
    setStatusError("");
    try {
      const next = await fetchFeishuStatus(connection);
      if (controller.signal.aborted) return;
      setStatus(next);
      if (next.enabled && next.connected && !next.needs_reauth) {
        void loadFolder([]);
      }
    } catch (error) {
      if (!controller.signal.aborted) setStatusError(errorText(error));
    } finally {
      if (!controller.signal.aborted) setStatusLoading(false);
    }
  }

  async function loadFolder(path: Folder[], pageToken?: string) {
    const controller = lifetime.current;
    const requestID = ++folderRequest.current;
    setFilesError("");
    if (pageToken) setLoadingMore(true);
    else setFilesLoading(true);
    try {
      const page = await listFeishuFiles(
        path.length ? path[path.length - 1].token : "",
        pageToken,
        connection,
        pageSize,
      );
      if (controller.signal.aborted || requestID !== folderRequest.current) return;
      setFolderPath(path);
      setFiles((current) => (pageToken ? [...current, ...page.files] : page.files));
      setNextPageToken(page.has_more ? page.next_page_token : undefined);
    } catch (error) {
      if (!controller.signal.aborted && requestID === folderRequest.current) {
        if (!pageToken) setFiles([]);
        setFilesError(errorText(error));
      }
    } finally {
      if (!controller.signal.aborted && requestID === folderRequest.current) {
        setFilesLoading(false);
        setLoadingMore(false);
      }
    }
  }

  async function runSearch(query: string, offset: number) {
    const controller = lifetime.current;
    const trimmed = query.trim();
    if (!trimmed) {
      setSearchError("请输入搜索关键词");
      return;
    }
    const requestID = ++searchRequest.current;
    setSearchError("");
    setSearchLoading(true);
    try {
      const page = await searchFeishuDocuments(
        trimmed,
        connection,
        offset,
        searchPageSize,
      );
      if (controller.signal.aborted || requestID !== searchRequest.current) return;
      setSearchQuery(trimmed);
      setSearchItems((current) =>
        offset > 0 ? [...current, ...page.items] : page.items,
      );
      setSearchHasMore(page.has_more);
    } catch (error) {
      if (!controller.signal.aborted && requestID === searchRequest.current) {
        setSearchError(errorText(error));
      }
    } finally {
      if (!controller.signal.aborted && requestID === searchRequest.current) {
        setSearchLoading(false);
      }
    }
  }

  function toggleSelection(documentID: string, title: string) {
    setSelected((current) => {
      const next = new Map(current);
      if (next.has(documentID)) {
        next.delete(documentID);
        return next;
      }
      if (next.size >= maxImportDocuments) {
        void message.warning(`单次最多导入 ${maxImportDocuments} 篇文档`);
        return current;
      }
      next.set(documentID, { document_id: documentID, title: title || "未命名文档" });
      return next;
    });
  }

  async function connectFeishu() {
    const controller = lifetime.current;
    setConnecting(true);
    setStatusError("");
    try {
      const authorizeURL = await startFeishuOAuth(connection);
      // The callback returns to this workspace; a full navigation is required
      // because Feishu's login page is cross-origin.
      window.location.assign(authorizeURL);
    } catch (error) {
      if (!controller.signal.aborted) {
        setStatusError(errorText(error));
        setConnecting(false);
      }
    }
  }

  async function disconnect() {
    try {
      await disconnectFeishu(connection);
      resetWorkspaceState();
      await loadStatus();
      void message.success("已断开飞书账号");
    } catch (error) {
      void message.error(errorText(error));
    }
  }

  async function submitImport() {
    if (!selectedList.length || importing) return;
    if (!canWrite) {
      setImportError("当前账号对该知识库没有整体写入权限，无法导入。");
      return;
    }
    if (!knowledgeBase) {
      setImportError("知识库配置尚未就绪，请稍后再试。");
      return;
    }
    setImporting(true);
    setImportError("");
    try {
      const response = await importFeishuDocuments(selectedList, connection);
      setResult(response);
      setSelected(new Map());
      setJobs(
        new Map(
          response.items
            .filter((item) => item.artifact_id && item.job)
            .map((item) => [item.artifact_id as string, item.job as DocumentBatchJob]),
        ),
      );
      if (response.accepted > 0) {
        onImported();
        void message.success(`已受理 ${response.accepted} 篇文档，正在后台索引`);
      }
    } catch (error) {
      setImportError(errorText(error));
    } finally {
      setImporting(false);
    }
  }

  // Poll the ordinary document batch until every accepted item reaches a
  // terminal job state. The import response is never treated as "searchable".
  useEffect(() => {
    const batchId = result?.batch_id;
    if (!batchId) return;
    let cancelled = false;
    let timer: number | undefined;
    let attempts = 0;
    const tick = async () => {
      if (attempts++ >= 600) return;
      try {
        const latest = await refreshFeishuImportJobs(batchId, connection);
        if (cancelled) return;
        setJobs(latest);
        const pending = (result?.items ?? []).some((item) => {
          if (item.status !== "accepted" || !item.artifact_id) return false;
          const job = latest.get(item.artifact_id) ?? item.job;
          return !job || (job.status !== "succeeded" && job.status !== "failed");
        });
        if (pending) timer = window.setTimeout(tick, 2000);
      } catch {
        if (!cancelled) timer = window.setTimeout(tick, 4000);
      }
    };
    timer = window.setTimeout(tick, 1200);
    return () => {
      cancelled = true;
      if (timer) window.clearTimeout(timer);
    };
  }, [result?.batch_id]);

  const browseColumns: TableColumnsType<FeishuFile> = [
    {
      title: "选择",
      key: "select",
      width: 56,
      render: (_, file) => (
        <Checkbox
          aria-label={`选择 ${file.name}`}
          checked={selected.has(file.token)}
          disabled={
            !file.importable ||
            (selected.size >= maxImportDocuments && !selected.has(file.token))
          }
          onChange={() => toggleSelection(file.token, file.name)}
        />
      ),
    },
    {
      title: "名称",
      key: "name",
      render: (_, file) => {
        if (file.navigable) {
          return (
            <button
              type="button"
              className="document-link"
              onClick={() =>
                void loadFolder([
                  ...folderPath,
                  { token: file.token, name: file.name },
                ])
              }
            >
              <FolderOpenOutlined /> {file.name}
            </button>
          );
        }
        return (
          <div className="feishu-file-name">
            <FileTextOutlined />
            <span className="break-text">{file.name}</span>
            {file.type === "shortcut" && <Tag color="blue">快捷方式</Tag>}
          </div>
        );
      },
    },
    {
      title: "类型",
      dataIndex: "type",
      width: 110,
      render: (value: string) => <Tag>{feishuTypeLabel(value)}</Tag>,
    },
    {
      title: "状态",
      key: "status",
      width: 190,
      render: (_, file) =>
        file.importable ? (
          <Tag color="green">可导入</Tag>
        ) : (
          <Tooltip title={file.unsupported_reason}>
            <Tag>{file.navigable ? "文件夹" : "暂不支持"}</Tag>
          </Tooltip>
        ),
    },
    {
      title: "更新时间",
      dataIndex: "modified_time",
      responsive: ["md"],
      render: (value: string) => (
        <span className="secondary">{formatDateTime(value)}</span>
      ),
    },
  ];

  const searchColumns: TableColumnsType<FeishuSearchItem> = [
    {
      title: "选择",
      key: "select",
      width: 56,
      render: (_, item) => (
        <Checkbox
          aria-label={`选择 ${item.title}`}
          checked={selected.has(item.token)}
          disabled={
            !item.importable ||
            (selected.size >= maxImportDocuments && !selected.has(item.token))
          }
          onChange={() => toggleSelection(item.token, item.title)}
        />
      ),
    },
    {
      title: "文档",
      key: "title",
      render: (_, item) => (
        <div className="feishu-file-name">
          <FileTextOutlined />
          <span className="break-text">{item.title}</span>
          {item.note && <span className="secondary">{item.note}</span>}
        </div>
      ),
    },
    {
      title: "类型",
      dataIndex: "type",
      width: 110,
      render: (value: string) => <Tag>{feishuTypeLabel(value)}</Tag>,
    },
  ];

  const resultRows = (result?.items ?? []).map((item, index) => ({
    ...item,
    key: `${item.document_id}:${index}`,
  }));
  const resultColumns: TableColumnsType<(typeof resultRows)[number]> = [
    {
      title: "文档",
      key: "document",
      render: (_, item) => (
        <div>
          <div className="break-text">{item.title}</div>
          <div className="secondary break-text">{item.name}</div>
        </div>
      ),
    },
    {
      title: "结果",
      key: "status",
      width: 100,
      render: (_, item) => (
        <Tag color={feishuImportStatusColors[item.status]}>
          {feishuImportStatusLabels[item.status]}
        </Tag>
      ),
    },
    {
      title: "说明",
      key: "reason",
      render: (_, item) => (
        <span className="secondary break-text">{feishuImportReason(item)}</span>
      ),
    },
    {
      title: "索引状态",
      key: "index",
      width: 150,
      render: (_, item) => {
        const job = item.artifact_id
          ? jobs.get(item.artifact_id) ?? item.job
          : item.job;
        const label = feishuJobLabel(job, item.status);
        const color =
          job?.status === "succeeded"
            ? "success"
            : job?.status === "failed"
              ? "error"
              : item.status === "accepted"
                ? "processing"
                : "default";
        return <Tag color={color}>{label}</Tag>;
      },
    },
  ];

  const connected =
    status?.enabled && status.connected && !status.needs_reauth;
  const canSubmit =
    connected && canWrite && !!knowledgeBase && selectedList.length > 0;

  function footer() {
    if (result) {
      return (
        <Space>
          <Button
            onClick={() => {
              setResult(undefined);
              setImportError("");
              setSelected(new Map());
            }}
          >
            继续导入
          </Button>
          <Button type="primary" onClick={onClose}>
            完成
          </Button>
        </Space>
      );
    }
    if (status?.enabled && status.connected && status.needs_reauth) {
      return (
        <Space>
          <Button onClick={onClose}>取消</Button>
          <Button type="primary" loading={connecting} onClick={() => void connectFeishu()}>
            重新连接飞书
          </Button>
        </Space>
      );
    }
    if (status?.enabled && !status.connected) {
      return (
        <Space>
          <Button onClick={onClose}>取消</Button>
          <Button
            type="primary"
            loading={connecting}
            icon={<LinkOutlined />}
            onClick={() => void connectFeishu()}
          >
            连接飞书
          </Button>
        </Space>
      );
    }
    return (
      <Space>
        <Button onClick={onClose}>取消</Button>
        <Button
          type="primary"
          icon={<CloudDownloadOutlined />}
          loading={importing}
          disabled={!canSubmit}
          onClick={() => void submitImport()}
        >
          导入 {selectedList.length ? `${selectedList.length} 篇` : ""}
        </Button>
      </Space>
    );
  }

  return (
    <Modal
      title="从飞书导入"
      className="feishu-import-modal"
      open={open}
      width={860}
      onCancel={onClose}
      footer={footer()}
    >
      <div className="feishu-import">
        {statusLoading && (
          <div className="secondary">
            <Spin size="small" /> 正在读取飞书连接状态
          </div>
        )}
        {statusError && (
          <Alert
            type="error"
            showIcon
            title="飞书连接状态读取失败"
            description={statusError}
            action={
              <Button size="small" onClick={() => void loadStatus()}>
                重试
              </Button>
            }
          />
        )}
        {status && !status.enabled && (
          <Alert
            type="warning"
            showIcon
            title="飞书导入未启用"
            description="部署方尚未配置飞书应用（RAG_FEISHU_APP_ID / RAG_FEISHU_APP_SECRET / RAG_FEISHU_REDIRECT_URI）。请联系管理员。"
          />
        )}
        {status?.enabled && !status.connected && (
          <Alert
            type="info"
            showIcon
            title="先连接你的飞书账号"
            description="浏览、搜索和读取正文都使用你自己的飞书权限；Praxis-RAG 只保存加密后的 user_access_token，不保存 App Secret。"
          />
        )}
        {status?.enabled && status.connected && (
          <div className="feishu-account">
            <Space wrap>
              <Tag color="green">已连接飞书</Tag>
              <span>{status.account?.name || status.account?.open_id}</span>
            </Space>
            <Popconfirm
              title="断开飞书账号"
              description="断开后需要重新授权才能继续导入；已导入的文档不受影响。"
              okText="断开"
              cancelText="取消"
              onConfirm={() => void disconnect()}
            >
              <Button size="small" danger>
                断开连接
              </Button>
            </Popconfirm>
          </div>
        )}
        {status?.notice && <Alert type="warning" showIcon title={status.notice} />}

        <Alert
          type="info"
          showIcon
          title={`导入目标：${namespace || "当前知识库"}`}
          description={`导入后，文档副本属于「${namespace || "当前知识库"}」。谁可以检索这份副本由 Praxis-RAG 的知识库权限决定，与飞书原文档的分享范围无关。`}
        />
        {!canWrite && (
          <Alert
            type="error"
            showIcon
            title="没有写入权限"
            description="你对该知识库没有整体写入权限，无法导入飞书文档。"
          />
        )}
        {!knowledgeBase && (
          <Alert
            type="warning"
            showIcon
            title="知识库配置未就绪"
            description="无法读取目标知识库配置，请关闭弹窗后重试。"
          />
        )}

        {connected && !result && (
          <>
            <Tabs
              activeKey={activeTab}
              onChange={setActiveTab}
              items={[
                {
                  key: "browse",
                  label: "浏览文件夹",
                  children: (
                    <div className="feishu-browser">
                      <div className="feishu-browser-toolbar">
                        <Breadcrumb
                          items={[
                            {
                              title: (
                                <button
                                  type="button"
                                  className="document-link"
                                  onClick={() => void loadFolder([])}
                                >
                                  我的空间
                                </button>
                              ),
                            },
                            ...folderPath.map((folder, index) => ({
                              title: (
                                <button
                                  type="button"
                                  className="document-link"
                                  onClick={() =>
                                    void loadFolder(folderPath.slice(0, index + 1))
                                  }
                                >
                                  {folder.name}
                                </button>
                              ),
                            })),
                          ]}
                        />
                        <Button
                          size="small"
                          icon={<ReloadOutlined />}
                          loading={filesLoading}
                          onClick={() => void loadFolder(folderPath)}
                        >
                          刷新
                        </Button>
                      </div>
                      {filesError && (
                        <Alert
                          type="error"
                          showIcon
                          title="文件夹读取失败"
                          description={filesError}
                        />
                      )}
                      <Table<FeishuFile>
                        rowKey="token"
                        size="small"
                        loading={filesLoading}
                        dataSource={files}
                        columns={browseColumns}
                        pagination={false}
                        locale={{
                          emptyText: (
                            <Empty
                              image={Empty.PRESENTED_IMAGE_SIMPLE}
                              description="这个文件夹里没有可访问的文档"
                            />
                          ),
                        }}
                      />
                      {nextPageToken && (
                        <Button
                          block
                          loading={loadingMore}
                          onClick={() => void loadFolder(folderPath, nextPageToken)}
                        >
                          加载更多
                        </Button>
                      )}
                    </div>
                  ),
                },
                {
                  key: "search",
                  label: "搜索文档",
                  children: (
                    <div className="feishu-browser">
                      <Input.Search
                        aria-label="搜索飞书文档"
                        placeholder="按标题搜索你有权限访问的飞书文档"
                        value={keyword}
                        onChange={(event) => setKeyword(event.target.value)}
                        onSearch={(value) => void runSearch(value, 0)}
                        enterButton={<SearchOutlined />}
                        loading={searchLoading}
                      />
                      {searchError && (
                        <Alert
                          type="error"
                          showIcon
                          title="搜索失败"
                          description={searchError}
                        />
                      )}
                      <Table<FeishuSearchItem>
                        rowKey="token"
                        size="small"
                        loading={searchLoading}
                        dataSource={searchItems}
                        columns={searchColumns}
                        pagination={false}
                        locale={{
                          emptyText: (
                            <Empty
                              image={Empty.PRESENTED_IMAGE_SIMPLE}
                              description={
                                searchQuery
                                  ? "没有找到匹配的飞书文档"
                                  : "输入关键词搜索飞书文档"
                              }
                            />
                          ),
                        }}
                      />
                      {searchHasMore && (
                        <Button
                          block
                          loading={searchLoading}
                          onClick={() => void runSearch(searchQuery, searchItems.length)}
                        >
                          加载更多
                        </Button>
                      )}
                    </div>
                  ),
                },
              ]}
            />
            <div className="feishu-selection">
              <div className="feishu-selection-heading">
                <span>
                  已选择 <strong>{selectedList.length}</strong> / {maxImportDocuments} 篇
                </span>
                {selectedList.length > 0 && (
                  <Button type="link" size="small" onClick={() => setSelected(new Map())}>
                    清空
                  </Button>
                )}
              </div>
              <Space wrap>
                {selectedList.map((document) => (
                  <Tag
                    key={document.document_id}
                    closable
                    onClose={(event) => {
                      event.preventDefault();
                      setSelected((current) => {
                        const next = new Map(current);
                        next.delete(document.document_id);
                        return next;
                      });
                    }}
                  >
                    {document.title}
                  </Tag>
                ))}
              </Space>
            </div>
          </>
        )}

        {result && (
          <div className="feishu-result">
            <Alert
              type={result.failed || result.skipped ? "warning" : "success"}
              showIcon
              title={`已受理 ${result.accepted} 篇，跳过 ${result.skipped} 篇，失败 ${result.failed} 篇`}
              description="已受理只代表进入后台索引队列；只有索引状态为「已索引」后，文档才可以被检索。"
            />
            <Table
              rowKey="key"
              size="small"
              dataSource={resultRows}
              columns={resultColumns}
              pagination={false}
            />
          </div>
        )}
        <div aria-live="polite">
          {importError && (
            <Alert type="error" showIcon title="导入失败" description={importError} />
          )}
        </div>
      </div>
    </Modal>
  );
}

// feishuTypeLabel renders the Feishu object type in Chinese.
export function feishuTypeLabel(type: string): string {
  switch (type) {
    case "docx":
      return "新版文档";
    case "doc":
      return "旧版文档";
    case "sheet":
      return "表格";
    case "bitable":
      return "多维表格";
    case "mindnote":
      return "思维笔记";
    case "folder":
      return "文件夹";
    case "shortcut":
      return "快捷方式";
    case "file":
      return "文件";
    default:
      return type || "未知";
  }
}
