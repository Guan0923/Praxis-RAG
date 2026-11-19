import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { SystemSettingsPage } from "./SystemSettingsPage";
import type { MailSettings } from "./api";

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

const savedSettings: MailSettings = {
  enabled: true,
  host: "smtp.example.com",
  port: 587,
  tls: "starttls",
  username: "mailer",
  password_set: true,
  from: "no-reply@example.com",
};

function renderPage() {
  render(
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <App>
        <SystemSettingsPage />
      </App>
    </ConfigProvider>,
  );
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

describe("SystemSettingsPage", () => {
  it("loads and backfills the saved mail settings", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ settings: savedSettings }));
    renderPage();

    expect(await screen.findByDisplayValue("smtp.example.com")).toBeTruthy();
    expect(screen.getByDisplayValue("587")).toBeTruthy();
    expect(screen.getByDisplayValue("mailer")).toBeTruthy();
    expect(screen.getByDisplayValue("no-reply@example.com")).toBeTruthy();
    expect(screen.getByRole("switch").getAttribute("aria-checked")).toBe("true");
    // The password never comes back from the server; the placeholder reports it.
    expect(screen.getByPlaceholderText("已保存")).toBeTruthy();
    expect(screen.getByText("已保存密码，留空表示保持不变")).toBeTruthy();
    expect((fetchMock.mock.calls[0] as [string])[0]).toBe("/v1/admin/mail-settings");
  });

  it("saves the edited settings and keeps the stored password when left empty", async () => {
    fetchMock.mockImplementation(async (path: string, options: RequestInit = {}) => {
      if (path === "/v1/admin/mail-settings" && options.method === "PUT")
        return jsonResponse({
          settings: { ...savedSettings, host: "smtp2.example.com", port: 465, tls: "tls" },
        });
      return jsonResponse({ settings: savedSettings });
    });
    renderPage();

    const hostInput = await screen.findByDisplayValue("smtp.example.com");
    fireEvent.change(hostInput, { target: { value: "smtp2.example.com" } });
    const portInput = screen.getByDisplayValue("587");
    fireEvent.change(portInput, { target: { value: "465" } });
    fireEvent.blur(portInput);
    fireEvent.mouseDown(screen.getByLabelText("加密方式"));
    fireEvent.click(await screen.findByTitle("SSL"));
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    const [path, options] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(path).toBe("/v1/admin/mail-settings");
    expect(options.method).toBe("PUT");
    expect(JSON.parse(String(options.body))).toEqual({
      enabled: true,
      host: "smtp2.example.com",
      port: 465,
      tls: "tls",
      username: "mailer",
      password: "",
      from: "no-reply@example.com",
    });
  });

  it("sends a test mail to the entered recipient", async () => {
    fetchMock.mockImplementation(async (path: string) => {
      if (path === "/v1/admin/mail-settings/test") return noContentResponse();
      return jsonResponse({ settings: savedSettings });
    });
    renderPage();
    await screen.findByDisplayValue("smtp.example.com");

    fireEvent.change(screen.getByPlaceholderText("收件人邮箱"), {
      target: { value: "ops@example.com" },
    });
    fireEvent.click(screen.getByRole("button", { name: /发送测试邮件/ }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    const [path, options] = fetchMock.mock.calls[1] as [string, RequestInit];
    expect(path).toBe("/v1/admin/mail-settings/test");
    expect(options.method).toBe("POST");
    expect(JSON.parse(String(options.body))).toEqual({ to: "ops@example.com" });
  });

  it("rejects an invalid test recipient without calling the server", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ settings: savedSettings }));
    renderPage();
    await screen.findByDisplayValue("smtp.example.com");

    fireEvent.change(screen.getByPlaceholderText("收件人邮箱"), { target: { value: "nope" } });
    fireEvent.click(screen.getByRole("button", { name: /发送测试邮件/ }));

    expect(await screen.findByText("请输入有效的收件人邮箱地址")).toBeTruthy();
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
