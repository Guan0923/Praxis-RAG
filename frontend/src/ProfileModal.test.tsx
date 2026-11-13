import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { ProfileModal } from "./ProfileModal";
import type { AuthUser } from "./api";

// jsonResponse builds the minimal Response surface the client touches.
function jsonResponse(payload: unknown, status = 200) {
  const text = JSON.stringify(payload);
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "test",
    json: async () => JSON.parse(text),
    text: async () => text,
  } as unknown as Response;
}

function noContentResponse() {
  return {
    ok: true,
    status: 204,
    statusText: "No Content",
    json: async () => { throw new Error("没有响应体"); },
    text: async () => "",
  } as unknown as Response;
}

const fetchMock = vi.fn();

const baseUser: AuthUser = {
  id: "u-1",
  username: "alice",
  role: "user",
  enabled: true,
  email: "alice@example.com",
  email_verified: true,
  age: 28,
  gender: "female",
  has_avatar: true,
  avatar_updated_at: "2026-11-13T10:00:00Z",
};

function renderModal(overrides: Partial<AuthUser> = {}) {
  const onUpdated = vi.fn();
  const onClose = vi.fn();
  render(
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <App>
        <ProfileModal
          open
          user={{ ...baseUser, ...overrides }}
          onUpdated={onUpdated}
          onClose={onClose}
        />
      </App>
    </ConfigProvider>,
  );
  return { onUpdated, onClose };
}

beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
  vi.stubGlobal("ResizeObserver", class {
    observe() { /* jsdom has no layout */ }
    unobserve() { /* jsdom has no layout */ }
    disconnect() { /* jsdom has no layout */ }
  });
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("ProfileModal", () => {
  it("renders the current profile values and the email state", async () => {
    renderModal();
    const dialog = await screen.findByRole("dialog");
    const profileForm = within(dialog).getByRole("form", { name: "基本资料" });

    expect((within(profileForm).getByLabelText("用户名") as HTMLInputElement).value).toBe("alice");
    expect((within(profileForm).getByLabelText("年龄") as HTMLInputElement).value).toBe("28");
    expect(within(profileForm).getByTitle("女")).toBeTruthy();
    expect(within(profileForm).getByRole("button", { name: /移除头像/ })).toBeTruthy();

    fireEvent.click(within(dialog).getByRole("tab", { name: "修改邮箱" }));
    await within(dialog).findByRole("form", { name: "修改邮箱" });
    expect(within(dialog).getByText(/alice@example\.com/)).toBeTruthy();
    expect(within(dialog).getByText("已验证")).toBeTruthy();
  });

  it("saves the edited username, age and gender", async () => {
    fetchMock.mockResolvedValue(jsonResponse({
      user: { ...baseUser, username: "alice2", age: 30, gender: "male" },
    }));
    const { onUpdated } = renderModal();
    const dialog = await screen.findByRole("dialog");
    const profileForm = within(dialog).getByRole("form", { name: "基本资料" });

    fireEvent.change(within(profileForm).getByLabelText("用户名"), { target: { value: "alice2" } });
    const ageInput = within(profileForm).getByLabelText("年龄");
    fireEvent.change(ageInput, { target: { value: "30" } });
    fireEvent.blur(ageInput);
    fireEvent.mouseDown(within(profileForm).getByLabelText("性别"));
    fireEvent.click(await screen.findByTitle("男"));
    fireEvent.click(within(profileForm).getByRole("button", { name: "保存资料" }));

    await waitFor(() => expect(onUpdated).toHaveBeenCalledTimes(1));
    const [path, options] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/v1/auth/profile");
    expect(options.method).toBe("PATCH");
    expect(JSON.parse(String(options.body))).toEqual({
      username: "alice2",
      age: 30,
      gender: "male",
    });
    expect(onUpdated.mock.calls[0][0]).toMatchObject({ username: "alice2", age: 30, gender: "male" });
  });

  it("keeps the current avatar through the upload endpoint", async () => {
    fetchMock.mockResolvedValue(jsonResponse({
      user: { ...baseUser, has_avatar: true, avatar_updated_at: "2026-11-14T08:00:00Z" },
    }));
    const { onUpdated } = renderModal({ has_avatar: false, avatar_updated_at: undefined });
    const dialog = await screen.findByRole("dialog");
    const fileInput = dialog.querySelector('input[type="file"]') as HTMLInputElement;
    const file = new File(["avatar-bytes"], "avatar.png", { type: "image/png" });

    fireEvent.change(fileInput, { target: { files: [file] } });

    await waitFor(() => expect(onUpdated).toHaveBeenCalledTimes(1));
    const [path, options] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/v1/auth/avatar");
    expect(options.method).toBe("PUT");
    expect(options.body).toBeInstanceOf(FormData);
    expect(onUpdated.mock.calls[0][0]).toMatchObject({
      has_avatar: true,
      avatar_updated_at: "2026-11-14T08:00:00Z",
    });
  });

  it("removes the avatar when the account has one", async () => {
    fetchMock.mockResolvedValue(noContentResponse());
    const { onUpdated } = renderModal();
    const dialog = await screen.findByRole("dialog");

    fireEvent.click(within(dialog).getByRole("button", { name: /移除头像/ }));

    await waitFor(() => expect(onUpdated).toHaveBeenCalledTimes(1));
    const [path, options] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/v1/auth/avatar");
    expect(options.method).toBe("DELETE");
    expect(onUpdated.mock.calls[0][0]).toMatchObject({ has_avatar: false });
  });

  it("rejects mismatched new passwords and then changes the password", async () => {
    renderModal();
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("tab", { name: "修改密码" }));
    const passwordForm = await within(dialog).findByRole("form", { name: "修改密码" });

    fireEvent.change(within(passwordForm).getByLabelText("当前密码"), { target: { value: "oldpass123" } });
    fireEvent.change(within(passwordForm).getByLabelText("新密码"), { target: { value: "newpass123" } });
    fireEvent.change(within(passwordForm).getByLabelText("确认新密码"), { target: { value: "mismatch123" } });
    fireEvent.click(within(passwordForm).getByRole("button", { name: "修改密码" }));

    expect(await within(passwordForm).findByText("两次输入的新密码不一致")).toBeTruthy();
    expect(fetchMock).not.toHaveBeenCalled();

    fetchMock.mockResolvedValue(noContentResponse());
    fireEvent.change(within(passwordForm).getByLabelText("确认新密码"), { target: { value: "newpass123" } });
    fireEvent.click(within(passwordForm).getByRole("button", { name: "修改密码" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [path, options] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/v1/auth/password");
    expect(options.method).toBe("POST");
    expect(JSON.parse(String(options.body))).toEqual({
      current_password: "oldpass123",
      new_password: "newpass123",
    });
  });

  it("requests an email code and confirms the email change", async () => {
    fetchMock.mockImplementation(async (path: string) => {
      if (path === "/v1/auth/email/change/code") return noContentResponse();
      if (path === "/v1/auth/email/change")
        return jsonResponse({ user: { ...baseUser, email: "new@example.com", email_verified: true } });
      return jsonResponse({});
    });
    const { onUpdated } = renderModal();
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("tab", { name: "修改邮箱" }));
    const emailForm = await within(dialog).findByRole("form", { name: "修改邮箱" });

    fireEvent.change(within(emailForm).getByLabelText("新邮箱"), { target: { value: "new@example.com" } });
    fireEvent.change(within(emailForm).getByLabelText("当前密码"), { target: { value: "oldpass123" } });
    fireEvent.click(within(emailForm).getByRole("button", { name: "发送验证码" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [codePath, codeOptions] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(codePath).toBe("/v1/auth/email/change/code");
    expect(JSON.parse(String(codeOptions.body))).toEqual({
      email: "new@example.com",
      password: "oldpass123",
    });

    fireEvent.change(within(emailForm).getByLabelText("邮箱验证码"), { target: { value: "654321" } });
    fireEvent.click(within(emailForm).getByRole("button", { name: "确认修改邮箱" }));

    await waitFor(() => expect(onUpdated).toHaveBeenCalledTimes(1));
    const changeCall = fetchMock.mock.calls.find(([path]) => path === "/v1/auth/email/change");
    expect(JSON.parse(String((changeCall![1] as RequestInit).body))).toEqual({
      email: "new@example.com",
      code: "654321",
    });
    expect(onUpdated.mock.calls[0][0]).toMatchObject({ email: "new@example.com", email_verified: true });
  });
});
