import { useCallback, useEffect, useState } from "react";
import {
  Alert,
  App,
  Button,
  Checkbox,
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
  ClearOutlined,
  CopyOutlined,
  DeleteOutlined,
  MinusCircleOutlined,
  PlusOutlined,
  ReloadOutlined,
  SafetyOutlined,
  SaveOutlined,
  TeamOutlined,
} from "@ant-design/icons";
import {
  api,
  clearKnowledgeBaseDeny,
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
  maskToText,
  normalizeMask,
  permissionBits,
  permissionLabels,
  PERM_CREATE,
  PERM_READ,
  revokeGrant,
  revokeInvitation,
  roleLabel,
  setKnowledgeBasePermission,
  type AccessView,
  type Connection,
  type CreateInvitationResult,
  type DocumentInfo,
  type GrantView,
  type InvitationView,
  type InviteUser,
  type Role,
  type ShareScope,
} from "./api";
import { InvitationScope } from "./InvitationsPage";

type InviteFormValues = {
  username: string;
  allow_mask: number;
  scope: ShareScope;
  document_names?: string[];
};

// TriStateValue is one operation's edit state: unset / allow / deny.
export type TriStateValue = "unset" | "allow" | "deny";

const triStateLabels: Record<TriStateValue, string> = {
  unset: "未设置",
  allow: "允许",
  deny: "拒绝",
};

// grantRuleKey 稳定标识一条规则（账号 × 文件名，""＝全库）。
export function grantRuleKey(userId: string, documentName: string) {
  return `${userId}\u0000${documentName}`;
}

// triStateOf 把一条规则的掩码解析为某个操作的三态值。
export function triStateOf(allow: number | undefined, deny: number | undefined, bit: number): TriStateValue {
  if ((deny || 0) & bit) return "deny";
  if ((allow || 0) & bit) return "allow";
  return "unset";
}

// masksOfTriStates 把每个操作的三态值折回 allow_mask / deny_mask 两个 0-15 掩码。
export function masksOfTriStates(states: Record<number, TriStateValue>) {
  let allow = 0;
  let deny = 0;
  for (const { bit } of permissionBits) {
    const state = states[bit];
    if (state === "allow") allow |= bit;
    else if (state === "deny") deny |= bit;
  }
  return { allow_mask: allow, deny_mask: deny };
}

// TriStateSelect is the per-operation control (未设置 / 允许 / 拒绝).
export function TriStateSelect({
  value,
  onChange,
  disabled,
  ariaLabel,
}: {
  value: TriStateValue;
  onChange: (value: TriStateValue) => void;
  disabled?: boolean;
  ariaLabel: string;
}) {
  return (
    <Select<TriStateValue>
      size="small"
      style={{ width: 92 }}
      disabled={disabled}
      aria-label={ariaLabel}
      value={value}
      onChange={onChange}
      popupMatchSelectWidth={false}
      options={(Object.keys(triStateLabels) as TriStateValue[]).map((key) => ({
        value: key,
        label: triStateLabels[key],
      }))}
    />
  );
}

// RuleMaskEditor renders one rule row's operation controls. Whole-library
// rules show create; file rules hide it (create only applies at the library
// level per the contract).
export function RuleMaskEditor({
  rule,
  states,
  onChange,
  saving,
  fileLevel,
}: {
  rule: { user_id: string; username: string; document_name: string };
  states: Record<number, TriStateValue>;
  onChange: (bit: number, value: TriStateValue) => void;
  saving: boolean;
  fileLevel: boolean;
}) {
  const bits = permissionBits.filter(({ bit }) => !fileLevel || bit !== PERM_CREATE);
  return (
    <div className="rule-mask-editor">
      <span className="rule-mask-title secondary">
        {rule.document_name || "全库"}
        {fileLevel ? "" : "（含新增）"}
      </span>
      <Space size={4} wrap>
        {bits.map(({ bit, label }) => (
          <label key={bit} className="rule-mask-item">
            <span className="rule-mask-op">{label}</span>
            <TriStateSelect
              ariaLabel={`${rule.username} ${rule.document_name || "全库"} ${label} 权限`}
              value={states[bit] ?? "unset"}
              disabled={saving}
              onChange={(value) => onChange(bit, value)}
            />
          </label>
        ))}
      </Space>
    </div>
  );
}

// triStatesFromMasks unfolds one rule's masks into per-operation tri-states.
export function triStatesFromMasks(allow?: number, deny?: number) {
  const states: Record<number, TriStateValue> = {};
  for (const { bit } of permissionBits) states[bit] = triStateOf(allow, deny, bit);
  return states;
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

  // 规则三态编辑态：key 为 grantRuleKey(user_id, document_name)，value 为该
  // 规则每个操作位的未设置/允许/拒绝。保存时折回 allow_mask / deny_mask 提交。
  const [triStates, setTriStates] = useState<Record<string, Record<number, TriStateValue>>>({});
  const [dirtyKeys, setDirtyKeys] = useState<Set<string>>(new Set());
  const [savingKey, setSavingKey] = useState("");
  const [revokingKey, setRevokingKey] = useState("");
  const [clearingKey, setClearingKey] = useState("");

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
          // Keep the namespace visible to its owner even after a membership
          // expires, so existing grants can still be reviewed and revoked. The
          // invite button itself follows can_share.
          return access.can_manage ? { namespace, access } : undefined;
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
      setTriStates({});
      setDirtyKeys(new Set());
      return;
    }
    let cancelled = false;
    setGrantsLoading(true);
    setGrantsError("");
    void fetchGrants(selected, connection)
      .then((rows) => {
        if (cancelled) return;
        setGrants(rows);
        // 每次重载都把服务端的掩码展开为三态草稿；未保存的本地改动会被丢弃。
        setTriStates(Object.fromEntries(rows.map((row) => [
          grantRuleKey(row.user_id, row.document_name),
          triStatesFromMasks(row.allow_mask, row.deny_mask),
        ])));
        setDirtyKeys(new Set());
      })
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

  const selectedAccess = selected ? accessByNamespace[selected] : undefined;
  const canShare = selectedAccess ? selectedAccess.can_share : true;
  const pendingInvitations = invitations.filter((item) => invitationStatus(item) === "pending").length;
  const dirtyCount = dirtyKeys.size;

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
    form.setFieldsValue({ allow_mask: PERM_READ, scope: "all", document_names: [] });
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
    const allowMask = normalizeMask(values.allow_mask || 0);
    if (values.scope === "files" && (allowMask & PERM_CREATE)) {
      setInviteError("文件级邀请不允许包含「新增」操作");
      return;
    }
    if (!allowMask) {
      setInviteError("请至少勾选一个允许的操作");
      return;
    }
    setCreating(true);
    setInviteError("");
    try {
      const result = await createInvitation(selected, {
        username: values.username,
        allow_mask: allowMask,
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

  async function revokeGrantFor(item: GrantView) {
    if (!selected) return;
    setRevokingKey(grantRuleKey(item.user_id, item.document_name));
    try {
      // DELETE 只撤销 allow：清除 allow_mask、保留 deny_mask。传文件名时只
      // 撤该文件的 allow，缺省撤销该用户全部 allow。
      await revokeGrant(selected, item.user_id, connection, item.document_name);
      void message.success(`已撤销 ${item.username} 的允许（${item.document_name || "全库"}）`);
      setListRevision((value) => value + 1);
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setRevokingKey("");
    }
  }

  async function clearDenyFor(item: GrantView) {
    if (!selected) return;
    setClearingKey(grantRuleKey(item.user_id, item.document_name));
    try {
      await clearKnowledgeBaseDeny(selected, item.user_id, item.document_name, connection);
      void message.success(`已清除 ${item.username} 的拒绝（${item.document_name || "全库"}）`);
      setListRevision((value) => value + 1);
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setClearingKey("");
    }
  }

  async function saveRule(item: GrantView) {
    if (!selected) return;
    const key = grantRuleKey(item.user_id, item.document_name);
    const states = triStates[key];
    if (!states) return;
    const { allow_mask, deny_mask } = masksOfTriStates(states);
    if (item.document_name && (allow_mask & PERM_CREATE)) {
      void message.error("文件级规则不允许包含「新增」操作（新增只作用于知识库级）");
      return;
    }
    setSavingKey(key);
    try {
      await setKnowledgeBasePermission(selected, item.user_id, {
        document_name: item.document_name,
        allow_mask,
        deny_mask,
      }, connection);
      void message.success(`已保存 ${item.username} 的规则（${item.document_name || "全库"}）`);
      setListRevision((value) => value + 1);
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setSavingKey("");
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

  const grantColumns: TableColumnsType<GrantView> = [
    {
      title: "用户",
      dataIndex: "username",
      width: 160,
      render: (value: string) => <span className="break-text">{value}</span>,
    },
    {
      title: "范围",
      key: "scope",
      width: 150,
      render: (_, item) => (
        <span className="break-text">
          {item.document_name
            ? <Tag className="break-text">{item.document_name}</Tag>
            : <Tag color="blue">全库</Tag>}
        </span>
      ),
    },
    {
      title: "操作权限（未设置 / 允许 / 拒绝）",
      key: "masks",
      render: (_, item) => {
        const key = grantRuleKey(item.user_id, item.document_name);
        const states = triStates[key] ?? triStatesFromMasks(item.allow_mask, item.deny_mask);
        return (
          <RuleMaskEditor
            rule={item}
            states={states}
            saving={savingKey === key}
            fileLevel={!!item.document_name}
            onChange={(bit, value) => {
              setTriStates((current) => ({
                ...current,
                [key]: { ...states, [bit]: value },
              }));
              setDirtyKeys((current) => new Set(current).add(key));
            }}
          />
        );
      },
    },
    {
      title: "当前允许",
      key: "allow",
      width: 170,
      render: (_, item) => (
        <span className="break-text">
          <Tag color={item.allow_mask ? "blue" : "default"}>允许：{maskToText(item.allow_mask, "无")}</Tag>
          {!!item.deny_mask && <Tag color="red">拒绝：{maskToText(item.deny_mask, "无")}</Tag>}
        </span>
      ),
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
      width: 210,
      render: (_, item) => {
        const key = grantRuleKey(item.user_id, item.document_name);
        const busy = savingKey === key || revokingKey === key || clearingKey === key;
        const dirty = dirtyKeys.has(key);
        return (
          <Space size={4} wrap>
            <Tooltip title={dirty ? "保存该规则的允许与拒绝" : "没有未保存的改动"}>
              <Button
                type="primary"
                size="small"
                icon={<SaveOutlined />}
                disabled={!dirty || busy}
                loading={savingKey === key}
                onClick={() => void saveRule(item)}
              >
                保存
              </Button>
            </Tooltip>
            <Popconfirm
              title="撤销允许"
              description={`将清除 ${item.username} 在${item.document_name ? `「${item.document_name}」` : "全库"}的全部允许位，拒绝位保持不变。`}
              okText="撤销允许"
              cancelText="取消"
              okButtonProps={{ danger: true }}
              onConfirm={() => void revokeGrantFor(item)}
            >
              <Tooltip title="撤销允许（保留拒绝）">
                <Button
                  danger
                  type="text"
                  size="small"
                  aria-label={`撤销 ${item.username} 在${item.document_name || "全库"}的允许`}
                  icon={<MinusCircleOutlined />}
                  loading={revokingKey === key}
                />
              </Tooltip>
            </Popconfirm>
            <Popconfirm
              title="清除拒绝"
              description={`将清除 ${item.username} 在${item.document_name ? `「${item.document_name}」` : "全库"}的全部拒绝位，允许位保持不变。`}
              okText="清除拒绝"
              cancelText="取消"
              onConfirm={() => void clearDenyFor(item)}
            >
              <Tooltip title="清除拒绝（保留允许）">
                <Button
                  type="text"
                  size="small"
                  aria-label={`清除 ${item.username} 在${item.document_name || "全库"}的拒绝`}
                  icon={<ClearOutlined />}
                  loading={clearingKey === key}
                />
              </Tooltip>
            </Popconfirm>
          </Space>
        );
      },
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
      title: "允许的操作",
      key: "allow_mask",
      width: 170,
      render: (_, invitation) => {
        // 优先展示掩码翻译的中文操作标签；旧邀请没有掩码时回退到只读/读写投影。
        if (invitation.allow_mask !== undefined && invitation.allow_mask !== 0)
          return <Tag color="blue">{maskToText(invitation.allow_mask)}</Tag>;
        if (invitation.allow_mask === 0)
          return <Tag color="default">未设置</Tag>;
        return (
          <Tag color={invitation.permission === "write" ? "blue" : "default"}>
            {permissionLabels[invitation.permission] || invitation.permission}
          </Tag>
        );
      },
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
          title="共享权限"
          description="只有自己拥有的知识库可以管理授权与邀请；Plus/Pro 会员与 VIP 均按此规则，其他被邀请的知识库仅可按授权使用。"
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
              {selectedAccess.scope.effective_library !== undefined
                ? `有效操作：${maskToText(selectedAccess.scope.effective_library, "无")}`
                : selectedAccess.scope.whole_write
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
            <h2><TeamOutlined /> 授权规则</h2>
            <span className="secondary">
              {grants.length} 条规则{dirtyCount > 0 ? ` · ${dirtyCount} 条未保存` : ""}
            </span>
          </div>
          {grantsError && (
            <Alert className="page-alert" type="error" showIcon title="授权列表加载失败" description={grantsError} />
          )}
          <Table<GrantView>
            className="permissions-table"
            rowKey={(record) => grantRuleKey(record.user_id, record.document_name)}
            dataSource={grants}
            loading={grantsLoading}
            size="middle"
            scroll={{ x: 1080 }}
            pagination={{ pageSize: 10, hideOnSinglePage: true }}
            locale={{
              emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="还没有授权记录，可创建邀请或直接新增规则。" />,
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
                form.setFieldsValue({ allow_mask: PERM_READ, scope: "all", document_names: [] });
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
              <Form.Item name="allow_mask" label="允许的操作" initialValue={PERM_READ}
                rules={[{
                  validator: (_, value: number | undefined) => value
                    ? Promise.resolve()
                    : Promise.reject(new Error("请至少勾选一个操作")),
                }]}>
                <Checkbox.Group
                  options={(scopeValue === "files"
                    ? permissionBits.filter(({ bit }) => bit !== PERM_CREATE)
                    : permissionBits
                  ).map(({ bit, label }) => ({ value: bit, label }))}
                  onChange={(checked) => {
                    // scope=files 时不允许带 create 位：切换范围时立即剪掉。
                    const next = normalizeMask(
                      Array.isArray(checked) ? (checked as number[]).reduce((mask, bit) => mask | bit, 0) : 0,
                    );
                    form.setFieldValue(
                      "allow_mask",
                      scopeValue === "files" ? next & ~PERM_CREATE : next,
                    );
                  }}
                />
              </Form.Item>
              {scopeValue === "files" && (
                <div className="secondary">文件级授权不包含「新增」操作；新增只在全库范围生效。</div>
              )}
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
