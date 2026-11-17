import { useCallback, useEffect, useState } from "react";
import {
  Alert, App, Button, Card, Divider, Form, Input, InputNumber, Select, Space, Spin, Switch,
  Typography,
} from "antd";
import { MailOutlined, SendOutlined } from "@ant-design/icons";
import {
  errorText,
  fetchMailSettings,
  mailTLSLabels,
  saveMailSettings,
  sendTestMail,
  type MailSettings,
  type MailTLS,
} from "./api";

type MailFormValues = {
  enabled: boolean;
  host: string;
  port: number;
  tls: MailTLS;
  username?: string;
  password?: string;
  from: string;
};

const emailPattern = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const tlsOptions = (Object.keys(mailTLSLabels) as MailTLS[]).map((value) => ({
  value,
  label: mailTLSLabels[value],
}));

// SystemSettingsPage is the admin-only SMTP configuration. The server never
// returns the stored password, so an empty password field keeps the saved one.
export function SystemSettingsPage() {
  const { message } = App.useApp();
  const [form] = Form.useForm<MailFormValues>();
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [error, setError] = useState("");
  const [settings, setSettings] = useState<MailSettings>();
  const [testTo, setTestTo] = useState("");
  const passwordSet = Boolean(settings?.password_set);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const next = await fetchMailSettings();
      setSettings(next);
      form.setFieldsValue({
        enabled: next.enabled,
        host: next.host,
        port: next.port || 587,
        tls: next.tls || "starttls",
        username: next.username,
        password: "",
        from: next.from,
      });
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setLoading(false);
    }
  }, [form]);

  useEffect(() => {
    void load();
  }, [load]);

  async function save(values: MailFormValues) {
    setError("");
    setSaving(true);
    try {
      const next = await saveMailSettings({
        enabled: Boolean(values.enabled),
        host: values.host.trim(),
        port: Number(values.port) || 587,
        tls: values.tls || "none",
        username: (values.username || "").trim(),
        // An empty password keeps the stored one on the server.
        password: values.password || "",
        from: values.from.trim(),
      });
      setSettings(next);
      form.setFieldValue("password", "");
      void message.success("邮件服务器配置已保存");
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setSaving(false);
    }
  }

  async function sendTest() {
    const recipient = testTo.trim();
    if (!emailPattern.test(recipient)) {
      setError("请输入有效的收件人邮箱地址");
      return;
    }
    setError("");
    setTesting(true);
    try {
      await sendTestMail(recipient);
      void message.success(`测试邮件已发送至 ${recipient}`);
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setTesting(false);
    }
  }

  return (
    <div className="settings-section system-settings">
      <div className="page-heading">
        <h1>系统设置</h1>
      </div>
      {error && (
        <Alert className="page-alert" type="error" showIcon title="操作失败" description={error} />
      )}
      <Spin spinning={loading}>
        <Card title={<span><MailOutlined /> 邮件服务器（SMTP）</span>}>
          <Form<MailFormValues>
            form={form}
            layout="vertical"
            requiredMark={false}
            initialValues={{ enabled: false, port: 587, tls: "starttls" }}
            onFinish={(values) => void save(values)}
          >
            <Form.Item name="enabled" label="启用邮件服务器" valuePropName="checked">
              <Switch />
            </Form.Item>
            <Form.Item
              name="host"
              label="SMTP 主机"
              rules={[{ required: true, message: "请输入 SMTP 主机" }]}
            >
              <Input placeholder="smtp.example.com" maxLength={255} />
            </Form.Item>
            <Form.Item
              name="port"
              label="端口"
              rules={[{ required: true, message: "请输入端口" }]}
            >
              <InputNumber min={1} max={65535} precision={0} style={{ width: "100%" }} />
            </Form.Item>
            <Form.Item name="tls" label="加密方式">
              <Select options={tlsOptions} />
            </Form.Item>
            <Form.Item name="username" label="用户名">
              <Input autoComplete="off" maxLength={255} placeholder="SMTP 登录用户名，可留空" />
            </Form.Item>
            <Form.Item
              name="password"
              label="密码"
              extra={passwordSet ? "已保存密码，留空表示保持不变" : "首次配置需要填写 SMTP 密码"}
            >
              <Input.Password
                autoComplete="new-password"
                maxLength={255}
                placeholder={passwordSet ? "已保存" : "SMTP 密码"}
              />
            </Form.Item>
            <Form.Item
              name="from"
              label="发件人地址"
              rules={[
                { required: true, message: "请输入发件人地址" },
                { pattern: emailPattern, message: "请输入有效的邮箱地址" },
              ]}
            >
              <Input type="email" maxLength={254} placeholder="no-reply@example.com" />
            </Form.Item>
            <Space wrap>
              <Button type="primary" htmlType="submit" loading={saving}>保存</Button>
              <Button onClick={() => void load()} loading={loading}>重新加载</Button>
            </Space>
          </Form>
          <Divider />
          <Typography.Title level={5} className="settings-subtitle">发送测试邮件</Typography.Title>
          <Space.Compact style={{ width: "100%" }}>
            <Input
              value={testTo}
              onChange={(event) => setTestTo(event.target.value)}
              placeholder="收件人邮箱"
              type="email"
              maxLength={254}
            />
            <Button icon={<SendOutlined />} loading={testing} onClick={() => void sendTest()}>
              发送测试邮件
            </Button>
          </Space.Compact>
          {settings && (
            <Typography.Paragraph type="secondary" className="settings-mail-state">
              当前状态：{settings.enabled ? "已启用" : "未启用"} · 发件人 {settings.from || "未设置"}
            </Typography.Paragraph>
          )}
        </Card>
      </Spin>
    </div>
  );
}
