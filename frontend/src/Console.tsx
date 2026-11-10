import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Alert, App, Badge, Button, Dropdown, Spin, Tooltip, type MenuProps,
} from "antd";
import {
  ApiOutlined,
  ArrowLeftOutlined,
  BellOutlined,
  BookOutlined,
  DatabaseOutlined,
  DownOutlined,
  LogoutOutlined,
  MoonOutlined,
  KeyOutlined,
  SafetyOutlined,
  SettingOutlined,
  ReloadOutlined,
  SearchOutlined,
  SunOutlined,
  TeamOutlined,
  UserOutlined,
} from "@ant-design/icons";
import {
  api,
  errorText,
  fetchAuthStatus,
  fetchCurrentUser,
  fetchInvitationInbox,
  logoutSession,
  permissionLabels,
  invitationStatus,
  readStorage,
  roleLabel,
  setUnauthorizedHandler,
  type AuthStatus,
  type AuthUser,
  type Connection,
  type Health,
  type InvitationView,
} from "./api";
import { SearchPage } from "./SearchPage";
import { DocumentsPage } from "./DocumentsPage";
import { DocumentPreview } from "./DocumentPreview";
import { KnowledgeBasesPage } from "./KnowledgeBasesPage";
import { ApiKeysPage } from "./ApiKeysPage";
import { CreateKnowledgeBase } from "./CreateKnowledgeBase";
import { LoginPage } from "./LoginPage";
import { UsersPage } from "./UsersPage";
import { InvitationsPage, InvitePage } from "./InvitationsPage";
import { PermissionsPage } from "./PermissionsPage";
import {
  readStoredInviteToken,
  storeInviteToken,
  useConsoleRoute,
  type ConsolePage,
} from "./navigation";

const pages = [
  { key: "search", label: "检索调试", icon: <SearchOutlined /> },
  { key: "documents", label: "知识文档", icon: <DatabaseOutlined /> },
  { key: "create", label: "创建知识库", icon: <ApiOutlined /> },
  { key: "permissions", label: "权限管理", icon: <SafetyOutlined /> },
  { key: "api-keys", label: "API Key", icon: <KeyOutlined /> },
  { key: "knowledge-bases", label: "知识库", icon: <SettingOutlined /> },
  { key: "users", label: "用户管理", icon: <TeamOutlined /> },
] as const;

// adminOnlyPages require the admin role once login is enabled. The API Key page
// is not listed: every account manages only its own keys there.
const adminOnlyPages = new Set<string>(["knowledge-bases", "users"]);
const inviteBellPollMs = 60_000;

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
  const [auth, setAuth] = useState<AuthStatus>();
  const [authUser, setAuthUser] = useState<AuthUser>();
  const [authReady, setAuthReady] = useState(false);
  const [authError, setAuthError] = useState("");
  const sessionMode = Boolean(auth?.auth_enabled);
  const connection = useMemo<Connection>(() => ({
    namespace,
    token: auth?.auth_enabled ? "" : token,
  }), [namespace, token, auth]);
  const [revision, setRevision] = useState(0);
  const [health, setHealth] = useState<Health>();
  const [healthError, setHealthError] = useState("");
  const [checking, setChecking] = useState(false);
  const healthRequest = useRef<AbortController | null>(null);
  const [workspaceNames, setWorkspaceNames] = useState<string[]>([]);
  const [ownedNamespaces, setOwnedNamespaces] = useState<string[]>([]);
  const [loadingWorkspaces, setLoadingWorkspaces] = useState(false);
  const [workspaceError, setWorkspaceError] = useState(false);
  const workspaceRequest = useRef<AbortController | null>(null);
  const [inbox, setInbox] = useState<InvitationView[]>([]);
  const [inboxLoading, setInboxLoading] = useState(false);
  const [inboxError, setInboxError] = useState("");
  const [inboxRevision, setInboxRevision] = useState(0);

  async function loadAuth() {
    setAuthReady(false);
    setAuthError("");
    try {
      const status = await fetchAuthStatus();
      setAuth(status);
      if (status.auth_enabled && status.authenticated) {
        const session = await fetchCurrentUser();
        setAuthUser(session.user);
      } else {
        setAuthUser(undefined);
      }
    } catch (error) {
      setAuthError(errorText(error));
    } finally {
      setAuthReady(true);
    }
  }

  useEffect(() => {
    void loadAuth();
  }, []);

  // A rejected request means the session expired; drop back to the login page.
  useEffect(() => {
    setUnauthorizedHandler(() => {
      setAuthUser(undefined);
      void message.warning("登录已失效，请重新登录");
    });
    return () => setUnauthorizedHandler(undefined);
  }, [message]);

  // The invitation inbox drives the header notification bell. It only exists
  // for console sessions; legacy API-key mode has no per-user inbox.
  const loadInbox = useCallback(async () => {
    if (!sessionMode || !authUser) {
      setInbox([]);
      return;
    }
    setInboxLoading(true);
    try {
      const rows = await fetchInvitationInbox();
      setInbox(rows);
      setInboxError("");
    } catch (error) {
      setInboxError(errorText(error));
    } finally {
      setInboxLoading(false);
    }
  }, [sessionMode, authUser]);

  useEffect(() => {
    if (!sessionMode || !authUser) {
      setInbox([]);
      setInboxError("");
      return;
    }
    void loadInbox();
    const timer = setInterval(() => void loadInbox(), inviteBellPollMs);
    return () => clearInterval(timer);
  }, [sessionMode, authUser, inboxRevision, loadInbox]);

  // Keep the raw invitation token while the visitor authenticates: the login
  // screen replaces the console but the token must survive until preview runs.
  useEffect(() => {
    if (route.page === "invite" && route.token) storeInviteToken(route.token);
  }, [route.page, route.token]);

  useEffect(() => {
    if (!sessionMode || !authUser) return;
    if (route.page !== "invite" || route.token) return;
    const token = readStoredInviteToken();
    if (token) navigate({ namespace, page: "invite", token }, true);
  }, [sessionMode, authUser, route.page, route.token, namespace, navigate]);

  // Non-admins cannot stay on an admin-only page, ordinary users cannot manage
  // permissions, and the invitation pages require a console session.
  useEffect(() => {
    if (!auth?.auth_enabled || !authUser) return;
    const adminOnly = adminOnlyPages.has(view) && authUser.role !== "admin";
    const permissionsBlocked = view === "permissions" && authUser.role === "user";
    if (adminOnly || permissionsBlocked) {
      navigate({ namespace, page: "search" }, true);
    }
  }, [auth, authUser, view, namespace, navigate]);

  async function handleLogout() {
    await logoutSession();
    setAuthUser(undefined);
    setAuth((current) => current ? { ...current, authenticated: false, user: undefined } : current);
    void message.success("已退出登录");
  }

  async function loadWorkspaces() {
    workspaceRequest.current?.abort();
    const controller = new AbortController();
    workspaceRequest.current = controller;
    setLoadingWorkspaces(true);
    setWorkspaceError(false);
    setWorkspaceNames([]);
    setOwnedNamespaces([]);
    try {
      const data = await api<{ namespaces: string[]; owned_namespaces?: string[] }>(
        "/v1/namespaces", connection, { signal: controller.signal, cache: "no-store" },
      );
      if (!controller.signal.aborted) {
        setWorkspaceNames(data.namespaces);
        // Legacy/no-session responses have no owned_namespaces field: there the
        // caller may bind every namespace, exactly like the namespaces list.
        setOwnedNamespaces(data.owned_namespaces ?? data.namespaces);
      }
    } catch (error) {
      if (!controller.signal.aborted) {
        setWorkspaceError(true);
        void message.error(errorText(error));
      }
    } finally {
      if (!controller.signal.aborted) setLoadingWorkspaces(false);
    }
  }

  useEffect(() => {
    // A direct link to #/api-keys can mount before the login state is known:
    // reload once the account (or legacy no-session mode) is available.
    if (view === "api-keys" && (!sessionMode || authUser)) void loadWorkspaces();
    return () => workspaceRequest.current?.abort();
  }, [view, sessionMode, authUser]);

  useEffect(() => {
    try {
      localStorage.setItem("rag.namespace", namespace);
    } catch {
      void message.error("浏览器无法保存工作区，请检查存储权限");
    }
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

  function saveConnection(values: Connection, notice = "连接配置已保存", page: ConsolePage = view) {
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
    navigate({ namespace: next.namespace, page });
    setToken(next.token);
    setRevision((value) => value + 1);
    void message.success(notice);
    return true;
  }
  function switchWorkspace(namespace: string) {
    saveConnection({ ...connection, namespace }, "知识库已切换");
  }
  const themeLabel = dark ? "切换浅色主题" : "切换深色主题";

  if (!authReady) {
    return (
      <div className="auth-shell">
        <Spin size="large" tip="正在检查登录状态…" />
      </div>
    );
  }
  if (sessionMode && !authUser) {
    return (
      <LoginPage
        registrationEnabled={Boolean(auth?.registration_enabled)}
        onAuthenticated={(user) => {
          setAuthUser(user);
          setAuth((current) => current
            ? { ...current, authenticated: true, user }
            : { auth_enabled: true, registration_enabled: false, authenticated: true, user });
        }}
      />
    );
  }
  if (sessionMode && authError) {
    return (
      <div className="auth-shell">
        <Alert type="error" showIcon title="无法连接服务" description={authError} />
        <Button onClick={() => void loadAuth()}>重试</Button>
      </div>
    );
  }

  const pendingInvitations = inbox.filter((invitation) => invitationStatus(invitation) === "pending");
  const bellItems: MenuProps["items"] = [
    ...(pendingInvitations.length
      ? pendingInvitations.slice(0, 5).map((invitation) => ({
          key: `invitation:${invitation.id}`,
          label: (
            <div className="invite-bell-item">
              <span className="invite-bell-kb break-text">{invitation.namespace}</span>
              <span className="secondary break-text">
                {invitation.inviter_username} 邀请你 · {permissionLabels[invitation.permission] || invitation.permission} · {invitation.scope === "all" ? "全库" : `${invitation.document_names.length} 个文件`}
              </span>
            </div>
          ),
        }))
      : [{
          key: "empty",
          disabled: true,
          label: inboxLoading ? "正在加载…" : inboxError ? "邀请加载失败" : "暂无待处理邀请",
        }]),
    { type: "divider" },
    { key: "all", label: pendingInvitations.length ? "查看全部邀请" : "邀请中心" },
  ];
  const visiblePages = pages.filter((page) => {
    if (page.key === "permissions")
      return sessionMode && (authUser?.role === "admin" || authUser?.role === "vip");
    if (adminOnlyPages.has(page.key))
      return !sessionMode || authUser?.role === "admin";
    return true;
  });

  return (
    <div className="console-shell">
      <aside className="sidebar">
        <div className="brand">
          <BookOutlined />
          <span>Praxis-RAG</span>
        </div>
        <nav aria-label="主导航">
          {visiblePages.map((page) => (
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
          {(view === "search" || view === "documents") && (
            <Dropdown
              trigger={["click"]}
              onOpenChange={(open) => { if (open) void loadWorkspaces(); }}
              disabled={uploading}
              menu={{
                selectable: true,
                selectedKeys: [connection.namespace],
                items: loadingWorkspaces || workspaceError || workspaceNames.length === 0 ? [{
                  key: ":status",
                  label: loadingWorkspaces ? "加载中…" : workspaceError ? "知识库列表加载失败" : "暂无知识库",
                  disabled: true,
                }] : workspaceNames.map((name) => ({
                  key: name,
                  label: <span className="workspace-option">{name}</span>,
                })),
                onClick: ({ key }) => {
                  if (key !== connection.namespace) switchWorkspace(key);
                },
              }}
            >
              <Button className="workspace-switch" aria-label="选择知识库" disabled={uploading}>
                <span className="workspace-name">{connection.namespace}</span>
                <DownOutlined />
              </Button>
            </Dropdown>
          )}
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
            {sessionMode && authUser && (
              <Dropdown
                trigger={["click"]}
                placement="bottomRight"
                menu={{
                  items: bellItems,
                  onClick: ({ key }) => {
                    if (key === "all" || key.startsWith("invitation:"))
                      navigate({ namespace, page: "invitations" });
                  },
                }}
              >
                <Button aria-label="邀请通知" title="邀请通知" type="text" className="invite-bell">
                  <Badge count={pendingInvitations.length} size="small" offset={[2, -2]}>
                    <BellOutlined />
                  </Badge>
                </Button>
              </Dropdown>
            )}
            {sessionMode && authUser && (
              <Dropdown
                trigger={["click"]}
                menu={{
                  items: [
                    {
                      key: "identity",
                      disabled: true,
                      label: `${authUser.username} · ${roleLabel(authUser.role)}`,
                    },
                    { type: "divider" },
                    { key: "logout", icon: <LogoutOutlined />, label: "退出登录" },
                  ],
                  onClick: ({ key }) => {
                    if (key === "logout") void handleLogout();
                  },
                }}
              >
                <Button aria-label="账号菜单" className="account-button" icon={<UserOutlined />}>
                  {authUser.username}
                </Button>
              </Dropdown>
            )}
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
                authEnabled={sessionMode}
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
          {view === "api-keys" && (!sessionMode || authUser) && (
            <ApiKeysPage
              namespaces={workspaceNames}
              ownedNamespaces={ownedNamespaces}
              role={authUser?.role}
            />
          )}
          {view === "knowledge-bases" && (!sessionMode || authUser?.role === "admin") && (
            <KnowledgeBasesPage disabled={uploading}
              onCreate={() => navigate({ namespace, page: "create" })}
              onChanged={() => { setRevision((value) => value + 1); void refreshHealth(); }} />
          )}
          {view === "users" && authUser?.role === "admin" && (
            <UsersPage currentUserId={authUser.id} />
          )}
          {view === "permissions" && authUser && (authUser.role === "admin" || authUser.role === "vip") && (
            <PermissionsPage
              connection={connection}
              currentUserId={authUser.id}
              role={authUser.role}
              onCreateKnowledgeBase={() => navigate({ namespace, page: "create" })}
            />
          )}
          {view === "invitations" && (
            <InvitationsPage
              onOpenInvite={(next) => navigate({ namespace, page: "invite", token: next })}
            />
          )}
          {view === "invite" && (
            <InvitePage
              token={route.token || readStoredInviteToken() || undefined}
              authEnabled={sessionMode}
              onHandled={() => setInboxRevision((value) => value + 1)}
              onOpenNamespace={(next) => {
                saveConnection({ ...connection, namespace: next }, "已切换到知识库", "documents");
              }}
              onBack={() => navigate({ namespace, page: "invitations" })}
            />
          )}
          <section hidden={view !== "create"} aria-label="创建知识库页面">
            <div className="page-heading">
              <h1>创建知识库</h1>
            </div>
            <CreateKnowledgeBase active={view === "create"} disabled={uploading}
              connection={connection} role={authUser?.role} onCreated={(namespace) => {
                if (saveConnection({ ...connection, namespace }, "已切换到新知识库")) {
                  navigate({ namespace, page: "documents" });
                }
              }} />
          </section>
        </div>
      </main>
    </div>
  );
}
