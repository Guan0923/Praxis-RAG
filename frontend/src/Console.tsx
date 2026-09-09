import { useEffect, useRef, useState } from "react";
import { Alert, App, Badge, Button, Form, Input, Tooltip } from "antd";
import {
  ApiOutlined,
  BookOutlined,
  DatabaseOutlined,
  MoonOutlined,
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

const pages = [
  { key: "search", label: "检索调试", icon: <SearchOutlined /> },
  { key: "documents", label: "知识文档", icon: <DatabaseOutlined /> },
  { key: "connection", label: "连接配置", icon: <ApiOutlined /> },
] as const;
type Page = (typeof pages)[number]["key"];

export function Console({
  dark,
  toggleTheme,
}: {
  dark: boolean;
  toggleTheme: () => void;
}) {
  const { message } = App.useApp();
  const [view, setView] = useState<Page>("search");
  const [connection, setConnection] = useState<Connection>(() => ({
    namespace: readStorage(localStorage, "rag.namespace", "default"),
    token: readStorage(sessionStorage, "rag.token"),
  }));
  const [revision, setRevision] = useState(0);
  const [uploading, setUploading] = useState(false);
  const [health, setHealth] = useState<Health>();
  const [healthError, setHealthError] = useState("");
  const [checking, setChecking] = useState(false);
  const healthRequest = useRef<AbortController | null>(null);
  const [form] = Form.useForm<Connection>();

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

  function saveConnection(values: Connection) {
    if (uploading) return;
    const next = {
      namespace: values.namespace.trim(),
      token: values.token || "",
    };
    try {
      localStorage.setItem("rag.namespace", next.namespace);
      sessionStorage.setItem("rag.token", next.token);
    } catch {
      void message.error("浏览器无法保存配置，请检查存储权限");
      return;
    }
    setConnection(next);
    setRevision((value) => value + 1);
    void message.success("连接配置已保存");
  }
  const config = {
    url: `${location.origin}/mcp`,
    headers: {
      "X-RAG-Namespace": connection.namespace,
      ...(connection.token
        ? { Authorization: "Bearer <RAG_SERVICE_TOKEN>" }
        : {}),
    },
  };
  const title = pages.find((page) => page.key === view)!.label;
  const themeLabel = dark ? "切换浅色主题" : "切换深色主题";

  return (
    <div className="console-shell">
      <aside className="sidebar">
        <div className="brand">
          <BookOutlined />
          <span>星河知识库</span>
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
                setView(page.key);
                if (page.key === "connection") void refreshHealth();
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
          <div className="breadcrumb">
            <span>工作区</span>
            <span>/</span>
            <span className="workspace-name">{connection.namespace}</span>
            <span>/</span>
            <span>{title}</span>
          </div>
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
          <div key={revision}>
            <section hidden={view !== "search"} aria-label="检索调试页面">
              <SearchPage connection={connection} onHealth={refreshHealth} />
            </section>
            <section hidden={view !== "documents"} aria-label="知识文档页面">
              <DocumentsPage
                connection={connection}
                active={view === "documents"}
                uploading={uploading}
                onUploading={setUploading}
                onHealth={refreshHealth}
              />
            </section>
          </div>
          <section hidden={view !== "connection"} aria-label="连接配置页面">
            <div className="page-heading">
              <h1>连接配置</h1>
            </div>
            <div className="settings-section">
              <h2>工作区与认证</h2>
              <Form
                form={form}
                layout="vertical"
                initialValues={connection}
                onFinish={saveConnection}
                disabled={uploading}
                className="connection-form"
              >
                <Form.Item
                  name="namespace"
                  label="Namespace"
                  rules={[
                    { required: true, message: "请输入 Namespace" },
                    {
                      pattern: /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/,
                      transform: (value) => value?.trim(),
                      message:
                        "使用 1–64 个字母、数字、点、下划线或横线，首位为字母或数字",
                    },
                  ]}
                >
                  <Input maxLength={64} />
                </Form.Item>
                <Form.Item name="token" label="Service Token">
                  <Input.Password
                    autoComplete="off"
                    placeholder="本机连接可留空"
                  />
                </Form.Item>
                <Button htmlType="submit" type="primary" disabled={uploading}>
                  保存配置
                </Button>
              </Form>
            </div>
            <div className="settings-section">
              <h2>模型状态</h2>
              {health?.models ? (
                Object.entries(health.models).map(([key, model]) => (
                  <div className="model-row" key={key}>
                    <span>
                      {key === "embedding" ? "Embedding" : "Reranker"}
                    </span>
                    <div>
                      {model.name}
                      <div className="secondary">
                        device: {model.device || "auto"}
                      </div>
                    </div>
                    <Badge
                      status={model.loaded ? "success" : "default"}
                      text={model.loaded ? "已载入" : "未载入"}
                    />
                  </div>
                ))
              ) : (
                <Alert
                  type="info"
                  title={
                    checking ? "正在检查模型状态" : "当前服务未报告模型状态"
                  }
                />
              )}
            </div>
            <div className="settings-section">
              <h2>HTTP MCP 配置</h2>
              <pre className="config-code" data-testid="mcp-config">
                {JSON.stringify(config, null, 2)}
              </pre>
            </div>
          </section>
        </div>
      </main>
    </div>
  );
}
