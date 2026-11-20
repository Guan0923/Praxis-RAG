import { useCallback, useEffect, useRef, useState } from "react";
import {
  Alert,
  App,
  Button,
  Descriptions,
  Empty,
  Input,
  Spin,
  Table,
  Tag,
  Tooltip,
  type TableColumnsType,
} from "antd";
import {
  ArrowLeftOutlined,
  CheckOutlined,
  CloseOutlined,
  LinkOutlined,
  ReloadOutlined,
} from "@ant-design/icons";
import {
  acceptInvitation,
  declineInvitation,
  errorText,
  fetchInvitationInbox,
  formatDateTime,
  invitationStatus,
  invitationStatusColors,
  invitationStatusLabels,
  maskToText,
  permissionLabels,
  previewInvitation,
  type InvitationView,
} from "./api";
import { clearInviteToken, parseInviteToken } from "./navigation";

export function invitationStatusTag(invitation: InvitationView) {
  const status = invitationStatus(invitation);
  return (
    <Tag color={invitationStatusColors[status] || "default"}>
      {invitationStatusLabels[status] || status}
    </Tag>
  );
}

// InvitationAllowTag 渲染一条邀请允许的具体操作：掩码翻译成中文标签（例如
// 「阅读、修改」）；旧邀请没有掩码时回退到只读/读写投影。
export function InvitationAllowTag({ invitation }: { invitation: InvitationView }) {
  if (invitation.allow_mask !== undefined)
    return <Tag color={invitation.allow_mask ? "blue" : "default"}>{maskToText(invitation.allow_mask)}</Tag>;
  return (
    <Tag color={invitation.permission === "write" ? "blue" : "default"}>
      {permissionLabels[invitation.permission] || invitation.permission}
    </Tag>
  );
}

export function InvitationScope({ invitation }: { invitation: InvitationView }) {
  if (invitation.scope === "all") return <span>全库</span>;
  return (
    <Tooltip title={invitation.document_names.join("、")}>
      <Tag>指定文件 {invitation.document_names.length} 个</Tag>
    </Tooltip>
  );
}

// InvitationsPage is the in-app notification inbox: it lists pending and
// handled invitations. Accepting still requires the raw link token (only the
// inviter has it), so the page also accepts a pasted invitation link.
export function InvitationsPage({ onOpenInvite }: {
  onOpenInvite: (token: string) => void;
}) {
  const { message } = App.useApp();
  const [invitations, setInvitations] = useState<InvitationView[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [linkText, setLinkText] = useState("");
  const [linkError, setLinkError] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      setInvitations(await fetchInvitationInbox());
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setLoading(false);
    }
  }, []);
  useEffect(() => {
    void load();
  }, [load]);

  function openPastedLink() {
    const token = parseInviteToken(linkText);
    if (!token) {
      setLinkError("请输入邀请链接或其中的 token");
      return;
    }
    setLinkError("");
    setLinkText("");
    onOpenInvite(token);
  }

  const pending = invitations.filter((item) => invitationStatus(item) === "pending").length;
  const columns: TableColumnsType<InvitationView> = [
    {
      title: "知识库",
      dataIndex: "namespace",
      width: 116,
      render: (value: string) => <span className="break-text">{value}</span>,
    },
    {
      title: "邀请人",
      dataIndex: "inviter_username",
      width: 88,
      render: (value: string) => <span className="break-text">{value}</span>,
    },
    {
      title: "允许的操作",
      key: "allow_mask",
      width: 116,
      render: (_, invitation) => <InvitationAllowTag invitation={invitation} />,
    },
    {
      title: "范围",
      key: "scope",
      width: 88,
      render: (_, invitation) => <InvitationScope invitation={invitation} />,
    },
    {
      title: "状态",
      key: "status",
      width: 64,
      render: (_, invitation) => invitationStatusTag(invitation),
    },
    {
      title: "失效时间",
      dataIndex: "expires_at",
      width: 112,
      render: (value: string) => <span className="secondary">{formatDateTime(value)}</span>,
    },
    {
      title: "邀请时间",
      dataIndex: "created_at",
      width: 112,
      render: (value: string) => <span className="secondary">{formatDateTime(value)}</span>,
    },
  ];

  return (
    <section aria-label="邀请通知页面">
      <div className="page-heading">
        <div className="page-heading-title">
          <h1>邀请通知</h1>
          {pending > 0 && <Tag color="processing">{pending} 个待处理</Tag>}
        </div>
        <div className="toolbar">
          <Tooltip title="刷新邀请列表">
            <Button aria-label="刷新邀请列表" icon={<ReloadOutlined spin={loading} />} onClick={() => void load()} />
          </Tooltip>
        </div>
      </div>
      {error && (
        <Alert className="page-alert" type="error" showIcon title="邀请列表加载失败" description={error} />
      )}
      <div className="invite-link-opener">
        <Input
          aria-label="邀请链接"
          prefix={<LinkOutlined />}
          value={linkText}
          maxLength={512}
          allowClear
          placeholder="粘贴邀请链接，例如 https://…/console#/invite?token=…"
          onChange={(event) => {
            setLinkText(event.target.value);
            setLinkError("");
          }}
          onPressEnter={openPastedLink}
        />
        <Button type="primary" disabled={!linkText.trim()} onClick={openPastedLink}>
          打开邀请链接
        </Button>
      </div>
      {linkError && <Alert className="page-alert" type="warning" showIcon title={linkError} />}
      <Table<InvitationView>
        className="invitations-table"
        rowKey="id"
        dataSource={invitations}
        loading={loading}
        size="middle"
        pagination={{ pageSize: 10, hideOnSinglePage: true }}
        locale={{
          emptyText: (
            <Empty
              image={Empty.PRESENTED_IMAGE_SIMPLE}
              description={error ? "无法读取邀请" : "暂无邀请"}
            />
          ),
        }}
        columns={columns}
      />
    </section>
  );
}

// InvitePage is the login-aware invitation detail page behind
// #/invite?token=…. The raw token is only used in requests, never rendered.
export function InvitePage({
  token,
  authEnabled,
  onHandled,
  onOpenNamespace,
  onBack,
}: {
  token?: string;
  authEnabled: boolean;
  onHandled: () => void;
  onOpenNamespace: (namespace: string) => void;
  onBack: () => void;
}) {
  const { message } = App.useApp();
  const [invitation, setInvitation] = useState<InvitationView>();
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [accepting, setAccepting] = useState(false);
  const [declining, setDeclining] = useState(false);
  const sequence = useRef(0);

  useEffect(() => {
    const current = ++sequence.current;
    setInvitation(undefined);
    setError("");
    if (!authEnabled) {
      setError("当前服务未启用登录，邀请链接需要在启用账号登录的控制台中使用。");
      return;
    }
    if (!token) {
      setError("邀请链接无效或缺少 token，请向邀请人索取完整链接。");
      return;
    }
    setLoading(true);
    void previewInvitation(token)
      .then((data) => {
        if (sequence.current === current) setInvitation(data);
      })
      .catch((cause) => {
        if (sequence.current === current) setError(errorText(cause));
      })
      .finally(() => {
        if (sequence.current === current) setLoading(false);
      });
  }, [token, authEnabled]);

  async function accept() {
    if (!token || accepting || declining) return;
    setAccepting(true);
    try {
      const result = await acceptInvitation(token);
      setInvitation(result.invitation);
      clearInviteToken();
      void message.success(`已接受邀请，加入知识库 ${result.namespace}`);
      onHandled();
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setAccepting(false);
    }
  }

  async function decline() {
    if (!token || accepting || declining) return;
    setDeclining(true);
    try {
      const updated = await declineInvitation(token);
      setInvitation(updated);
      clearInviteToken();
      void message.success("已拒绝该邀请");
      onHandled();
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setDeclining(false);
    }
  }

  const status = invitation ? invitationStatus(invitation) : "";
  const pending = status === "pending";
  const expired = status === "expired";
  const described = invitation ? [
    { key: "namespace", label: "知识库", children: <span className="break-text">{invitation.namespace}</span> },
    { key: "inviter", label: "邀请人", children: <span className="break-text">{invitation.inviter_username}</span> },
    { key: "permission", label: "允许的操作", children: <InvitationAllowTag invitation={invitation} /> },
    {
      key: "scope",
      label: "范围",
      children: invitation.scope === "all"
        ? <span>全库</span>
        : <div className="scope-files">
            <div className="secondary">{invitation.document_names.length} 个文件</div>
            {invitation.document_names.map((name) => <Tag key={name} className="break-text">{name}</Tag>)}
          </div>,
    },
    { key: "status", label: "状态", children: invitationStatusTag(invitation) },
    { key: "created", label: "邀请时间", children: formatDateTime(invitation.created_at) },
    { key: "expires", label: "失效时间", children: formatDateTime(invitation.expires_at) },
    ...(invitation.accepted_at
      ? [{ key: "accepted", label: "接受时间", children: formatDateTime(invitation.accepted_at) }]
      : []),
  ] : [];

  return (
    <section aria-label="邀请详情页面">
      <div className="page-heading">
        <div className="page-heading-title">
          <h1>邀请详情</h1>
          {invitation && invitationStatusTag(invitation)}
        </div>
        <div className="toolbar">
          <Button icon={<ArrowLeftOutlined />} onClick={onBack}>返回邀请中心</Button>
        </div>
      </div>
      <div className="invite-detail">
        {loading && <Spin tip="正在读取邀请…" />}
        {error && <Alert className="page-alert" type="error" showIcon title="无法打开邀请" description={error} />}
        {invitation && (
          <>
            {pending && !expired && (
              <Alert
                className="page-alert"
                type="info"
                showIcon
                title="确认接受该邀请？"
                description="接受后你将在授权范围内访问该知识库。只有被邀请的账号可以接受。"
              />
            )}
            {expired && (
              <Alert
                className="page-alert"
                type="warning"
                showIcon
                title="邀请已过期"
                description="该邀请链接已超过有效期，请联系邀请人重新发送。"
              />
            )}
            {status === "accepted" && (
              <Alert
                className="page-alert"
                type="success"
                showIcon
                title="已接受邀请"
                description={`你已获得知识库 ${invitation.namespace} 的访问权限。`}
              />
            )}
            {status === "declined" && (
              <Alert className="page-alert" type="info" showIcon title="已拒绝该邀请" />
            )}
            {status === "revoked" && (
              <Alert className="page-alert" type="warning" showIcon title="邀请已被撤销" description="邀请人已撤销该邀请，请联系邀请人重新发送。" />
            )}
            <Descriptions className="invite-detail-fields" column={1} bordered size="middle" items={described} />
            <div className="toolbar invite-detail-actions">
              {pending && !expired && (
                <>
                  <Button type="primary" icon={<CheckOutlined />} loading={accepting} disabled={declining} onClick={() => void accept()}>
                    接受邀请
                  </Button>
                  <Button danger icon={<CloseOutlined />} loading={declining} disabled={accepting} onClick={() => void decline()}>
                    拒绝邀请
                  </Button>
                </>
              )}
              {status === "accepted" && (
                <Button onClick={() => onOpenNamespace(invitation.namespace)}>
                  前往知识库
                </Button>
              )}
            </div>
          </>
        )}
      </div>
    </section>
  );
}
