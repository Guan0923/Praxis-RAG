import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Alert,
  App,
  Button,
  Empty,
  Form,
  Modal,
  Popconfirm,
  Radio,
  Select,
  Space,
  Spin,
  Table,
  Tag,
  Tooltip,
  type TableColumnsType,
} from "antd";
import {
  CopyOutlined,
  DeleteOutlined,
  PlusOutlined,
  ReloadOutlined,
  SafetyOutlined,
  TeamOutlined,
} from "@ant-design/icons";
import {
  api,
  createInvitation,
  errorText,
  fetchAccess,
  fetchGrants,
  fetchInvitableUsers,
  fetchNamespaceInvitations,
  formatDateTime,
  invitationLink,
  invitationStatus,
  invitationStatusColors,
  invitationStatusLabels,
  permissionLabels,
  revokeGrant,
  revokeInvitation,
  roleLabel,
  type AccessView,
  type Connection,
  type CreateInvitationResult,
  type DocumentInfo,
  type GrantView,
  type InvitationView,
  type InviteUser,
  type PermissionLevel,
  type Role,
  type ShareScope,
} from "./api";
import { InvitationScope } from "./InvitationsPage";

type InviteFormValues = {
  username: string;
  permission: PermissionLevel;
  scope: ShareScope;
  document_names?: string[];
};

type AggregatedGrant = {
  user_id: string;
  username: string;
  wholeRead: boolean;
  wholeWrite: boolean;
  readDocuments: string[];
  writeDocuments: string[];
  granted_by_username: string;
  updated_at: string;
};

// aggregateGrants folds the per-row permission model (one row per file) into one
// row per user, keeping whole-library and per-file read/write scopes apart.
function aggregateGrants(grants: GrantView[]): AggregatedGrant[] {
  const byUser = new Map<string, AggregatedGrant & { grantedBy: Set<string> }>();
  for (const grant of grants) {
    let item = byUser.get(grant.user_id);
    if (!item) {
      item = {
        user_id: grant.user_id,
        username: grant.username,
        wholeRead: false,
        wholeWrite: false,
        readDocuments: [],
        writeDocuments: [],
        granted_by_username: "",
        updated_at: "",
        grantedBy: new Set<string>(),
      };
      byUser.set(grant.user_id, item);
    }
    const write = grant.permission === "write";
    if (!grant.document_name) {
      item.wholeRead = true;
      if (write) item.wholeWrite = true;
    } else if (write) {
      if (!item.writeDocuments.includes(grant.document_name)) item.writeDocuments.push(grant.document_name);
    } else if (!item.readDocuments.includes(grant.document_name)) {
      item.readDocuments.push(grant.document_name);
    }
    if (grant.granted_by_username) item.grantedBy.add(grant.granted_by_username);
    if (grant.updated_at > item.updated_at) item.updated_at = grant.updated_at;
  }
  return Array.from(byUser.values(), ({ grantedBy, ...item }) => ({
    ...item,
    granted_by_username: Array.from(grantedBy).join("、"),
  }));
}

export function PermissionsPage({
  connection,
  currentUserId,
  role,
  onCreateKnowledgeBase,
}: {
  connection: Connection;
  currentUserId: string;
  role: Role;
  onCreateKnowledgeBase: () => void;
}) {
  const { message } = App.useApp();
  const [form] = Form.useForm<InviteFormValues>();
  const scopeValue = Form.useWatch("scope", form);

  const [namespaces, setNamespaces] = useState<string[]>([]);
  const [accessByNamespace, setAccessByNamespace] = useState<Record<string, AccessView>>({});
  const [loadingNamespaces, setLoadingNamespaces] = useState(false);
  const [namespacesError, setNamespacesError] = useState("");
  const [selected, setSelected] = useState<string>();

  const [grants, setGrants] = useState<GrantView[]>([]);
  const [grantsLoading, setGrantsLoading] = useState(false);
  const [grantsError, setGrantsError] = useState("");
  const [invitations, setInvitations] = useState<InvitationView[]>([]);
  const [invitationsLoading, setInvitationsLoading] = useState(false);
  const [invitationsError, setInvitationsError] = useState("");
  const [revision, setRevision] = useState(0);
  const [listRevision, setListRevision] = useState(0);

  const [users, setUsers] = useState<InviteUser[]>([]);
  const [usersLoading, setUsersLoading] = useState(false);
  const [usersError, setUsersError] = useState("");
  const [documents, setDocuments] = useState<DocumentInfo[]>([]);
  const [documentsFor, setDocumentsFor] = useState("");
  const [documentsLoading, setDocumentsLoading] = useState(false);
  const [documentsError, setDocumentsError] = useState("");

  const [inviteOpen, setInviteOpen] = useState(false);
  const [creating, setCreating] = useState(false);
  const [inviteError, setInviteError] = useState("");
  const [created, setCreated] = useState<(CreateInvitationResult & { link: string })>();
  const [copied, setCopied] = useState(false);
  const [revokingGrantUser, setRevokingGrantUser] = useState("");
  const [revokingInvitationId, setRevokingInvitationId] = useState("");

  const loadNamespaces = useCallback(async () => {
    setLoadingNamespaces(true);
    setNamespacesError("");
    try {
      const data = await api<{ namespaces: string[] }>("/v1/namespaces", connection, { cache: "no-store" });
      const names = data.namespaces || [];
      // Only knowledge bases the account may manage (admin or owner) are
      // selectable; others stay read-only elsewhere in the Console. The server
      // exposes can_share/can_manage; fall back to can_manage if a deployment
      // predates can_share.
      const probes = await Promise.all(names.map(async (namespace) => {
        try {
          const access = await fetchAccess(namespace, connection);
          const shareable = typeof access.can_share === "boolean" ? access.can_share : access.can_manage;
          return access.can_manage && shareable ? { namespace, access } : undefined;
        } catch {
          return undefined;
        }
      }));
      const manageable = probes.filter((item): item is { namespace: string; access: AccessView } => !!item);
      setNamespaces(manageable.map((item) => item.namespace));
      setAccessByNamespace(Object.fromEntries(manageable.map((item) => [item.namespace, item.access])));
      setSelected((current) =>
        current && manageable.some((item) => item.namespace === current)
          ? current
          : manageable[0]?.namespace);
    } catch (cause) {
      setNamespacesError(errorText(cause));
    } finally {
      setLoadingNamespaces(false);
    }
  }, [connection]);

  useEffect(() => {
    void loadNamespaces();
  }, [loadNamespaces, revision]);

  // Grants and invitations reload together after any mutation in the page.
  useEffect(() => {
    if (!selected) {
      setGrants([]);
      setInvitations([]);
      setGrantsError("");
      setInvitationsError("");
      return;
    }
    let cancelled = false;
    setGrantsLoading(true);
    setGrantsError("");
    void fetchGrants(selected, connection)
      .then((rows) => { if (!cancelled) setGrants(rows); })
      .catch((cause) => { if (!cancelled) setGrantsError(errorText(cause)); })
      .finally(() => { if (!cancelled) setGrantsLoading(false); });
    setInvitationsLoading(true);
    setInvitationsError("");
    void fetchNamespaceInvitations(selected, connection)
      .then((rows) => { if (!cancelled) setInvitations(rows); })
      .catch((cause) => { if (!cancelled) setInvitationsError(errorText(cause)); })
      .finally(() => { if (!cancelled) setInvitationsLoading(false); });
    return () => { cancelled = true; };
  }, [selected, revision, listRevision, connection]);

  const aggregated = useMemo(() => aggregateGrants(grants), [grants]);
  const selectedAccess = selected ? accessByNamespace[selected] : undefined;
  const canShare = selectedAccess ? selectedAccess.can_share || selectedAccess.can_manage : true;
  const pendingInvitations = invitations.filter((item) => invitationStatus(item) === "pending").length;

  async function loadUsers() {
    setUsersLoading(true);
    setUsersError("");
    try {
      setUsers(await fetchInvitableUsers());
    } catch (cause) {
      setUsersError(errorText(cause));
    } finally {
      setUsersLoading(false);
    }
  }

  async function loadDocuments(namespace: string) {
    setDocumentsLoading(true);
    setDocumentsError("");
    try {
      const data = await api<{ documents: DocumentInfo[] }>(
        "/v1/documents",
        { ...connection, namespace },
        { cache: "no-store" },
      );
      setDocuments(data.documents || []);
      setDocumentsFor(namespace);
    } catch (cause) {
      setDocuments([]);
      setDocumentsFor(namespace);
      setDocumentsError(errorText(cause));
    } finally {
      setDocumentsLoading(false);
    }
  }

  function openInviteDialog() {
    if (!selected || !canShare) return;
    setCreated(undefined);
    setInviteError("");
    setCopied(false);
    form.resetFields();
    form.setFieldsValue({ permission: "read", scope: "all", document_names: [] });
    setInviteOpen(true);
    if (!users.length && !usersLoading) void loadUsers();
    if (documentsFor !== selected) void loadDocuments(selected);
  }

  async function submitInvite(values: InviteFormValues) {
    if (!selected || creating) return;
    const documentNames = values.scope === "files"
      ? (values.document_names || []).filter(Boolean)
      : [];
    if (values.scope === "files" && !documentNames.length) {
      setInviteError("请至少选择一个文件");
      return;
    }
    setCreating(true);
    setInviteError("");
    try {
      const result = await createInvitation(selected, {
        username: values.username,
        permission: values.permission,
        scope: values.scope,
        document_names: documentNames,
      }, connection);
      setCreated({ ...result, link: invitationLink(result.invite_path) });
      setCopied(false);
      void message.success("邀请已创建，已向该用户推送邀请消息");
      setListRevision((value) => value + 1);
    } catch (cause) {
      setInviteError(errorText(cause));
    } finally {
      setCreating(false);
    }
  }

  async function copyLink(link: string) {
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(link);
        setCopied(true);
        void message.success("邀请链接已复制");
        return;
      }
    } catch {
      // Clipboard permission may be denied; fall back to a temporary textarea.
    }
    try {
      const area = document.createElement("textarea");
      area.value = link;
      area.style.position = "fixed";
      area.style.opacity = "0";
      document.body.appendChild(area);
      area.select();
      document.execCommand("copy");
      document.body.removeChild(area);
      setCopied(true);
      void message.success("邀请链接已复制");
    } catch {
      void message.error("复制失败，请手动选中链接复制");
    }
  }

  async function revokeGrantFor(item: AggregatedGrant) {
    if (!selected) return;
    setRevokingGrantUser(item.user_id);
    try {
      await revokeGrant(selected, item.user_id, connection);
      void message.success(`已撤销 ${item.username} 的授权`);
      setListRevision((value) => value + 1);
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setRevokingGrantUser("");
    }
  }

  async function revokeInvitationFor(invitation: InvitationView) {
    setRevokingInvitationId(invitation.id);
    try {
      await revokeInvitation(invitation.id);
      void message.success("邀请已撤销");
      setListRevision((value) => value + 1);
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setRevokingInvitationId("");
    }
  }

  const grantColumns: TableColumnsType<AggregatedGrant> = [
    {
      title: "用户",
      dataIndex: "username",
      width: 160,
      render: (value: string) => <span className="break-text">{value}</span>,
    },
    {
      title: "权限",
      key: "permission",
      width: 90,
      render: (_, item) => {
        const hasWrite = item.wholeWrite || item.writeDocuments.length > 0;
        return <Tag color={hasWrite ? "blue" : "default"}>{hasWrite ? "读写" : "只读"}</Tag>;
      },
    },
    {
      title: "范围",
      key: "scope",
      render: (_, item) => {
        const files = [
          ...item.writeDocuments.map((name) => ({ name, write: true })),
          ...item.readDocuments.map((name) => ({ name, write: false })),
        ];
        return (
          <div className="scope-files">
            <span>{item.wholeWrite ? "全库" : item.wholeRead ? "全库（只读）" : ""}</span>
            {files.length > 0 && !item.wholeWrite && (
              <>
                <Tag>指定文件 {files.length} 个</Tag>
                <span className="secondary break-text">
                  {files.map((file) => `${file.name}${file.write ? "（可写）" : ""}`).join("、")}
                </span>
              </>
            )}
          </div>
        );
      },
    },
    {
      title: "授权人",
      dataIndex: "granted_by_username",
      width: 130,
      render: (value: string) => <span className="break-text">{value || "—"}</span>,
    },
    {
      title: "更新时间",
      dataIndex: "updated_at",
      width: 180,
      render: (value: string) => <span className="secondary">{formatDateTime(value)}</span>,
    },
    {
      title: "操作",
      key: "actions",
      width: 90,
      render: (_, item) => (
        <Popconfirm
          title="撤销授权"
          description={`将撤销 ${item.username} 在该知识库的全部授权（${item.wholeWrite ? "全库读写" : item.wholeRead ? "全库只读" : `${item.readDocuments.length + item.writeDocuments.length} 个文件`}）。`}
          okText="撤销"
          cancelText="取消"
          okButtonProps={{ danger: true }}
          onConfirm={() => void revokeGrantFor(item)}
        >
          <Tooltip title="撤销授权">
            <Button
              danger
              type="text"
              size="small"
              aria-label={`撤销 ${item.username} 的授权`}
              icon={<DeleteOutlined />}
              loading={revokingGrantUser === item.user_id}
            />
          </Tooltip>
        </Popconfirm>
      ),
    },
  ];

  const invitationColumns: TableColumnsType<InvitationView> = [
    {
      title: "被邀请人",
      dataIndex: "invitee_username",
      width: 150,
      render: (value: string) => <span className="break-text">{value}</span>,
    },
    {
      title: "权限",
      dataIndex: "permission",
      width: 90,
      render: (value: PermissionLevel) => (
        <Tag color={value === "write" ? "blue" : "default"}>{permissionLabels[value] || value}</Tag>
      ),
    },
    {
      title: "范围",
      key: "scope",
      width: 130,
      render: (_, invitation) => <InvitationScope invitation={invitation} />,
    },
    {
      title: "状态",
      key: "status",
      width: 90,
      render: (_, invitation) => {
        const status = invitationStatus(invitation);
        return (
          <Tag color={invitationStatusColors[status] || "default"}>
            {invitationStatusLabels[status] || status}
          </Tag>
        );
      },
    },
    {
      title: "邀请人",
      dataIndex: "inviter_username",
      width: 130,
      render: (value: string) => <span className="break-text">{value}</span>,
    },
    {
      title: "失效时间",
      dataIndex: "expires_at",
      width: 180,
      render: (value: string) => <span className="secondary">{formatDateTime(value)}</span>,
    },
    {
      title: "操作",
      key: "actions",
      width: 90,
      render: (_, invitation) => invitationStatus(invitation) === "pending" ? (
        <Popconfirm
          title="撤销邀请"
          description={`确定撤销发给 ${invitation.invitee_username} 的邀请吗？链接将立即失效。`}
          okText="撤销"
          cancelText="取消"
          okButtonProps={{ danger: true }}
          onConfirm={() => void revokeInvitationFor(invitation)}
        >
          <Tooltip title="撤销邀请">
            <Button
              danger
              type="text"
              size="small"
              aria-label={`撤销 ${invitation.invitee_username} 的邀请`}
              icon={<DeleteOutlined />}
              loading={revokingInvitationId === invitation.id}
            />
          </Tooltip>
        </Popconfirm>
      ) : "—",
    },
  ];

  const selectedAccessLabel = selectedAccess
    ? selectedAccess.owner
      ? "创建者"
      : roleLabel(selectedAccess.role)
    : "";

  return (
    <section aria-label="权限管理页面">
      <div className="page-heading">
        <h1>权限管理</h1>
        <div className="toolbar">
          <Tooltip title="刷新权限与邀请">
            <Button
              aria-label="刷新权限与邀请"
              icon={<ReloadOutlined spin={loadingNamespaces || grantsLoading || invitationsLoading} />}
              onClick={() => {
                setRevision((value) => value + 1);
                setListRevision((value) => value + 1);
              }}
            />
          </Tooltip>
          <Tooltip title={canShare ? "创建邀请" : "没有该知识库的邀请权限"}>
            <Button
              type="primary"
              icon={<PlusOutlined />}
              disabled={!selected || !canShare}
              onClick={openInviteDialog}
            >
              创建邀请
            </Button>
          </Tooltip>
        </div>
      </div>
      {role !== "admin" && (
        <Alert
          className="page-alert"
          type="info"
          showIcon
          title="VIP 权限"
          description="VIP 只能管理自己创建的知识库的授权与邀请；其他被邀请的知识库仅可按授权使用。"
        />
      )}
      {namespacesError && (
        <Alert className="page-alert" type="error" showIcon title="知识库列表加载失败" description={namespacesError} />
      )}
      <div className="permissions-toolbar">
        <span className="permissions-label"><SafetyOutlined /> 知识库</span>
        <Select
          aria-label="选择知识库"
          className="permissions-select"
          value={selected}
          loading={loadingNamespaces}
          onChange={setSelected}
          placeholder={loadingNamespaces ? "正在加载…" : "选择要管理的知识库"}
          options={namespaces.map((namespace) => ({ value: namespace, label: namespace }))}
          notFoundContent={loadingNamespaces ? <Spin size="small" /> : "暂无可管理的知识库"}
        />
        {selectedAccess && (
          <Space size={6} wrap>
            <Tag color="blue">{selectedAccessLabel}</Tag>
            <span className="secondary">
              {selectedAccess.scope.whole_write
                ? "全库读写"
                : selectedAccess.scope.whole_read
                  ? "全库只读"
                  : "文件级权限"}
            </span>
          </Space>
        )}
      </div>
      {loadingNamespaces && !namespaces.length ? (
        <Spin tip="正在加载可管理的知识库…" />
      ) : !loadingNamespaces && !namespaces.length ? (
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={
          role === "admin"
            ? "暂无知识库，创建后即可在这里管理授权与邀请。"
            : "你还没有创建的知识库；创建后即可在这里管理授权与邀请。"
        }>
          <Button type="primary" onClick={onCreateKnowledgeBase}>创建知识库</Button>
        </Empty>
      ) : (
        <>
          <div className="section-heading">
            <h2><TeamOutlined /> 授权列表</h2>
            <span className="secondary">{aggregated.length} 位用户</span>
          </div>
          {grantsError && (
            <Alert className="page-alert" type="error" showIcon title="授权列表加载失败" description={grantsError} />
          )}
          <Table<AggregatedGrant>
            className="permissions-table"
            rowKey="user_id"
            dataSource={aggregated}
            loading={grantsLoading}
            size="middle"
            scroll={{ x: 860 }}
            pagination={{ pageSize: 10, hideOnSinglePage: true }}
            locale={{
              emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有授权记录，可创建邀请让其他用户加入。" />,
            }}
            columns={grantColumns}
          />
          <div className="section-heading section-heading-spaced">
            <h2>邀请记录</h2>
            <span className="secondary">
              {pendingInvitations > 0 ? `${pendingInvitations} 个待处理` : `${invitations.length} 条记录`}
            </span>
          </div>
          {invitationsError && (
            <Alert className="page-alert" type="error" showIcon title="邀请记录加载失败" description={invitationsError} />
          )}
          <Table<InvitationView>
            className="permissions-table"
            rowKey="id"
            dataSource={invitations}
            loading={invitationsLoading}
            size="middle"
            scroll={{ x: 940 }}
            pagination={{ pageSize: 10, hideOnSinglePage: true }}
            locale={{
              emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有邀请记录。" />,
            }}
            columns={invitationColumns}
          />
        </>
      )}
      <Modal
        title={`邀请成员${selected ? " · " + selected : ""}`}
        open={inviteOpen}
        width={560}
        forceRender
        onCancel={() => {
          if (!creating) setInviteOpen(false);
        }}
        closable={!creating}
        mask={{ closable: !creating }}
        footer={created
          ? [
              <Button key="again" disabled={creating} onClick={() => {
                setCreated(undefined);
                setCopied(false);
                setInviteError("");
                form.resetFields();
                form.setFieldsValue({ permission: "read", scope: "all", document_names: [] });
                if (selected && documentsFor !== selected) void loadDocuments(selected);
              }}>继续邀请</Button>,
              <Button key="close" type="primary" onClick={() => setInviteOpen(false)}>关闭</Button>,
            ]
          : [
              <Button key="cancel" disabled={creating} onClick={() => setInviteOpen(false)}>取消</Button>,
              <Button key="submit" type="primary" icon={<PlusOutlined />} loading={creating}
                onClick={() => void form.validateFields().then((values) => submitInvite(values)).catch(() => {})}>
                创建邀请
              </Button>,
            ]}
      >
        {created ? (
          <div className="invite-created">
            <Alert
              type="success"
              showIcon
              title="邀请已创建，已向该用户推送邀请消息"
              description={`请把下面的链接发送给 ${created.invitation.invitee_username}；对方登录自己的账号后打开链接即可接受。`}
            />
            <div className="invite-link-box">
              <code>{created.link}</code>
              <Button icon={<CopyOutlined />} onClick={() => void copyLink(created.link)}>
                {copied ? "已复制" : "复制链接"}
              </Button>
            </div>
            <div className="secondary">
              链接中的 token 只会显示一次，请立即复制并妥善保存。邀请将在 {formatDateTime(created.invitation.expires_at)} 失效。
            </div>
          </div>
        ) : (
          <div className="invite-dialog">
            {inviteError && <Alert className="page-alert" type="error" showIcon title="邀请创建失败" description={inviteError} />}
            {usersError && (
              <Alert className="page-alert" type="error" showIcon title="用户列表加载失败" description={usersError}
                action={<Button size="small" onClick={() => void loadUsers()}>重试</Button>} />
            )}
            <Form form={form} layout="vertical" requiredMark={false}
              disabled={creating}
              onFinish={(values) => void submitInvite(values)}>
              <Form.Item name="username" label="用户名" rules={[{ required: true, message: "请选择被邀请的用户" }]}>
                <Select
                  showSearch
                  optionFilterProp="label"
                  loading={usersLoading}
                  placeholder={usersLoading ? "正在加载用户…" : "选择要邀请的用户"}
                  options={users
                    .filter((user) => user.id !== currentUserId)
                    .map((user) => ({ value: user.username, label: `${user.username}（${roleLabel(user.role)}）` }))}
                  notFoundContent={usersLoading ? <Spin size="small" /> : "没有可邀请的用户"}
                />
              </Form.Item>
              <Form.Item name="permission" label="权限" rules={[{ required: true, message: "请选择权限" }]}>
                <Radio.Group
                  optionType="button"
                  buttonStyle="solid"
                  options={[
                    { value: "read", label: permissionLabels.read },
                    { value: "write", label: permissionLabels.write },
                  ]}
                />
              </Form.Item>
              <Form.Item name="scope" label="范围" rules={[{ required: true, message: "请选择授权范围" }]}>
                <Radio.Group
                  optionType="button"
                  buttonStyle="solid"
                  options={[
                    { value: "all", label: "全库" },
                    { value: "files", label: "指定文件" },
                  ]}
                />
              </Form.Item>
              {scopeValue === "files" && (
                <Form.Item name="document_names" label="文件" rules={[{ required: true, message: "请至少选择一个文件" }]}>
                  <Select
                    mode="multiple"
                    showSearch
                    optionFilterProp="label"
                    loading={documentsLoading}
                    placeholder={documentsLoading ? "正在加载文件…" : "选择要授权的文件"}
                    options={documents.map((document) => ({ value: document.name, label: document.name }))}
                    notFoundContent={documentsLoading ? <Spin size="small" /> : "该知识库暂无文件"}
                  />
                </Form.Item>
              )}
              {scopeValue === "files" && documentsError && (
                <Alert className="page-alert" type="error" showIcon title="文件列表加载失败" description={documentsError}
                  action={<Button size="small" onClick={() => selected && void loadDocuments(selected)}>重试</Button>} />
              )}
              <div className="secondary">
                邀请创建后会推送站内通知；被邀请人登录自己的账号并打开邀请链接后才能接受。
              </div>
            </Form>
          </div>
        )}
      </Modal>
    </section>
  );
}
