import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Alert, App, Avatar, Badge, Button, Dropdown, Spin, Tooltip, type MenuProps,
} from "antd";
import {
  ApiOutlined,
  AppstoreOutlined,
  ArrowLeftOutlined,
  BellOutlined,
  BookOutlined,
  DashboardOutlined,
  DatabaseOutlined,
  DownOutlined,
  IdcardOutlined,
  LogoutOutlined,
  MailOutlined,
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
  avatarUrl,
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
import { ModelProfilesPage } from "./ModelProfilesPage";
import { MonitorPage } from "./MonitorPage";
import { ApiKeysPage } from "./ApiKeysPage";
import { CreateKnowledgeBase } from "./CreateKnowledgeBase";
import { LoginPage } from "./LoginPage";
import { ProfileModal } from "./ProfileModal";
import { SystemSettingsPage } from "./SystemSettingsPage";
import { UsersPage } from "./UsersPage";
import { InvitationsPage, InvitePage } from "./InvitationsPage";
import { PermissionsPage } from "./PermissionsPage";
import {
  canViewConsolePage,
  readStoredInviteToken,
  routeForNamespace,
  storeInviteToken,
  useConsoleRoute,
  type ConsolePage,
} from "./navigation";
import { useWorkspaces, workspacePagesAccessible } from "./workspaces";

const pages = [
  { key: "search", label: "检索调试", icon: <SearchOutlined /> },
  { key: "documents", label: "知识文档", icon: <DatabaseOutlined /> },
  { key: "create", label: "创建知识库", icon: <ApiOutlined /> },
  { key: "permissions", label: "权限管理", icon: <SafetyOutlined /> },
  { key: "api-keys", label: "API Key", icon: <KeyOutlined /> },
  { key: "model-profiles", label: "模型配置", icon: <AppstoreOutlined /> },
  { key: "knowledge-bases", label: "知识库", icon: <SettingOutlined /> },
  { key: "users", label: "用户管理", icon: <TeamOutlined /> },
  { key: "system-settings", label: "系统设置", icon: <MailOutlined /> },
  { key: "monitor", label: "系统监控", icon: <DashboardOutlined /> },
] as const;

// adminOnlyPages (navigation.ts) gate the sidebar and direct #/<page> links.
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
  // Feishu OAuth returns to /console?feishu=...; report the outcome once and
  // strip the query so a refresh does not repeat the notice.
  const feishuNoticeShown = useRef(false);
  useEffect(() => {
    if (feishuNoticeShown.current) return;
    const params = new URLSearchParams(window.location.search);
    const outcome = params.get("feishu");
    if (!outcome) return;
    feishuNoticeShown.current = true;
    const reason = params.get("feishu_reason") || "";
    if (outcome === "connected") {
      void message.success("飞书账号已连接，可以开始导入文档");
    } else if (outcome === "denied") {
      void message.warning("已取消飞书授权");
    } else if (outcome === "login_required") {
      void message.error("登录状态已失效，请重新登录后再连接飞书");
    } else {
      void message.error(feishuOAuthError(reason));
    }
    params.delete("feishu");
    params.delete("feishu_reason");
    const query = params.toString();
    window.history.replaceState(
      null,
      "",
      `${window.location.pathname}${query ? `?${query}` : ""}${window.location.hash}`,
    );
  }, [message]);
  const onNavigationBlocked = useCallback(() => {
    void message.warning("文档正在上传，请完成后再选择知识库");
  }, [message]);
  const { route, navigate } = useConsoleRoute(uploading, onNavigationBlocked);
  const { page: view, documentName, namespace } = route;
  const [token, setToken] = useState(() => readStorage(sessionStorage, "rag.token"));
  const [auth, setAuth] = useState<AuthStatus>();
  const [authUser, setAuthUser] = useState<AuthUser>();
  const [profileOpen, setProfileOpen] = useState(false);
  const [authReady, setAuthReady] = useState(false);
  const [authError, setAuthError] = useState("");
  const sessionMode = Boolean(auth?.auth_enabled);
  const connection = useMemo<Connection>(() => ({
    namespace,
    token: auth?.auth_enabled ? "" : token,
  }), [namespace, token, auth]);
  const [revision, setRevision] = useState(0);
  // documentRevision refreshes an already-opened document preview after a
  // replacement, without remounting the whole workspace page.
  const [documentRevision, setDocumentRevision] = useState(0);
  const [health, setHealth] = useState<Health>();
  const [healthError, setHealthError] = useState("");
  const [checking, setChecking] = useState(false);
  const healthRequest = useRef<AbortController | null>(null);
  const [inbox, setInbox] = useState<InvitationView[]>([]);
  const [inboxLoading, setInboxLoading] = useState(false);
  const [inboxError, setInboxError] = useState("");
  const [inboxRevision, setInboxRevision] = useState(0);
  // inboxRequest aborts the previous inbox fetch and inboxGeneration drops a
  // response that belongs to an account which already signed out; both keep one
  // user's invitations from landing in another user's notification bell.
  const inboxRequest = useRef<AbortController | null>(null);
  const inboxGeneration = useRef(0);
  // The identity the route namespace was last synced for: a change means the
  // account switched rather than an initial/auto-corrected namespace, which
  // decides whether a stale document detail route has to be dropped.
  const syncedWorkspaceIdentity = useRef<string | null>(null);
  // The workspace-selection lifecycle: load GET /v1/namespaces once the
  // session identity is known, validate/restore/auto-correct the selection,
  // persist it per user and drop everything when the account changes. Legacy
  // API-key mode keeps the historical shared key and falls back to the old
  // namespace-driven behaviour while loading or after a list error.
  const workspaceIdentityReady = authReady && (!sessionMode || Boolean(authUser));
  const workspaces = useWorkspaces({
    enabled: workspaceIdentityReady,
    sessionMode,
    userId: authUser?.id,
    connection,
  });
  const workspaceNames = workspaces.namespaces;
  const ownedNamespaces = workspaces.ownedNamespaces;
  const workspaceError = Boolean(workspaces.error);
  const workspaceEmpty = workspaces.empty;

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
    // Invalidate every earlier response before starting: the identity or the
    // revision may have changed while the previous request was in flight.
    const generation = ++inboxGeneration.current;
    inboxRequest.current?.abort();
    if (!sessionMode || !authUser) {
      inboxRequest.current = null;
      setInbox([]);
      setInboxError("");
      setInboxLoading(false);
      return;
    }
    const controller = new AbortController();
    inboxRequest.current = controller;
    setInboxLoading(true);
    try {
      const rows = await fetchInvitationInbox({ signal: controller.signal });
      if (controller.signal.aborted || generation !== inboxGeneration.current) return;
      setInbox(rows);
      setInboxError("");
    } catch (error) {
      if (controller.signal.aborted || generation !== inboxGeneration.current) return;
      setInboxError(errorText(error));
    } finally {
      if (!controller.signal.aborted && generation === inboxGeneration.current)
        setInboxLoading(false);
    }
  }, [sessionMode, authUser]);

  useEffect(() => {
    if (!sessionMode || !authUser) {
      inboxGeneration.current += 1;
      inboxRequest.current?.abort();
      inboxRequest.current = null;
      setInbox([]);
      setInboxError("");
      setInboxLoading(false);
      return;
    }
    void loadInbox();
    const timer = setInterval(() => void loadInbox(), inviteBellPollMs);
    return () => {
      clearInterval(timer);
      // Account switch/unmount: a response from the previous identity must not
      // reach the next account's bell.
      inboxGeneration.current += 1;
      inboxRequest.current?.abort();
    };
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

  // A page this account may not view redirects to the search page; the rule is
  // shared with the sidebar filter, so a direct #/<page> link cannot bypass it.
  useEffect(() => {
    if (!auth?.auth_enabled || !authUser) return;
    if (!canViewConsolePage(view, true, authUser.role)) {
      navigate({ namespace, page: "search" }, true);
    }
  }, [auth, authUser, view, namespace, navigate]);

  async function handleLogout() {
    await logoutSession();
    setAuthUser(undefined);
    setAuth((current) => current ? { ...current, authenticated: false, user: undefined } : current);
    void message.success("已退出登录");
  }

  // Keep the route namespace in sync with the validated selection: a restored
  // or auto-corrected namespace must also drive the page connection, while an
  // explicit workspace switch sets both sides at once. `replace` avoids a
  // history entry per correction and never fights a user selection. The same
  // rule applies to legacy no-session mode, where a remembered namespace can be
  // invalid too.
  useEffect(() => {
    if (!workspaces.settled || workspaces.error || !workspaces.namespace) return;
    // The first settled identity is not a switch: a deep link opened by the
    // same account keeps its document detail. A later identity means the
    // previous account's document detail route must be dropped.
    const identityChanged = syncedWorkspaceIdentity.current !== null
      && syncedWorkspaceIdentity.current !== workspaces.identity;
    syncedWorkspaceIdentity.current = workspaces.identity;
    if (!identityChanged && route.namespace === workspaces.namespace) return;
    navigate(routeForNamespace(route, workspaces.namespace, identityChanged), true);
  }, [workspaces.settled, workspaces.error, workspaces.namespace, workspaces.identity, route, navigate]);

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
  function switchWorkspace(next: string) {
    // Persist the per-user selection immediately, then drive the route so the
    // existing namespace/remount behaviour is preserved.
    workspaces.select(next);
    saveConnection({ ...connection, namespace: next }, "知识库已切换");
  }
  // workspaceSelected prefers the validated selection over the route: the route
  // correction runs in an effect, one render later, so the selector must never
  // briefly show the previous account's namespace.
  const workspaceSelected = workspaces.namespace || namespace;
  // workspaceLabel annotates the selector while the accessible list is not
  // usable yet instead of showing a stale/guessed namespace. Once settled it
  // prefers the hook's validated namespace over the route, which is corrected
  // in an effect one render later.
  const workspaceLabel = !workspaces.settled
    ? sessionMode ? "加载中…" : namespace
    : workspaces.error
      ? sessionMode ? "加载失败" : namespace
      : workspaceEmpty
        ? workspaces.loading ? "加载中…" : "暂无可访问的知识库"
        : workspaces.namespace || namespace;
  // Namespace-dependent pages stay unmounted until access is proven: with a
  // console session we must not fire /v1/documents or search requests against a
  // namespace the account may no longer access. Legacy mode only falls back to
  // historical behaviour while loading/after an error; an empty settled list
  // stops the pages there too.
  const workspaceSyncing = Boolean(
    workspaces.settled && !workspaces.error && !workspaceEmpty
    && workspaces.namespace && route.namespace !== workspaces.namespace,
  );
  const namespaceAccessible = workspacePagesAccessible({
    sessionMode,
    settled: workspaces.settled,
    error: workspaces.error,
    empty: workspaceEmpty,
    syncing: workspaceSyncing,
  });
  const workspaceGateVisible = Boolean(
    (view === "search" || view === "documents") && !namespaceAccessible
    && (sessionMode || workspaces.settled),
  );
  const themeLabel = dark ? "切换浅色主题" : "切换深色主题";

  if (!authReady) {
    return (
      <div className="auth-shell">
        <Spin size="large" description="正在检查登录状态…" />
      </div>
    );
  }
  if (sessionMode && !authUser) {
    return (
      <LoginPage
        registrationEnabled={Boolean(auth?.registration_enabled)}
        emailVerificationEnabled={Boolean(auth?.email_verification_enabled)}
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
  const visiblePages = pages.filter((page) =>
    canViewConsolePage(page.key, sessionMode, authUser?.role));
  // The selector's menu doubles as its status display: loading, explicit
  // failure + retry, the exact "no accessible knowledge base" copy, or the
  // accessible list.
  const workspaceMenuItems: MenuProps["items"] =
    workspaces.loading && (workspaceEmpty || !workspaces.settled)
      ? [{ key: ":status", disabled: true, label: "加载中…" }]
      : workspaces.error
        ? [
            { key: ":status", disabled: true, label: "知识库列表加载失败" },
            { key: ":retry", label: "重试", icon: <ReloadOutlined /> },
          ]
        : workspaceEmpty
          ? [{ key: ":status", disabled: true, label: "暂无可访问的知识库" }]
          : !workspaces.settled
            ? [{ key: ":status", disabled: true, label: "加载中…" }]
            : workspaces.namespaces.map((name) => ({
                key: name,
                label: <span className="workspace-option">{name}</span>,
              }));

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
              onOpenChange={(open) => { if (open) void workspaces.reload(); }}
              disabled={uploading}
              menu={{
                selectable: true,
                selectedKeys: workspaceSelected ? [workspaceSelected] : [],
                items: workspaceMenuItems,
                onClick: ({ key }) => {
                  if (key === ":retry") {
                    void workspaces.reload();
                    return;
                  }
                  if (key.startsWith(":")) return;
                  if (key !== workspaceSelected) switchWorkspace(key);
                },
              }}
            >
              <Button
                className="workspace-switch"
                aria-label="选择知识库"
                disabled={uploading}
                title={workspaceLabel}
              >
                <span className="workspace-name">{workspaceLabel}</span>
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
                    { key: "profile", icon: <IdcardOutlined />, label: "个人信息" },
                    { type: "divider" },
                    { key: "logout", icon: <LogoutOutlined />, label: "退出登录" },
                  ],
                  onClick: ({ key }) => {
                    if (key === "profile") setProfileOpen(true);
                    if (key === "logout") void handleLogout();
                  },
                }}
              >
                <Button
                  aria-label="账号菜单"
                  className="account-button"
                  icon={authUser.has_avatar
                    ? <Avatar size={22} src={avatarUrl(authUser.avatar_updated_at)} />
                    : <UserOutlined />}
                >
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
          {sessionMode && workspaces.error && !workspaceGateVisible && (
            <Alert
              className="page-alert"
              type="error"
              showIcon
              title="知识库列表加载失败"
              description={workspaces.error}
              action={<Button size="small" onClick={() => void workspaces.reload()}>重试</Button>}
            />
          )}
          {workspaceGateVisible && (
            <section aria-label="知识库访问状态">
              {workspaceError ? (
                <Alert
                  type="error"
                  showIcon
                  title="知识库列表加载失败"
                  description={workspaces.error}
                  action={<Button onClick={() => void workspaces.reload()}>重试</Button>}
                />
              ) : workspaceEmpty ? (
                <Alert
                  type="info"
                  showIcon
                  title="暂无可访问的知识库"
                  description="你还没有可访问的知识库。请先创建一个知识库，或联系管理员为你分配访问权限。"
                  action={(
                    <Button type="primary" onClick={() => navigate({ namespace, page: "create" })}>
                      创建知识库
                    </Button>
                  )}
                />
              ) : (
                <div className="auth-shell">
                  <Spin size="large" description="正在加载知识库…" />
                </div>
              )}
            </section>
          )}
          {namespaceAccessible && (
            <div key={`${authUser?.id ?? "legacy"}:${namespace}:${revision}`}>
              <section hidden={view !== "search"} aria-label="检索调试页面">
                <SearchPage connection={connection} onHealth={refreshHealth} />
              </section>
              <section hidden={view !== "documents" || documentName !== undefined} aria-label="知识文档页面">
                <DocumentsPage
                  connection={connection}
                  active={view === "documents" && documentName === undefined}
                  authEnabled={sessionMode}
                  onOpenDocument={(name) => navigate({ namespace, page: "documents", documentName: name })}
                  onDocumentReplaced={() => setDocumentRevision((value) => value + 1)}
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
                    key={`${revision}:${documentRevision}:${documentName}`}
                    name={documentName}
                    connection={connection}
                  />
                </section>
              )}
            </div>
          )}
          {view === "api-keys" && (!sessionMode || authUser) && (
            <ApiKeysPage
              namespaces={workspaceNames}
              ownedNamespaces={ownedNamespaces}
              role={authUser?.role}
            />
          )}
          {view === "model-profiles" && (!sessionMode || authUser) && (
            <ModelProfilesPage disabled={uploading} role={authUser?.role} />
          )}
          {view === "knowledge-bases" && (!sessionMode || authUser?.role === "admin") && (
            <KnowledgeBasesPage disabled={uploading}
              onCreate={() => navigate({ namespace, page: "create" })}
              onChanged={() => { setRevision((value) => value + 1); void refreshHealth(); }} />
          )}
          {view === "users" && authUser?.role === "admin" && (
            <UsersPage currentUserId={authUser.id} />
          )}
          {view === "system-settings" && authUser?.role === "admin" && (
            <SystemSettingsPage />
          )}
          {view === "monitor" && canViewConsolePage("monitor", sessionMode, authUser?.role) && (
            <MonitorPage role={authUser?.role} />
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
                // Accepting an invitation can expose a new namespace: select it
                // before the route change and refresh the accessible list.
                workspaces.select(next);
                saveConnection({ ...connection, namespace: next }, "已切换到知识库", "documents");
                void workspaces.reload();
              }}
              onBack={() => navigate({ namespace, page: "invitations" })}
            />
          )}
          <section hidden={view !== "create"} aria-label="创建知识库页面">
            <div className="page-heading">
              <h1>创建知识库</h1>
            </div>
            <CreateKnowledgeBase active={view === "create"} disabled={uploading}
              connection={connection}
              onOpenModelProfiles={() => navigate({ namespace, page: "model-profiles" })}
              onCreated={(next) => {
                // Select the freshly created namespace and refresh the list so
                // the selector/correction logic sees it as accessible.
                workspaces.select(next);
                if (saveConnection({ ...connection, namespace: next }, "已切换到新知识库")) {
                  navigate({ namespace: next, page: "documents" });
                  void workspaces.reload();
                }
              }} />
          </section>
        </div>
      </main>
      {sessionMode && authUser && (
        <ProfileModal
          open={profileOpen}
          user={authUser}
          onUpdated={setAuthUser}
          onClose={() => setProfileOpen(false)}
        />
      )}
    </div>
  );
}

// feishuOAuthError maps the callback's stable reason code to actionable text.
function feishuOAuthError(reason: string): string {
  switch (reason) {
    case "state":
      return "飞书授权校验已过期，请重新发起连接";
    case "permission":
      return "飞书授权缺少文档权限，请重新连接并同意全部权限";
    case "reauth":
      return "飞书账号授权失败，请重新连接";
    case "config":
      return "飞书应用配置有误，请联系管理员检查 App ID / Secret / 回调地址";
    case "network":
      return "连接飞书服务失败，请稍后重试";
    case "disabled":
      return "部署方尚未启用飞书导入";
    default:
      return "飞书授权失败，请重试";
  }
}
