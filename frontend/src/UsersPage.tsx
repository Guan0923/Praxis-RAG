import { useCallback, useEffect, useState } from "react";
import {
  Alert, App, Button, Popconfirm, Select, Space, Switch, Table, Tag, Tooltip,
} from "antd";
import { DeleteOutlined, ReloadOutlined } from "@ant-design/icons";
import {
  deleteUser,
  errorText,
  fetchUsers,
  updateUser,
  type AuthUser,
} from "./api";

const roleLabels: Record<AuthUser["role"], string> = {
  admin: "管理员",
  user: "普通用户",
};

export function UsersPage({ currentUserId }: { currentUserId?: string }) {
  const { message } = App.useApp();
  const [users, setUsers] = useState<AuthUser[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [savingId, setSavingId] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      setUsers(await fetchUsers());
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function change(user: AuthUser, update: { role?: AuthUser["role"]; enabled?: boolean }) {
    setSavingId(user.id);
    try {
      const updated = await updateUser(user.id, {
        role: update.role ?? user.role,
        enabled: update.enabled ?? user.enabled,
      });
      setUsers((current) => current.map((item) => (item.id === updated.id ? updated : item)));
      void message.success("用户已更新");
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setSavingId("");
    }
  }

  async function remove(user: AuthUser) {
    setSavingId(user.id);
    try {
      await deleteUser(user.id);
      setUsers((current) => current.filter((item) => item.id !== user.id));
      void message.success("用户已删除");
    } catch (cause) {
      void message.error(errorText(cause));
    } finally {
      setSavingId("");
    }
  }

  return (
    <section aria-label="用户管理页面">
      <div className="page-heading">
        <h1>用户管理</h1>
        <Space>
          <Tooltip title="刷新用户列表">
            <Button aria-label="刷新用户列表" icon={<ReloadOutlined spin={loading} />} onClick={() => void load()} />
          </Tooltip>
        </Space>
      </div>
      <Alert
        className="page-alert"
        type="info"
        showIcon
        title="权限说明"
        description="管理员可管理知识库、API Key 与用户；普通用户可检索、上传和删除文档（所有知识库）。系统始终保留至少一名启用状态的管理员。"
      />
      {error && (
        <Alert className="page-alert" type="error" showIcon title="用户列表加载失败" description={error} />
      )}
      <Table<AuthUser>
        rowKey="id"
        size="middle"
        loading={loading}
        dataSource={users}
        pagination={false}
        columns={[
          {
            title: "用户名",
            dataIndex: "username",
            render: (username: string, user) => (
              <Space size={6}>
                <span>{username}</span>
                {user.id === currentUserId && <Tag color="blue">当前账号</Tag>}
              </Space>
            ),
          },
          {
            title: "角色",
            dataIndex: "role",
            width: 160,
            render: (role: AuthUser["role"], user) => (
              <Select
                size="small"
                value={role}
                style={{ width: 130 }}
                loading={savingId === user.id}
                options={[
                  { value: "admin", label: roleLabels.admin },
                  { value: "user", label: roleLabels.user },
                ]}
                onChange={(value) => void change(user, { role: value as AuthUser["role"] })}
              />
            ),
          },
          {
            title: "状态",
            dataIndex: "enabled",
            width: 120,
            render: (enabled: boolean, user) => (
              <Switch
                size="small"
                checked={enabled}
                loading={savingId === user.id}
                checkedChildren="启用"
                unCheckedChildren="禁用"
                onChange={(checked) => void change(user, { enabled: checked })}
              />
            ),
          },
          {
            title: "最后登录",
            dataIndex: "last_login_at",
            width: 200,
            render: (value?: string) => value ? new Date(value).toLocaleString() : "—",
          },
          {
            title: "创建时间",
            dataIndex: "created_at",
            width: 200,
            render: (value?: string) => value ? new Date(value).toLocaleString() : "—",
          },
          {
            title: "操作",
            width: 90,
            render: (_, user) => (
              <Popconfirm
                title="删除用户"
                description={user.id === currentUserId
                  ? "不能删除当前登录账号。"
                  : `确定删除 ${user.username} 吗？该账号将立即失去访问权限。`}
                okText="删除"
                okButtonProps={{ danger: true }}
                disabled={user.id === currentUserId}
                onConfirm={() => void remove(user)}
              >
                <Tooltip title={user.id === currentUserId ? "不能删除当前登录账号" : "删除用户"}>
                  <Button
                    danger
                    type="text"
                    size="small"
                    aria-label={`删除用户 ${user.username}`}
                    icon={<DeleteOutlined />}
                    disabled={user.id === currentUserId || savingId === user.id}
                  />
                </Tooltip>
              </Popconfirm>
            ),
          },
        ]}
      />
    </section>
  );
}
