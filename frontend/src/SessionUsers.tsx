import { useEffect, useState } from "react";
import { Alert, App, Button, Input, Popconfirm, Select, Space, Table, Tag } from "antd";
import { ReloadOutlined } from "@ant-design/icons";
import { api, errorText, formatDateTime, roleLabel, type Role } from "./api";

const presenceLabels = { online: "在线", idle: "暂离", offline: "离线" };
type Presence = keyof typeof presenceLabels;
type Account = {
  id: string; username: string; role: Role; enabled: boolean; presence: Presence;
  sessions: number; last_seen_at: string | null;
};

export function SessionUsers({ currentUserId }: { currentUserId: string }) {
  const { message } = App.useApp();
  const [users, setUsers] = useState<Account[]>([]);
  const [revision, setRevision] = useState(0);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [pending, setPending] = useState("");
  const [name, setName] = useState("");
  const [presence, setPresence] = useState<Presence>();

  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    async function refresh() {
      try {
        const page = await api<{ users: Account[] }>("/v1/admin/session-users", undefined,
          { signal: controller.signal, cache: "no-store" });
        if (!controller.signal.aborted) { setUsers(page.users); setError(""); }
      } catch (cause) {
        if (!controller.signal.aborted) setError(errorText(cause));
      } finally {
        if (!controller.signal.aborted) {
          setLoading(false);
          timer = setTimeout(refresh, 30_000);
        }
      }
    }
    void refresh();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [revision]);

  async function forceLogout(user: Account) {
    setPending(user.id);
    try {
      const result = await api<{ revoked_sessions: number }>(
        `/v1/admin/users/${encodeURIComponent(user.id)}/force-logout`, undefined, { method: "POST" });
      void message.success(result.revoked_sessions
        ? `已撤销 ${user.username} 的 ${result.revoked_sessions} 个登录会话` : "该账号已无有效会话");
      setRevision(value => value + 1);
    } catch (cause) { void message.error(errorText(cause)); }
    finally { setPending(""); }
  }

  return <section aria-label="用户会话">
    <Space wrap>
      <Input aria-label="搜索用户" placeholder="用户名或账号 ID" allowClear value={name} onChange={e => setName(e.target.value)} />
      <Select<Presence> aria-label="在线状态" placeholder="全部状态" allowClear value={presence} onChange={setPresence}
        style={{ minWidth: 110 }} options={Object.entries(presenceLabels).map(([value, label]) => ({ value: value as Presence, label }))} />
      <Button aria-label="刷新用户列表" icon={<ReloadOutlined />} onClick={() => setRevision(value => value + 1)} />
    </Space>
    <p className="secondary">最近 5 分钟使用过有效登录会话的账号显示为在线，每 30 秒刷新。</p>
    {error && <Alert type="error" showIcon title="用户状态刷新失败" description={error} />}
    <Table<Account> rowKey="id" size="small" loading={loading} scroll={{ x: 570 }}
      dataSource={users.filter(u => (!presence || u.presence === presence) &&
        (u.username.toLowerCase().includes(name.trim().toLowerCase()) || u.id.includes(name.trim())))}
      pagination={{ pageSize: 20, hideOnSinglePage: true }}
      columns={[
        { title: "用户", dataIndex: "username", render: (value, u) => <span>{value} {u.id === currentUserId && <Tag>自己</Tag>}{!u.enabled && <Tag>已禁用</Tag>}</span> },
        { title: "角色", dataIndex: "role", width: 80, render: roleLabel },
        { title: "状态", dataIndex: "presence", width: 60, render: (v: Presence) => <Tag color={v === "online" ? "green" : undefined}>{presenceLabels[v]}</Tag> },
        { title: "有效会话", dataIndex: "sessions", width: 72 },
        { title: "最近活动", dataIndex: "last_seen_at", width: 116, render: (v: string | null) => v ? formatDateTime(v) : "—" },
        { title: "操作", width: 92, render: (_, u) => <Popconfirm title={`强制下线 ${u.username}？`}
          description="该账号所有设备的登录会话将失效，之后可以重新登录。" okText="强制下线" cancelText="取消"
          disabled={u.id === currentUserId || !u.sessions || !!pending} onConfirm={() => forceLogout(u)}>
          <Button size="small" danger disabled={u.id === currentUserId || !u.sessions || !!pending} loading={pending === u.id}>强制下线</Button>
        </Popconfirm> },
      ]} />
  </section>;
}
