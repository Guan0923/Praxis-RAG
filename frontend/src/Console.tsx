import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Alert, App, Badge, Button, Dropdown, Form, Input, Modal, Tooltip,
} from "antd";
import {
  ApiOutlined,
  ArrowLeftOutlined,
  BookOutlined,
  DatabaseOutlined,
  DownOutlined,
  MoonOutlined,
  KeyOutlined,
  SettingOutlined,
  ReloadOutlined,
  SearchOutlined,
  SunOutlined,
} from "@ant-design/icons";
import {
  api,
  errorText,
  readStorage,
  type Connection,
  type Health,
} from "./api";
import { SearchPage } from "./SearchPage";
import { DocumentsPage } from "./DocumentsPage";
import { DocumentPreview } from "./DocumentPreview";
import { ModelSettings } from "./ModelSettings";
import { ApiKeysPage } from "./ApiKeysPage";
import { CreateKnowledgeBase } from "./CreateKnowledgeBase";
import { namespacePattern, useConsoleRoute } from "./navigation";

const pages = [
  { key: "search", label: "检索调试", icon: <SearchOutlined /> },
  { key: "documents", label: "知识文档", icon: <DatabaseOutlined /> },
  { key: "api-keys", label: "API Key", icon: <KeyOutlined /> },
  { key: "models", label: "模型配置", icon: <SettingOutlined /> },
  { key: "create", label: "创建知识库", icon: <ApiOutlined /> },
] as const;

function recentWorkspaces(): string[] {
  try {
    const saved: unknown = JSON.parse(
      readStorage(localStorage, "rag.workspaces", "[]"),
    );
    return Array.isArray(saved)
      ? saved.filter(
          (name): name is string =>
            typeof name === "string" && namespacePattern.test(name),
        )
      : [];
  } catch {
    return [];
  }
}

export function Console({
  dark,
  toggleTheme,
}: {
  dark: boolean;
  toggleTheme: () => void;
}) {
  const { message } = App.useApp();
  const [uploading, setUploading] = useState(false);
  const onNavigationBlocked = useCallback(() => {
    void message.warning("文档正在上传，请完成后再选择知识库");
  }, [message]);
  const { route, navigate } = useConsoleRoute(uploading, onNavigationBlocked);
  const { page: view, documentName, namespace } = route;
  const [token, setToken] = useState(() => readStorage(sessionStorage, "rag.token"));
  const connection = useMemo<Connection>(() => ({ namespace, token }), [namespace, token]);
  const [revision, setRevision] = useState(0);
  const [health, setHealth] = useState<Health>();
  const [healthError, setHealthError] = useState("");
  const [checking, setChecking] = useState(false);
  const healthRequest = useRef<AbortController | null>(null);
  const [workspaceForm] = Form.useForm<{ namespace: string }>();
  const [workspaceOpen, setWorkspaceOpen] = useState(false);
  const [recent, setRecent] = useState(recentWorkspaces);
  const workspaceNames = [...new Set([connection.namespace, "default", ...recent])];

  useEffect(() => {
    const nextRecent = [...new Set([namespace, ...recentWorkspaces()])].slice(0, 20);
    try {
      localStorage.setItem("rag.namespace", namespace);
      localStorage.setItem("rag.workspaces", JSON.stringify(nextRecent));
    } catch {
      void message.error("浏览器无法保存工作区，请检查存储权限");
    }
    setRecent(nextRecent);
  }, [namespace, message]);

  async function refreshHealth() {
    healthRequest.current?.abort();
    const controller = new AbortController();
    healthRequest.current = controller;
    setChecking(true);
    try {
      const data = await api<Health>("/health", undefined, {
        signal: controller.signal,
      });
      if (!controller.signal.aborted) {
        setHealth(data);
        setHealthError("");
      }
    } catch (error) {
      if (!controller.signal.aborted) {
        setHealth(undefined);
        setHealthError(errorText(error));
      }
    } finally {
      if (!controller.signal.aborted) setChecking(false);
    }
  }
  useEffect(() => {
    void refreshHealth();
    return () => healthRequest.current?.abort();
  }, []);

  function saveConnection(values: Connection, notice = "连接配置已保存") {
    if (uploading) return false;
    const next = {
      namespace: values.namespace.trim(),
      token: values.token || "",
    };
    try {
      sessionStorage.setItem("rag.token", next.token);
    } catch {
      void message.error("浏览器无法保存配置，请检查存储权限");
      return false;
    }
    navigate({ namespace: next.namespace, page: view });
    setToken(next.token);
    setRevision((value) => value + 1);
    void message.success(notice);
    return true;
  }
  function switchWorkspace(namespace: string) {
    if (saveConnection({ ...connection, namespace }, "工作区已切换")) {
      setWorkspaceOpen(false);
    }
  }
  const title = pages.find((page) => page.key === view)!.label;
  const themeLabel = dark ? "切换浅色主题" : "切换深色主题";

  return (
    <div className="console-shell">
      <aside className="sidebar">
        <div className="brand">
          <BookOutlined />
          <span>Praxis-RAG</span>
        </div>
        <nav aria-label="主导航">
          {pages.map((page) => (
            <Button
              key={page.key}
              type="text"
              icon={page.icon}
              aria-label={page.label}
              aria-current={view === page.key ? "page" : undefined}
              className={view === page.key ? "nav-active" : ""}
              onClick={() => {
                navigate({ namespace, page: page.key });
                if (page.key === "create") void refreshHealth();
              }}
            >
              {page.label}
            </Button>
          ))}
        </nav>
        <div className="service-state">
          <Badge
            status={healthError ? "error" : health ? "success" : "processing"}
            text={healthError ? "服务异常" : health ? "服务正常" : "检查中"}
          />
          <span className="secondary">{health?.backend || "RAG Service"}</span>
        </div>
      </aside>
      <main className="workspace">
        <header className="topbar">
          <nav className="breadcrumb" aria-label="当前位置">
            {view === "api-keys" || view === "models" || view === "create" ? <><span>全局设置</span><span>/</span></> : <>
            <span>工作区</span>
            <span>/</span>
            <Dropdown
              trigger={["click"]}
              disabled={uploading}
              menu={{
                selectable: true,
                selectedKeys: [connection.namespace],
                items: [
                  ...workspaceNames.map((name) => ({
                    key: name,
                    label: <span className="workspace-option">{name}</span>,
                  })),
                  { type: "divider" },
                  { key: ":other", label: "其他知识库…" },
                ],
                onClick: ({ key }) => {
                  if (key === ":other") {
                    workspaceForm.resetFields();
                    setWorkspaceOpen(true);
                  } else if (key !== connection.namespace) {
                    switchWorkspace(key);
                  }
                },
              }}
            >
              <Button
                type="text"
                className="workspace-switch"
                aria-label="选择知识库"
                disabled={uploading}
              >
                <span className="workspace-name">{connection.namespace}</span>
                <DownOutlined />
              </Button>
            </Dropdown>
            <span>/</span>
            </>}
            {view === "documents" && documentName !== undefined ? (
              <>
                <button
                  type="button"
                  className="breadcrumb-link"
                  onClick={() => navigate({ namespace, page: "documents" })}
                >
                  知识文档
                </button>
                <span>/</span>
                <span className="break-text" aria-current="page">{documentName}</span>
              </>
            ) : (
              <span aria-current="page">{title}</span>
            )}
          </nav>
          <div className="toolbar">
            <Tooltip title="刷新服务状态">
              <Button
                aria-label="刷新服务状态"
                type="text"
                icon={<ReloadOutlined spin={checking} />}
                onClick={() => void refreshHealth()}
              />
            </Tooltip>
            <Tooltip title={themeLabel}>
              <Button
                aria-label={themeLabel}
                type="text"
                icon={dark ? <SunOutlined /> : <MoonOutlined />}
                onClick={toggleTheme}
              />
            </Tooltip>
          </div>
        </header>
        <div className="page-content">
          {healthError && (
            <Alert
              className="page-alert"
              type="error"
              showIcon
              title="服务连接失败"
              description={healthError}
            />
          )}
          <div key={`${namespace}:${revision}`}>
            <section hidden={view !== "search"} aria-label="检索调试页面">
              <SearchPage connection={connection} onHealth={refreshHealth} />
            </section>
            <section hidden={view !== "documents" || documentName !== undefined} aria-label="知识文档页面">
              <DocumentsPage
                connection={connection}
                active={view === "documents" && documentName === undefined}
                onOpenDocument={(name) => navigate({ namespace, page: "documents", documentName: name })}
                uploading={uploading}
                onUploading={setUploading}
                onHealth={refreshHealth}
              />
            </section>
            {view === "documents" && documentName !== undefined && (
              <section aria-label="文档详情页面">
                <div className="page-heading document-heading">
                  <Tooltip title="返回知识文档">
                    <Button
                      aria-label="返回知识文档"
                      icon={<ArrowLeftOutlined />}
                      onClick={() => navigate({ namespace, page: "documents" })}
                    />
                  </Tooltip>
                  <h1>{documentName}</h1>
                </div>
                <DocumentPreview
                  key={`${revision}:${documentName}`}
                  name={documentName}
                  connection={connection}
                />
              </section>
            )}
          </div>
          {view === "api-keys" && <ApiKeysPage namespaces={workspaceNames} />}
          {view === "models" && <section aria-label="模型配置页面">
            <div className="page-heading"><h1>模型配置</h1></div>
            <ModelSettings
              connection={{ namespace: "default", token: "" }}
              disabled={uploading}
              onApplied={() => {
                setRevision((value) => value + 1);
                void refreshHealth();
              }}
            />
          </section>}
          <section hidden={view !== "create"} aria-label="创建知识库页面">
            <div className="page-heading">
              <h1>创建知识库</h1>
            </div>
            <CreateKnowledgeBase disabled={uploading} onCreated={(namespace) => {
              if (saveConnection({ ...connection, namespace }, "已切换到新知识库")) {
                navigate({ namespace, page: "documents" });
              }
            }} />
          </section>
        </div>
      </main>
      <Modal
        title="选择知识库"
        forceRender
        open={workspaceOpen}
        onCancel={() => setWorkspaceOpen(false)}
        onOk={() => workspaceForm.submit()}
        okText="切换"
        cancelText="取消"
        okButtonProps={{ disabled: uploading }}
      >
        <Form
          name="workspace-switch"
          form={workspaceForm}
          layout="vertical"
          onFinish={({ namespace }) => switchWorkspace(namespace.trim())}
          disabled={uploading}
        >
          <Form.Item
            name="namespace"
            label="知识库名称"
            rules={[
              { required: true, whitespace: true, message: "请输入知识库名称" },
              {
                pattern: namespacePattern,
                transform: (value) => value?.trim(),
                message: "使用 1–64 个字母、数字、点、下划线或横线，首位为字母或数字",
              },
            ]}
          >
            <Input maxLength={64} autoComplete="off" />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
}
