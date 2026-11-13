import { useEffect, useState } from "react";
import { Alert, App, Button, Card, Form, Input, Segmented, Space, Typography } from "antd";
import { LockOutlined, MailOutlined, SafetyOutlined, UserOutlined } from "@ant-design/icons";
import {
  errorText,
  login,
  registerAccount,
  requestRegisterCode,
  type AuthUser,
} from "./api";

type Mode = "login" | "register";

type FormValues = {
  username: string;
  password: string;
  confirm?: string;
  email?: string;
  code?: string;
};

// emailPattern is deliberately simple: the server performs the authoritative
// check, this only keeps an obviously malformed address out of the form.
const emailPattern = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
// resendSeconds matches the server's 60s minimum interval between two codes.
const resendSeconds = 60;

export function LoginPage({
  registrationEnabled,
  emailVerificationEnabled = false,
  onAuthenticated,
}: {
  registrationEnabled: boolean;
  // emailVerificationEnabled comes from GET /v1/auth/status and turns the
  // registration form into email + code once an admin configured SMTP.
  emailVerificationEnabled?: boolean;
  onAuthenticated: (user: AuthUser) => void;
}) {
  const { message } = App.useApp();
  const [mode, setMode] = useState<Mode>("login");
  const [submitting, setSubmitting] = useState(false);
  const [sending, setSending] = useState(false);
  const [countdown, setCountdown] = useState(0);
  const [error, setError] = useState("");
  const [form] = Form.useForm<FormValues>();

  // The countdown only ticks while it is positive, so the button re-enables
  // exactly resendSeconds after the last successful request.
  useEffect(() => {
    if (countdown <= 0) return;
    const timer = window.setTimeout(() => setCountdown((value) => value - 1), 1000);
    return () => window.clearTimeout(timer);
  }, [countdown]);

  async function sendRegisterCode() {
    const email = (form.getFieldValue("email") || "").trim();
    if (!emailPattern.test(email)) {
      form.setFields([{ name: "email", errors: ["请输入有效的邮箱地址"] }]);
      return;
    }
    setError("");
    setSending(true);
    try {
      await requestRegisterCode(email);
      setCountdown(resendSeconds);
      void message.success("验证码已发送，请查收邮件");
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setSending(false);
    }
  }

  async function submit(values: FormValues) {
    setSubmitting(true);
    setError("");
    try {
      const session = mode === "login"
        ? await login(values.username.trim(), values.password)
        : await registerAccount({
            username: values.username,
            password: values.password,
            email: values.email,
            code: emailVerificationEnabled ? values.code : "",
          });
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
              setCountdown(0);
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
          form={form}
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
          {mode === "register" && (
            <Form.Item
              name="email"
              label="邮箱"
              rules={[
                ...(emailVerificationEnabled
                  ? [{ required: true, message: "请输入邮箱" }]
                  : []),
                { pattern: emailPattern, message: "请输入有效的邮箱地址" },
              ]}
              extra={emailVerificationEnabled ? "用于接收注册验证码" : "可选，用于接收通知"}
            >
              <Input
                prefix={<MailOutlined />}
                type="email"
                autoComplete="email"
                maxLength={254}
                placeholder="name@example.com"
              />
            </Form.Item>
          )}
          {mode === "register" && emailVerificationEnabled && (
            <Form.Item
              name="code"
              label="邮箱验证码"
              htmlFor="register-email-code"
              rules={[
                { required: true, message: "请输入邮箱验证码" },
                { pattern: /^\d{6}$/, message: "验证码为 6 位数字" },
              ]}
            >
              <Space.Compact style={{ width: "100%" }}>
                <Input
                  id="register-email-code"
                  prefix={<SafetyOutlined />}
                  inputMode="numeric"
                  maxLength={6}
                  autoComplete="one-time-code"
                  placeholder="6 位数字验证码"
                />
                <Button
                  onClick={() => void sendRegisterCode()}
                  disabled={countdown > 0}
                  loading={sending}
                >
                  {countdown > 0 ? `${countdown} 秒后重发` : "发送验证码"}
                </Button>
              </Space.Compact>
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
