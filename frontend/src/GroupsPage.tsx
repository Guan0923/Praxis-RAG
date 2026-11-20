import { useCallback, useEffect, useState } from "react";
import {
  Alert,
  App,
  Button,
  Descriptions,
  Empty,
  Form,
  Input,
  Modal,
  Popconfirm,
  Select,
  Space,
  Spin,
  Table,
  Tag,
  Tooltip,
  type TableColumnsType,
} from "antd";
import {
  CheckOutlined,
  CloseOutlined,
  CopyOutlined,
  DeleteOutlined,
  LinkOutlined,
  PlusOutlined,
  ReloadOutlined,
  TeamOutlined,
  UsergroupDeleteOutlined,
} from "@ant-design/icons";
import {
  acceptGroupInvitation,
  createGroup,
  createGroupInvitation,
  declineGroupInvitation,
  dissolveGroup,
  errorText,
  fetchGroupDetail,
  fetchGroupInvitationInbox,
  fetchGroups,
  fetchInvitableUsers,
  formatDateTime,
  groupInvitationLink,
  groupInvitationStatusLabels,
  groupStatusLabels,
  invitationStatusColors,
  previewGroupInvitation,
  removeGroupMember,
  leaveGroup,
  roleLabel,
  revokeGroupInvitation,
  type GroupInvitationView,
  type GroupMemberView,
  type GroupView,
  type InviteUser,
  type Role,
} from "./api";
import { parseInviteToken } from "./navigation";

// groupInvitationStatus computes the effective display status: a pending
// invitation whose TTL elapsed shows as expired.
export function groupInvitationStatus(invitation: GroupInvitationView) {
  if (invitation.status === "pending" && invitation.expires_at) {
    const expires = Date.parse(invitation.expires_at);
    if (!Number.isNaN(expires) && expires <= Date.now()) return "expired";
  }
  return invitation.status;
}

// invitationStatusTag renders one group invitation's effective status tag.
export function groupInvitationStatusTag(invitation: GroupInvitationView) {
  const status = groupInvitationStatus(invitation);
  return (
    <Tag color={invitationStatusColors[status] || "default"}>
      {groupInvitationStatusLabels[status] || status}
    </Tag>
  );
}

// GroupsPage is the 用户组 Console page: vip/admin create groups and invite
// members; every account reviews its invitations (accept/decline) and the
// groups it created or joined.
export function GroupsPage({
  role,
  onOpenInvite,
}: {
  role: Role;
  onOpenInvite: (token: string) => void;
}) {
  const { message } = App.useApp();
  const [form] = Form.useForm<{ name: string }>();

  const [owned, setOwned] = useState<GroupView[]>([]);
  const [joined, setJoined] = useState<GroupView[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const [inbox, setInbox] = useState<GroupInvitationView[]>([]);
  const [inboxLoading, setInboxLoading] = useState(false);
  const [handlingToken, setHandlingToken] = useState("");
  const [linkText, setLinkText] = useState("");
  const [linkError, setLinkError] = useState("");

  const [detail, setDetail] = useState<GroupView | null>(null);
  const [members, setMembers] = useState<GroupMemberView[]>([]);
  const [detailInvitations, setDetailInvitations] = useState<GroupInvitationView[]>([]);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState("");

  const [users, setUsers] = useState<InviteUser[]>([]);
  const [usersLoading, setUsersLoading] = useState(false);
  const [inviteOpen, setInviteOpen] = useState(false);
  const [invitingFor, setInvitingFor] = useState<GroupView | null>(null);
  const [inviteForm] = Form.useForm<{ username: string }>();
  const [inviting, setInviting] = useState(false);
  const [inviteError, setInviteError] = useState("");
  const [created, setCreated] = useState<{ path: string; invitee: string; expiresAt: string } | null>(null);
  const [copied, setCopied] = useState(false);

  const [createOpen, setCreateOpen] = useState(false);
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState("");

  const [removingUser, setRemovingUser] = useState("");
  const [revokingInvitationId, setRevokingInvitationId] = useState("");

  const mayManage = role === "admin" || role === "vip";

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const data = await fetchGroups();
      setOwned(data.owned);
      setJoined(data.joined);
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setLoading(false);
    }
  }, []);

  const loadInbox = useCallback(async () => {
    setInboxLoading(true);
    try {
      setInbox(await fetchGroupInvitationInbox());
    } catch {
      setInbox([]);
    } finally {
      setInboxLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
    void loadInbox();
  }, [load, loadInbox]);

  async function openDetail(group: GroupView) {
    setDetail(group);
    setDetailError("");
    setDetailLoading(true);
    try {
      const data = await fetchGroupDetail(group.id);
      setDetail(data.group);
      setMembers(data.members);
      setDetailInvitations(data.invitations);
    } catch (cause) {
      setDetailError(errorText(cause));
    } finally {
      setDetailLoading(false);
    }
  }

  async function openInviteDialog(group: GroupView) {
    setInvitingFor(group);
    setCreated(null);
    setInviteError("");
    setCopied(false);
    inviteForm.resetFields();
    setInviteOpen(true);
    if (!users.length && !usersLoading) {
      setUsersLoading(true);
      try {
        setUsers(await fetchInvitableUsers());
      } catch {
        setUsers([]);
      } finally {
        setUsersLoading(false);
      }
    }
  }

  async function submitInvite(values: { username: string }) {
    if (!invitingFor || inviting) return;
    setInviting(true);
    setInviteError("");
    try {
      const result = await createGroupInvitation(invitingFor.id, values.username);
      setCreated({
        path: result.invite_path,
        invitee: result.invitation.invitee_username,
        expiresAt: result.invitation.expires_at,
      });
      void message.success("邀请已创建，已向该用户推送邀请消息");
      if (detail && detail.id === invitingFor.id) void openDetail(invitingFor);
    } catch (cause) {
      setInviteError(errorText(cause));
    } finally {
      setInviting(false);
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
      // Clipboard permission may be denied; fall back below.
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

  async function submitCreate(values: { name: string }) {
    if (creating) return;
    setCreating(true);
    setCreateError("");
    try {
      await createGroup(values.name.trim());
      void message.success("用户组已创建");
      setCreateOpen(false);
      form.resetFields();
      void load();
    } catch (cause) {
      setCreateError(errorText(cause));
    } finally {
      setCreating(false);
    }
  }

  async function removeMember(group: GroupView, member: GroupMemberView) {
    setRemovingUser(member.user_id);
    try {
      await removeGroupMember(group.id, member.user_id);
      void message.success(`已移除 ${member.username}`);
      if (detail && detail.id === group.id) void openDetail(group);
      void load();
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setRemovingUser("");
    }
  }

  async function leave(group: GroupView) {
    try {
      await leaveGroup(group.id);
      void message.success(`已退出 ${group.name}`);
      if (detail && detail.id === group.id) setDetail(null);
      void load();
    } catch (cause) {
      void message.error(errorText(cause));
    }
  }

  async function dissolve(group: GroupView) {
    try {
      await dissolveGroup(group.id);
      void message.success(`已解散 ${group.name}`);
      if (detail && detail.id === group.id) setDetail(null);
      void load();
    } catch (cause) {
      void message.error(errorText(cause));
    }
  }

  async function revokeInvitation(invitation: GroupInvitationView) {
    setRevokingInvitationId(invitation.id);
    try {
      await revokeGroupInvitation(invitation.id);
      void message.success("邀请已撤销");
      if (detail && detail.id === invitation.group_id) void openDetail(detail);
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setRevokingInvitationId("");
    }
  }

  async function handleInvite(link: string, action: "accept" | "decline") {
    const token = parseInviteToken(link);
    if (!token) {
      setLinkError("请粘贴完整的邀请链接或 token");
      return;
    }
    setHandlingToken(token);
    setLinkError("");
    try {
      const invitation = action === "accept"
        ? await acceptGroupInvitation(token)
        : await declineGroupInvitation(token);
      void message.success(
        action === "accept"
          ? `已加入 ${invitation.group_name}`
          : `已拒绝加入 ${invitation.group_name}`,
      );
      setLinkText("");
      await Promise.all([load(), loadInbox()]);
    } catch (cause) {
      setLinkError(errorText(cause));
    } finally {
      setHandlingToken("");
    }
  }

  const memberColumns: TableColumnsType<GroupMemberView> = [
    {
      title: "成员",
      dataIndex: "username",
      width: 104,
      render: (value: string) => <span className="break-text">{value}</span>,
    },
    {
      title: "角色",
      dataIndex: "role",
      width: 72,
      render: (value: Role) => <Tag>{roleLabel(value)}</Tag>,
    },
    {
      title: "加入时间",
      dataIndex: "joined_at",
      width: 112,
      render: (value: string) => <span className="secondary">{formatDateTime(value)}</span>,
    },
    {
      title: "操作",
      key: "actions",
      width: 64,
      render: (_, member) => detail && detail.owner_user_id === member.user_id ? "—" : (
        <Popconfirm
          title="移除成员"
          description={`确定把 ${member.username} 移出该用户组吗？其来自该组的权限将立即失效。`}
          okText="移除"
          cancelText="取消"
          okButtonProps={{ danger: true }}
          onConfirm={() => detail && void removeMember(detail, member)}
        >
          <Tooltip title="移除成员">
            <Button
              danger
              type="text"
              size="small"
              aria-label={`移除 ${member.username}`}
              icon={<DeleteOutlined />}
              loading={removingUser === member.user_id}
            />
          </Tooltip>
        </Popconfirm>
      ),
    },
  ];

  const invitationColumns: TableColumnsType<GroupInvitationView> = [
    {
      title: "被邀请人",
      dataIndex: "invitee_username",
      width: 96,
      render: (value: string) => <span className="break-text">{value}</span>,
    },
    {
      title: "状态",
      key: "status",
      width: 64,
      render: (_, invitation) => groupInvitationStatusTag(invitation),
    },
    {
      title: "失效时间",
      dataIndex: "expires_at",
      width: 112,
      render: (value: string) => <span className="secondary">{formatDateTime(value)}</span>,
    },
    {
      title: "操作",
      key: "actions",
      width: 64,
      render: (_, invitation) => groupInvitationStatus(invitation) === "pending" ? (
        <Popconfirm
          title="撤销邀请"
          description={`确定撤销发给 ${invitation.invitee_username} 的邀请吗？链接将立即失效。`}
          okText="撤销"
          cancelText="取消"
          okButtonProps={{ danger: true }}
          onConfirm={() => void revokeInvitation(invitation)}
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

  const pendingInbox = inbox.filter((item) => item.status === "pending");

  return (
    <section aria-label="用户组页面">
      <div className="page-heading">
        <h1>用户组</h1>
        <div className="toolbar">
          <Tooltip title="刷新用户组与邀请">
            <Button
              aria-label="刷新用户组与邀请"
              icon={<ReloadOutlined spin={loading || inboxLoading} />}
              onClick={() => { void load(); void loadInbox(); }}
            />
          </Tooltip>
          <Tooltip title={mayManage ? "创建用户组" : "需要 VIP 或管理员权限才能创建用户组"}>
            <Button
              type="primary"
              icon={<PlusOutlined />}
              disabled={!mayManage}
              onClick={() => { setCreateError(""); form.resetFields(); setCreateOpen(true); }}
            >
              创建用户组
            </Button>
          </Tooltip>
        </div>
      </div>
      {!mayManage && (
        <Alert
          className="page-alert"
          type="info"
          showIcon
          title="用户组"
          description="普通账号可以查看并接受邀请、查看已加入的组；创建用户组和邀请成员需要 VIP 或管理员角色。"
        />
      )}
      {error && (
        <Alert className="page-alert" type="error" showIcon title="用户组加载失败" description={error} />
      )}

      <div className="section-heading section-heading-spaced">
        <h2><TeamOutlined /> 我创建的用户组</h2>
        <span className="secondary">{owned.length} 个组</span>
      </div>
      {loading && !owned.length && !joined.length ? (
        <Spin tip="正在加载用户组…" />
      ) : !loading && !owned.length ? (
        mayManage ? (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有创建用户组；创建后即可邀请成员并授权知识库。">
            <Button type="primary" onClick={() => { setCreateError(""); form.resetFields(); setCreateOpen(true); }}>
              创建用户组
            </Button>
          </Empty>
        ) : (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无内容" />
        )
      ) : (
        <Table<GroupView>
          className="permissions-table"
          rowKey="id"
          dataSource={owned}
          loading={loading}
          size="middle"
          scroll={{ x: 600 }}
          pagination={{ pageSize: 10, hideOnSinglePage: true }}
          locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无内容" /> }}
          columns={[
            {
              title: "组名",
              dataIndex: "name",
              render: (value: string, group) => (
                <Space size={6}>
                  <span className="break-text">{value}</span>
                  {group.status === "dissolved" && (
                    <Tag>{groupStatusLabels.dissolved}</Tag>
                  )}
                </Space>
              ),
            },
            {
              title: "成员数",
              dataIndex: "member_count",
              width: 64,
              render: (value: number) => value ?? 0,
            },
            {
              title: "创建时间",
              dataIndex: "created_at",
              width: 112,
              render: (value: string) => <span className="secondary">{formatDateTime(value)}</span>,
            },
            {
              title: "操作",
              key: "actions",
              width: 148,
              render: (_, group) => (
                <Space size={4}>
                  <Button
                    size="small"
                    icon={<TeamOutlined />}
                    disabled={group.status === "dissolved"}
                    onClick={() => void openDetail(group)}
                  >
                    管理成员
                  </Button>
                  {group.status === "active" && (
                    <>
                      <Popconfirm
                        title="解散用户组"
                        description={`解散后成员立即失效、待接受邀请作废、相关知识库组授权同时清除，不可恢复。`}
                        okText="解散"
                        cancelText="取消"
                        okButtonProps={{ danger: true }}
                        onConfirm={() => void dissolve(group)}
                      >
                        <Button
                          danger
                          size="small"
                          icon={<UsergroupDeleteOutlined />}
                          aria-label={`解散 ${group.name}`}
                        >
                          解散
                        </Button>
                      </Popconfirm>
                    </>
                  )}
                </Space>
              ),
            },
          ]}
        />
      )}

      <div className="section-heading section-heading-spaced">
        <h2>我加入的用户组</h2>
        <span className="secondary">{joined.length} 个组</span>
      </div>
      <Table<GroupView>
        className="permissions-table"
        rowKey="id"
        dataSource={joined}
        loading={loading}
        size="middle"
        scroll={{ x: 540 }}
        pagination={{ pageSize: 10, hideOnSinglePage: true }}
        locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有加入任何用户组；可让组创建者邀请你。" /> }}
        columns={[
          {
            title: "组名",
            dataIndex: "name",
            render: (value: string) => <span className="break-text">{value}</span>,
          },
          {
            title: "创建者",
            dataIndex: "owner_username",
            width: 88,
            render: (value: string) => <span className="break-text">{value || "—"}</span>,
          },
          {
            title: "成员数",
            dataIndex: "member_count",
            width: 64,
            render: (value: number) => value ?? 0,
          },
          {
            title: "加入时间",
            key: "joined",
            width: 112,
            render: () => <span className="secondary">—</span>,
          },
          {
            title: "操作",
            key: "actions",
            width: 64,
            render: (_, group) => (
              <Popconfirm
                title="退出用户组"
                description="退出后来自该用户组的权限立即失效。"
                okText="退出"
                cancelText="取消"
                okButtonProps={{ danger: true }}
                onConfirm={() => void leave(group)}
              >
                <Button danger size="small" aria-label={`退出 ${group.name}`}>退出</Button>
              </Popconfirm>
            ),
          },
        ]}
      />

      <div className="section-heading section-heading-spaced">
        <h2>我的组邀请</h2>
        <span className="secondary">
          {pendingInbox.length > 0 ? `${pendingInbox.length} 个待处理` : `${inbox.length} 条记录`}
        </span>
      </div>
      <Table<GroupInvitationView>
        className="permissions-table"
        rowKey="id"
        dataSource={inbox}
        loading={inboxLoading}
        size="middle"
        scroll={{ x: 600 }}
        pagination={{ pageSize: 10, hideOnSinglePage: true }}
        locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无组邀请。" /> }}
        columns={[
          {
            title: "用户组",
            dataIndex: "group_name",
            width: 104,
            render: (value: string) => <span className="break-text">{value}</span>,
          },
          {
            title: "邀请人",
            dataIndex: "inviter_username",
            width: 88,
            render: (value: string) => <span className="break-text">{value || "—"}</span>,
          },
          {
            title: "状态",
            key: "status",
            width: 64,
            render: (_, invitation) => groupInvitationStatusTag(invitation),
          },
          {
            title: "失效时间",
            dataIndex: "expires_at",
            width: 112,
            render: (value: string) => <span className="secondary">{formatDateTime(value)}</span>,
          },
          {
            title: "操作",
            key: "actions",
            width: 104,
            render: (_, invitation) => groupInvitationStatus(invitation) === "pending" ? (
              <span className="secondary">接受需要邀请链接（仅邀请人持有），请在下方粘贴链接</span>
            ) : "—",
          },
        ]}
      />
      <div className="invite-link-entry">
        <Space.Compact style={{ width: 480 }}>
          <Input
            allowClear
            aria-label="粘贴用户组邀请链接"
            placeholder="粘贴用户组邀请链接（#/group-invite?token=…）"
            value={linkText}
            onChange={(event) => setLinkText(event.target.value)}
            onPressEnter={() => void handleInvite(linkText, "accept")}
          />
          <Button
            icon={<LinkOutlined />}
            loading={Boolean(handlingToken)}
            onClick={() => void handleInvite(linkText, "accept")}
          >
            打开邀请
          </Button>
        </Space.Compact>
        {linkError && <div className="secondary" role="alert">{linkError}</div>}
      </div>

      <Modal
        title={detail ? `成员 · ${detail.name}` : "成员"}
        open={Boolean(detail)}
        width={760}
        footer={null}
        onCancel={() => setDetail(null)}
      >
        {detailError && (
          <Alert className="page-alert" type="error" showIcon title="成员加载失败" description={detailError} />
        )}
        {detail && (
          <>
            <Descriptions size="small" column={3} style={{ marginBottom: 12 }}>
              <Descriptions.Item label="组名">{detail.name}</Descriptions.Item>
              <Descriptions.Item label="创建者">{detail.owner_username}</Descriptions.Item>
              <Descriptions.Item label="成员数">{members.length}</Descriptions.Item>
            </Descriptions>
            <Space style={{ marginBottom: 12 }}>
              <Button
                size="small"
                icon={<PlusOutlined />}
                disabled={!mayManage || detail.status === "dissolved"}
                onClick={() => void openInviteDialog(detail)}
              >
                邀请成员
              </Button>
            </Space>
            <Table<GroupMemberView>
              rowKey="user_id"
              dataSource={members}
              loading={detailLoading}
              size="small"
              pagination={{ pageSize: 8, hideOnSinglePage: true }}
              locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="该组还没有成员。" /> }}
              columns={memberColumns}
            />
            {detailInvitations.length > 0 && (
              <>
                <div className="section-heading" style={{ marginTop: 16 }}>
                  <h2>组邀请记录</h2>
                </div>
                <Table<GroupInvitationView>
                  rowKey="id"
                  dataSource={detailInvitations}
                  size="small"
                  pagination={false}
                  columns={invitationColumns}
                />
              </>
            )}
          </>
        )}
      </Modal>

      <Modal
        title={invitingFor ? `邀请成员 · ${invitingFor.name}` : "邀请成员"}
        open={inviteOpen}
        width={560}
        forceRender
        onCancel={() => { if (!inviting) setInviteOpen(false); }}
        closable={!inviting}
        footer={created ? [
          <Button key="again" disabled={inviting} onClick={() => {
            setCreated(null);
            setCopied(false);
            setInviteError("");
            inviteForm.resetFields();
          }}>继续邀请</Button>,
          <Button key="close" type="primary" onClick={() => setInviteOpen(false)}>关闭</Button>,
        ] : [
          <Button key="cancel" disabled={inviting} onClick={() => setInviteOpen(false)}>取消</Button>,
          <Button key="submit" type="primary" icon={<PlusOutlined />} loading={inviting}
            onClick={() => void inviteForm.validateFields().then((values) => submitInvite(values)).catch(() => {})}>
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
              description={`请把下面的链接发送给 ${created.invitee}；对方登录自己的账号后打开链接即可接受。`}
            />
            <div className="invite-link-box">
              <code>{groupInvitationLink(created.path)}</code>
              <Button icon={<CopyOutlined />} onClick={() => void copyLink(groupInvitationLink(created.path))}>
                {copied ? "已复制" : "复制链接"}
              </Button>
            </div>
            <div className="secondary">
              链接中的 token 只会显示一次，请立即复制并妥善保存。邀请将在 {formatDateTime(created.expiresAt)} 失效。
            </div>
          </div>
        ) : (
          <div className="invite-dialog">
            {inviteError && <Alert className="page-alert" type="error" showIcon title="邀请创建失败" description={inviteError} />}
            <Form form={inviteForm} layout="vertical" requiredMark={false}
              disabled={inviting}
              onFinish={(values) => void submitInvite(values)}>
              <Form.Item name="username" label="用户名" rules={[{ required: true, message: "请选择被邀请的用户" }]}>
                <Select
                  showSearch
                  optionFilterProp="label"
                  loading={usersLoading}
                  placeholder={usersLoading ? "正在加载用户…" : "选择要邀请的用户"}
                  options={users.map((user) => ({
                    value: user.username, label: `${user.username}（${roleLabel(user.role)}）`,
                  }))}
                  notFoundContent={usersLoading ? <Spin size="small" /> : "没有可邀请的用户"}
                />
              </Form.Item>
              <div className="secondary">
                邀请创建后会推送站内通知；被邀请人登录自己的账号并打开邀请链接后才能接受。
              </div>
            </Form>
          </div>
        )}
      </Modal>

      <Modal
        title="创建用户组"
        open={createOpen}
        width={480}
        onCancel={() => { if (!creating) setCreateOpen(false); }}
        closable={!creating}
        footer={[
          <Button key="cancel" disabled={creating} onClick={() => setCreateOpen(false)}>取消</Button>,
          <Button key="submit" type="primary" icon={<PlusOutlined />} loading={creating}
            onClick={() => void form.validateFields().then((values) => submitCreate(values)).catch(() => {})}>
            创建
          </Button>,
        ]}
      >
        {createError && <Alert className="page-alert" type="error" showIcon title="创建失败" description={createError} />}
        <Form form={form} layout="vertical" requiredMark={false} disabled={creating}>
          <Form.Item name="name" label="组名" rules={[
            { required: true, message: "请输入用户组名称" },
            { max: 100, message: "组名最多 100 个字符" },
          ]}>
            <Input placeholder="例如 行业研究组" maxLength={100} />
          </Form.Item>
          <div className="secondary">
            创建后你是组创建者：可以邀请成员、移除成员、撤销邀请和解散组；降级为普通账号后仍可管理已有组，但不能再创建组或发新邀请。
          </div>
        </Form>
      </Modal>
    </section>
  );
}

// GroupInvitePage handles one membership invitation link: the invitee previews
// the group and the inviter, then accepts or declines.
export function GroupInvitePage({
  token,
  onHandled,
  onBack,
}: {
  token?: string;
  onHandled: () => void;
  onBack: () => void;
}) {
  const { message } = App.useApp();
  const [preview, setPreview] = useState<GroupInvitationView | null>(null);
  const [error, setError] = useState("");
  const [handling, setHandling] = useState(false);

  useEffect(() => {
    if (!token) return;
    let cancelled = false;
    setError("");
    void previewGroupInvitation(token)
      .then((invitation) => { if (!cancelled) setPreview(invitation); })
      .catch((cause) => { if (!cancelled) setError(errorText(cause)); });
    return () => { cancelled = true; };
  }, [token]);

  async function accept() {
    if (!token) return;
    setHandling(true);
    try {
      const invitation = await acceptGroupInvitation(token);
      void message.success(`已加入 ${invitation.group_name}`);
      onHandled();
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setHandling(false);
    }
  }

  async function decline() {
    if (!token) return;
    setHandling(true);
    try {
      const invitation = await declineGroupInvitation(token);
      void message.info(`已拒绝加入 ${invitation.group_name}`);
      onHandled();
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setHandling(false);
    }
  }

  if (!token) {
    return (
      <section aria-label="用户组邀请页面">
        <div className="page-heading">
          <Button aria-label="返回用户组" onClick={onBack}>返回</Button>
          <h1>用户组邀请</h1>
        </div>
        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="邀请链接缺少 token，请向邀请人索取完整链接。" />
      </section>
    );
  }

  return (
    <section aria-label="用户组邀请页面">
      <div className="page-heading">
        <Button aria-label="返回用户组" onClick={onBack}>返回</Button>
        <h1>用户组邀请</h1>
      </div>
      {error ? (
        <Alert className="page-alert" type="error" showIcon title="邀请无法打开" description={error}
          action={<Button size="small" onClick={onBack}>返回用户组</Button>} />
      ) : !preview ? (
        <Spin tip="正在解析邀请…" />
      ) : (
        <div className="invite-card">
          <Descriptions title="邀请详情" bordered column={2} size="small">
            <Descriptions.Item label="用户组">{preview.group_name}</Descriptions.Item>
            <Descriptions.Item label="邀请人">{preview.inviter_username}</Descriptions.Item>
            <Descriptions.Item label="被邀请人">{preview.invitee_username}</Descriptions.Item>
            <Descriptions.Item label="失效时间">{formatDateTime(preview.expires_at)}</Descriptions.Item>
          </Descriptions>
          <Space style={{ marginTop: 16 }}>
            <Button type="primary" icon={<CheckOutlined />} loading={handling} onClick={() => void accept()}>
              接受并加入
            </Button>
            <Button danger icon={<CloseOutlined />} disabled={handling} onClick={() => void decline()}>
              拒绝
            </Button>
          </Space>
          <div className="secondary" style={{ marginTop: 12 }}>
            接受后你立即成为该组成员：该组被授权的知识库权限（阅读/新增/修改/删除）会与你的个人授权取并集。
          </div>
        </div>
      )}
    </section>
  );
}
