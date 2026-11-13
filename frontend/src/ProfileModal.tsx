import { useEffect, useState } from "react";
import {
  Alert, App, Avatar, Button, Divider, Form, Input, InputNumber, Modal, Select, Space, Tabs,
  Tag, Typography, Upload,
} from "antd";
import {
  CameraOutlined, DeleteOutlined, LockOutlined, MailOutlined, UserOutlined,
} from "@ant-design/icons";
import {
  avatarUrl,
  changeEmail,
  changePassword,
  deleteAvatar,
  errorText,
  genderLabels,
  requestEmailChange,
  updateProfile,
  uploadAvatar,
  type AuthUser,
  type Gender,
} from "./api";

type ProfileValues = {
  username: string;
  age?: number | null;
  gender?: Gender | "";
};

type EmailChangeValues = { email: string; password: string; code: string };

type PasswordValues = { current: string; next: string; confirm: string };

// The avatar limits mirror the server: 2MB over the PNG/JPEG/WEBP/GIF set.
const avatarMaxBytes = 2 * 1024 * 1024;
const avatarContentTypes = ["image/png", "image/jpeg", "image/webp", "image/gif"];
const emailPattern = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

const genderOptions = [
  { value: "", label: "未设置" },
  ...(Object.keys(genderLabels) as Gender[]).map((value) => ({
    value,
    label: genderLabels[value],
  })),
];

// isFormValidationError tells an antd field-validation rejection apart from a
// request failure, so the Alert never receives an opaque object.
function isFormValidationError(error: unknown) {
  return typeof error === "object" && error !== null && "errorFields" in error;
}

// ProfileModal edits the signed-in account: the basic tab saves the public
// profile (avatar / username / age / gender) and the security tab changes the
// email address through a mailed code and rotates the password.
export function ProfileModal({ open, user, onUpdated, onClose }: {
  open: boolean;
  user: AuthUser;
  onUpdated: (user: AuthUser) => void;
  onClose: () => void;
}) {
  const { message } = App.useApp();
  const [tab, setTab] = useState("profile");
  const [error, setError] = useState("");
  const [savingProfile, setSavingProfile] = useState(false);
  const [uploadingAvatar, setUploadingAvatar] = useState(false);
  const [removingAvatar, setRemovingAvatar] = useState(false);
  const [sendingEmailCode, setSendingEmailCode] = useState(false);
  const [changingEmail, setChangingEmail] = useState(false);
  const [changingPassword, setChangingPassword] = useState(false);
  const [profileForm] = Form.useForm<ProfileValues>();
  const [emailForm] = Form.useForm<EmailChangeValues>();
  const [passwordForm] = Form.useForm<PasswordValues>();

  // Each tab starts on its first page and without a stale alert every time the
  // dialog opens; the form values come from initialValues because
  // `destroyOnHidden` remounts the pane (and re-applies them) on each open.
  useEffect(() => {
    if (!open) return;
    setError("");
    setTab("profile");
  }, [open]);

  const avatarSrc = user.has_avatar ? avatarUrl(user.avatar_updated_at) : undefined;

  async function changeAvatar(file: File) {
    if (!avatarContentTypes.includes(file.type)) {
      setError("头像需为 PNG/JPEG/WEBP/GIF 图片");
      return;
    }
    if (file.size > avatarMaxBytes) {
      setError("头像不能超过 2MB");
      return;
    }
    setError("");
    setUploadingAvatar(true);
    try {
      const next = await uploadAvatar(file);
      onUpdated(next);
      void message.success("头像已更新");
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setUploadingAvatar(false);
    }
  }

  async function removeAvatar() {
    setError("");
    setRemovingAvatar(true);
    try {
      await deleteAvatar();
      onUpdated({ ...user, has_avatar: false, avatar_updated_at: undefined });
      void message.success("头像已移除");
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setRemovingAvatar(false);
    }
  }

  async function saveProfile(values: ProfileValues) {
    setError("");
    setSavingProfile(true);
    try {
      const next = await updateProfile({
        username: values.username,
        age: typeof values.age === "number" && Number.isFinite(values.age) ? values.age : null,
        gender: values.gender ?? "",
      });
      onUpdated(next);
      void message.success("资料已保存");
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setSavingProfile(false);
    }
  }

  async function sendEmailCode() {
    setError("");
    try {
      const values = await emailForm.validateFields(["email", "password"]);
      setSendingEmailCode(true);
      await requestEmailChange(values.email, values.password);
      void message.success("验证码已发送到新邮箱");
    } catch (cause) {
      if (!isFormValidationError(cause)) setError(errorText(cause));
    } finally {
      setSendingEmailCode(false);
    }
  }

  async function submitEmailChange(values: EmailChangeValues) {
    setError("");
    setChangingEmail(true);
    try {
      const next = await changeEmail(values.email, values.code);
      onUpdated(next);
      emailForm.resetFields();
      void message.success("邮箱已更新");
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setChangingEmail(false);
    }
  }

  async function submitPasswordChange(values: PasswordValues) {
    setError("");
    setChangingPassword(true);
    try {
      await changePassword(values.current, values.next);
      passwordForm.resetFields();
      void message.success("密码已更新");
    } catch (cause) {
      setError(errorText(cause));
    } finally {
      setChangingPassword(false);
    }
  }

  const profilePane = (
    <Form<ProfileValues>
      form={profileForm}
      aria-label="基本资料"
      layout="vertical"
      requiredMark={false}
      initialValues={{
        username: user.username,
        age: user.age ?? undefined,
        gender: user.gender ?? "",
      }}
      onFinish={(values) => void saveProfile(values)}
    >
      <div className="profile-avatar-row">
        <Avatar size={72} src={avatarSrc} icon={<UserOutlined />} />
        <Space wrap>
          <Upload
            accept={avatarContentTypes.join(",")}
            showUploadList={false}
            disabled={uploadingAvatar || removingAvatar}
            beforeUpload={(file) => {
              void changeAvatar(file);
              return false;
            }}
          >
            <Button icon={<CameraOutlined />} loading={uploadingAvatar}>上传头像</Button>
          </Upload>
          {user.has_avatar && (
            <Button
              icon={<DeleteOutlined />}
              loading={removingAvatar}
              onClick={() => void removeAvatar()}
            >
              移除头像
            </Button>
          )}
        </Space>
        <span className="secondary profile-avatar-hint">
          支持 PNG/JPEG/WEBP/GIF，不超过 2MB
        </span>
      </div>
      <Form.Item
        name="username"
        label="用户名"
        rules={[{ required: true, whitespace: true, message: "请输入用户名" }]}
      >
        <Input prefix={<UserOutlined />} maxLength={64} />
      </Form.Item>
      <Form.Item name="age" label="年龄">
        <InputNumber min={1} max={150} precision={0} style={{ width: "100%" }} placeholder="未设置" />
      </Form.Item>
      <Form.Item name="gender" label="性别">
        <Select options={genderOptions} placeholder="未设置" />
      </Form.Item>
      <Button type="primary" htmlType="submit" loading={savingProfile}>保存资料</Button>
    </Form>
  );

  const securityPane = (
    <div className="profile-security">
      <Typography.Title level={5} className="profile-section-title">修改邮箱</Typography.Title>
      <Typography.Paragraph type="secondary">
        当前邮箱：{user.email || "未设置"}
        {user.email && (
          <Tag color={user.email_verified ? "success" : "warning"} className="profile-email-tag">
            {user.email_verified ? "已验证" : "未验证"}
          </Tag>
        )}
      </Typography.Paragraph>
      <Form<EmailChangeValues>
        form={emailForm}
        aria-label="修改邮箱"
        layout="vertical"
        requiredMark={false}
        onFinish={(values) => void submitEmailChange(values)}
      >
        <Form.Item
          name="email"
          label="新邮箱"
          rules={[
            { required: true, message: "请输入新邮箱" },
            { pattern: emailPattern, message: "请输入有效的邮箱地址" },
          ]}
        >
          <Input prefix={<MailOutlined />} type="email" maxLength={254} placeholder="name@example.com" />
        </Form.Item>
        <Form.Item
          name="password"
          label="当前密码"
          rules={[{ required: true, message: "请输入当前密码" }]}
        >
          <Input.Password prefix={<LockOutlined />} autoComplete="current-password" maxLength={128} />
        </Form.Item>
        <Form.Item
          name="code"
          label="邮箱验证码"
          htmlFor="profile-email-code"
          rules={[
            { required: true, message: "请输入邮箱验证码" },
            { pattern: /^\d{6}$/, message: "验证码为 6 位数字" },
          ]}
        >
          <Space.Compact style={{ width: "100%" }}>
            <Input
              id="profile-email-code"
              inputMode="numeric"
              maxLength={6}
              autoComplete="one-time-code"
              placeholder="6 位数字验证码"
            />
            <Button onClick={() => void sendEmailCode()} loading={sendingEmailCode}>发送验证码</Button>
          </Space.Compact>
        </Form.Item>
        <Button type="primary" htmlType="submit" loading={changingEmail}>确认修改邮箱</Button>
      </Form>
      <Divider />
      <Typography.Title level={5} className="profile-section-title">修改密码</Typography.Title>
      <Form<PasswordValues>
        form={passwordForm}
        aria-label="修改密码"
        layout="vertical"
        requiredMark={false}
        onFinish={(values) => void submitPasswordChange(values)}
      >
        <Form.Item
          name="current"
          label="当前密码"
          rules={[{ required: true, message: "请输入当前密码" }]}
        >
          <Input.Password prefix={<LockOutlined />} autoComplete="current-password" maxLength={128} />
        </Form.Item>
        <Form.Item
          name="next"
          label="新密码"
          rules={[
            { required: true, message: "请输入新密码" },
            { min: 8, message: "密码至少 8 个字符" },
          ]}
        >
          <Input.Password prefix={<LockOutlined />} autoComplete="new-password" maxLength={128} />
        </Form.Item>
        <Form.Item
          name="confirm"
          label="确认新密码"
          dependencies={["next"]}
          rules={[
            { required: true, message: "请再次输入新密码" },
            ({ getFieldValue }) => ({
              validator(_, value) {
                return !value || getFieldValue("next") === value
                  ? Promise.resolve()
                  : Promise.reject(new Error("两次输入的新密码不一致"));
              },
            }),
          ]}
        >
          <Input.Password prefix={<LockOutlined />} autoComplete="new-password" maxLength={128} />
        </Form.Item>
        <Button type="primary" htmlType="submit" loading={changingPassword}>修改密码</Button>
      </Form>
    </div>
  );

  return (
    <Modal
      title="个人信息"
      open={open}
      onCancel={onClose}
      footer={null}
      width={560}
      destroyOnHidden
    >
      {error && (
        <Alert className="page-alert" type="error" showIcon title="操作失败" description={error} />
      )}
      <Tabs
        activeKey={tab}
        onChange={setTab}
        items={[
          { key: "profile", label: "基本资料", children: profilePane },
          { key: "security", label: "账号安全", children: securityPane },
        ]}
      />
    </Modal>
  );
}
