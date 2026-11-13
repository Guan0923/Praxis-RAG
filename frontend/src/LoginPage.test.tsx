import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { LoginPage } from "./LoginPage";
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

const registeredUser: AuthUser = {
  id: "u-1",
  username: "alice",
  role: "admin",
  enabled: true,
  email: "a@b.com",
  email_verified: true,
};

function renderLogin({ emailVerificationEnabled = true } = {}) {
  const onAuthenticated = vi.fn();
  render(
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <App>
        <LoginPage
          registrationEnabled
          emailVerificationEnabled={emailVerificationEnabled}
          onAuthenticated={onAuthenticated}
        />
      </App>
    </ConfigProvider>,
  );
  return onAuthenticated;
}

// switchToRegister clicks the hidden Segmented radio input: the visible label
// text alone does not change the value in jsdom.
function switchToRegister() {
  const label = screen.getByText("注册");
  const input = label.closest("label")?.querySelector("input");
  fireEvent.click(input || label);
}

function fillRegistration({ email, code }: { email?: string; code?: string } = {}) {
  fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "alice" } });
  fireEvent.change(screen.getByLabelText("密码"), { target: { value: "password123" } });
  fireEvent.change(screen.getByLabelText("确认密码"), { target: { value: "password123" } });
  if (email !== undefined)
    fireEvent.change(screen.getByLabelText("邮箱"), { target: { value: email } });
  if (code !== undefined)
    fireEvent.change(screen.getByLabelText("邮箱验证码"), { target: { value: code } });
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

describe("LoginPage registration with email verification", () => {
  it("renders the email and code fields when mail verification is enabled", async () => {
    renderLogin();
    switchToRegister();

    expect(await screen.findByLabelText("邮箱")).toBeTruthy();
    expect(screen.getByLabelText("邮箱验证码")).toBeTruthy();
    expect(screen.getByRole("button", { name: "发送验证码" })).toBeTruthy();
    expect(screen.getByText("用于接收注册验证码")).toBeTruthy();
  });

  it("keeps the email optional and hides the code row when verification is disabled", async () => {
    renderLogin({ emailVerificationEnabled: false });
    switchToRegister();

    expect(await screen.findByLabelText("邮箱")).toBeTruthy();
    expect(screen.queryByLabelText("邮箱验证码")).toBeNull();
    expect(screen.getByText("可选，用于接收通知")).toBeTruthy();
  });

  it("requests a registration code and starts the 60s resend countdown", async () => {
    fetchMock.mockResolvedValue(noContentResponse());
    renderLogin();
    switchToRegister();

    fireEvent.change(await screen.findByLabelText("邮箱"), { target: { value: "a@b.com" } });
    fireEvent.click(screen.getByRole("button", { name: "发送验证码" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [path, options] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/v1/auth/register/code");
    expect(JSON.parse(String(options.body))).toEqual({ email: "a@b.com" });

    const resend = await screen.findByRole("button", { name: /秒后重发/ });
    expect((resend as HTMLButtonElement).disabled).toBe(true);
  });

  it("validates the email before requesting a code", async () => {
    renderLogin();
    switchToRegister();

    fireEvent.change(await screen.findByLabelText("邮箱"), { target: { value: "not-an-email" } });
    fireEvent.click(screen.getByRole("button", { name: "发送验证码" }));

    expect(await screen.findByText("请输入有效的邮箱地址")).toBeTruthy();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("submits email and code with the registration request", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ user: registeredUser, csrf_token: "csrf-1" }));
    const onAuthenticated = renderLogin();
    switchToRegister();

    await screen.findByLabelText("邮箱");
    fillRegistration({ email: "a@b.com", code: "123456" });
    fireEvent.click(screen.getByRole("button", { name: "注册并登录" }));

    await waitFor(() => expect(onAuthenticated).toHaveBeenCalledTimes(1));
    const registerCall = fetchMock.mock.calls.find(([path]) => path === "/v1/auth/register");
    expect(registerCall).toBeTruthy();
    expect(JSON.parse(String((registerCall![1] as RequestInit).body))).toEqual({
      username: "alice",
      password: "password123",
      email: "a@b.com",
      code: "123456",
    });
    expect(onAuthenticated.mock.calls[0][0]).toMatchObject(registeredUser);
  });

  it("registers without email fields when verification is disabled", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ user: registeredUser, csrf_token: "csrf-2" }));
    const onAuthenticated = renderLogin({ emailVerificationEnabled: false });
    switchToRegister();

    await screen.findByLabelText("邮箱");
    fillRegistration();
    fireEvent.click(screen.getByRole("button", { name: "注册并登录" }));

    await waitFor(() => expect(onAuthenticated).toHaveBeenCalledTimes(1));
    const registerCall = fetchMock.mock.calls.find(([path]) => path === "/v1/auth/register");
    expect(JSON.parse(String((registerCall![1] as RequestInit).body))).toEqual({
      username: "alice",
      password: "password123",
    });
  });
});
