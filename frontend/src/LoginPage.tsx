import { useState } from "react";
import { Alert, App, Button, Card, Form, Input, Segmented, Typography } from "antd";
import { LockOutlined, UserOutlined } from "@ant-design/icons";
import {
  errorText,
  login,
  registerAccount,
  type AuthUser,
} from "./api";

type Mode = "login" | "register";

type FormValues = { username: string; password: string; confirm?: string };

export function LoginPage({
  registrationEnabled,
  onAuthenticated,
}: {
  registrationEnabled: boolean;
  onAuthenticated: (user: AuthUser) => void;
}) {
  const { message } = App.useApp();
  const [mode, setMode] = useState<Mode>("login");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");

  async function submit(values: FormValues) {
    setSubmitting(true);
    setError("");
    try {
      const session = mode === "login"
        ? await login(values.username.trim(), values.password)
        : await registerAccount(values.username.trim(), values.password);
      message.success(mode === "login" ? "登录成功" : "注册成功，已自动登录");
      onAuthenticated(session.user);
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setSubmitting(false);
    }
  }

  const title = mode === "login" ? "登录 Praxis-RAG" : "注册 Praxis-RAG";
  const description = mode === "login"
    ? "使用账号登录后即可检索与管理知识库。"
    : "第一个注册的账号将自动成为管理员。";

  return (
    <div className="auth-shell">
      <Card className="auth-card" variant="outlined">
        <div className="auth-brand">
          <Typography.Title level={3} style={{ margin: 0 }}>Praxis-RAG</Typography.Title>
          <Typography.Text type="secondary">知识与检索控制台</Typography.Text>
        </div>
        {registrationEnabled && (
          <Segmented
            block
            className="auth-mode"
            value={mode}
            onChange={(value) => {
              setMode(value as Mode);
              setError("");
            }}
            options={[
              { label: "登录", value: "login" },
              { label: "注册", value: "register" },
            ]}
          />
        )}
        <Typography.Title level={4} className="auth-title">{title}</Typography.Title>
        <Typography.Paragraph type="secondary" className="auth-description">
          {description}
        </Typography.Paragraph>
        {error && (
          <Alert className="page-alert" type="error" showIcon title="操作失败" description={error} />
        )}
        <Form<FormValues>
          layout="vertical"
          requiredMark={false}
          onFinish={(values) => void submit(values)}
        >
          <Form.Item
            name="username"
            label="用户名"
            rules={[{ required: true, whitespace: true, message: "请输入用户名" }]}
          >
            <Input
              prefix={<UserOutlined />}
              autoComplete="username"
              maxLength={64}
              autoFocus
              placeholder="3-64 位字母、数字、点、下划线或横线"
            />
          </Form.Item>
          <Form.Item
            name="password"
            label="密码"
            rules={[{ required: true, message: "请输入密码" }]}
          >
            <Input.Password
              prefix={<LockOutlined />}
              autoComplete={mode === "login" ? "current-password" : "new-password"}
              maxLength={128}
              placeholder="至少 8 个字符"
            />
          </Form.Item>
          {mode === "register" && (
            <Form.Item
              name="confirm"
              label="确认密码"
              dependencies={["password"]}
              rules={[
                { required: true, message: "请再次输入密码" },
                ({ getFieldValue }) => ({
                  validator(_, value) {
                    return !value || getFieldValue("password") === value
                      ? Promise.resolve()
                      : Promise.reject(new Error("两次输入的密码不一致"));
                  },
                }),
              ]}
            >
              <Input.Password prefix={<LockOutlined />} autoComplete="new-password" maxLength={128} />
            </Form.Item>
          )}
          <Button type="primary" htmlType="submit" block loading={submitting}>
            {mode === "login" ? "登录" : "注册并登录"}
          </Button>
        </Form>
      </Card>
    </div>
  );
}
