import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  api,
  apiResponse,
  createInvitation,
  currentCSRFToken,
  errorText,
  fetchAccess,
  fetchGrants,
  fetchInvitationInbox,
  formatBytes,
  formatDateTime,
  formatScore,
  invitationLink,
  invitationStatus,
  invitationStatusLabels,
  permissionLabels,
  roleLabel,
  scopeLabels,
  setCSRFToken,
  setUnauthorizedHandler,
  type Connection,
  type InvitationView,
} from "./api";

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

function textResponse(body: string, status: number) {
  return {
    ok: false,
    status,
    statusText: "test",
    json: async () => JSON.parse(body),
    text: async () => body,
  } as unknown as Response;
}

const fetchMock = vi.fn();
const connection: Connection = { namespace: "team-kb", token: "" };

function requestArgs(call: number) {
  return fetchMock.mock.calls[call] as [string, RequestInit];
}

function invitation(overrides: Partial<InvitationView> = {}): InvitationView {
  return {
    id: "inv-1",
    namespace: "team-kb",
    inviter_user_id: "u-admin",
    inviter_username: "admin",
    invitee_user_id: "u-vip",
    invitee_username: "vip",
    permission: "read",
    scope: "all",
    document_names: [],
    status: "pending",
    created_at: "2026-11-09T10:00:00.000000+00:00",
    expires_at: "2026-11-16T10:00:00.000000+00:00",
    accepted_at: null,
    ...overrides,
  };
}

beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
  setCSRFToken("");
  setUnauthorizedHandler(undefined);
});

afterEach(() => {
  setCSRFToken("");
  setUnauthorizedHandler(undefined);
  vi.unstubAllGlobals();
});

describe("role and permission labels", () => {
  it("maps every role to its Console label", () => {
    expect(roleLabel("admin")).toBe("管理员");
    expect(roleLabel("vip")).toBe("VIP");
    expect(roleLabel("user")).toBe("普通用户");
  });

  it("keeps forward-compatible roles readable", () => {
    expect(roleLabel("auditor")).toBe("普通用户");
    expect(roleLabel(undefined)).toBe("普通用户");
    expect(roleLabel("")).toBe("普通用户");
  });

  it("exposes the frozen sharing vocabulary", () => {
    expect(permissionLabels).toEqual({ read: "只读", write: "读写" });
    expect(scopeLabels).toEqual({ all: "全库", files: "指定文件" });
    expect(invitationStatusLabels).toMatchObject({
      pending: "待处理",
      accepted: "已接受",
      declined: "已拒绝",
      revoked: "已撤销",
      expired: "已过期",
    });
  });
});

describe("invitationStatus", () => {
  const now = Date.parse("2026-11-10T00:00:00Z");

  it("keeps processed invitations untouched", () => {
    expect(invitationStatus(invitation({ status: "accepted" }), now)).toBe("accepted");
    expect(invitationStatus(invitation({ status: "revoked" }), now)).toBe("revoked");
  });

  it("computes expiry for pending invitations", () => {
    expect(invitationStatus(invitation(), now)).toBe("pending");
    expect(
      invitationStatus(invitation({ expires_at: "2026-11-09T00:00:00Z" }), now),
    ).toBe("expired");
  });

  it("never expires an invitation with an unreadable timestamp", () => {
    expect(invitationStatus(invitation({ expires_at: "not-a-date" }), now)).toBe("pending");
  });
});

describe("small formatting helpers", () => {
  it("formats scores, sizes, timestamps and unknown errors", () => {
    expect(formatScore(0.5)).toBe("0.5000");
    expect(formatScore("0.5")).toBe("—");
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(2048)).toBe("2.0 KB");
    expect(formatBytes(2 * 1048576)).toBe("2.0 MB");
    expect(formatDateTime("")).toBe("—");
    expect(formatDateTime("2026-11-09T10:00:00Z")).not.toBe("—");
    expect(errorText(new Error("boom"))).toBe("boom");
    expect(errorText("plain")).toBe("plain");
  });
});

describe("apiResponse transport contract", () => {
  it("attaches the namespace, optional bearer token and credentials", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ ok: true }));
    const response = await apiResponse("/v1/documents", connection);
    expect(response.ok).toBe(true);
    const [path, options] = requestArgs(0);
    const headers = options.headers as Headers;
    expect(path).toBe("/v1/documents");
    expect(headers.get("X-RAG-Namespace")).toBe("team-kb");
    expect(headers.get("Authorization")).toBeNull();
    expect(headers.get("X-CSRF-Token")).toBeNull();
    expect(options.credentials).toBe("same-origin");
  });

  it("adds the session CSRF token to writes only", async () => {
    setCSRFToken("csrf-123");
    fetchMock.mockResolvedValue(jsonResponse({ ok: true }));
    await apiResponse("/v1/documents", connection, { method: "POST", body: "{}" });
    await apiResponse("/v1/documents", connection);
    const writeHeaders = requestArgs(0)[1].headers as Headers;
    const readHeaders = requestArgs(1)[1].headers as Headers;
    expect(writeHeaders.get("X-CSRF-Token")).toBe("csrf-123");
    expect(writeHeaders.get("Content-Type")).toBe("application/json");
    expect(readHeaders.get("X-CSRF-Token")).toBeNull();
    expect(currentCSRFToken()).toBe("csrf-123");
  });

  it("sends a bearer token when the legacy connection carries one", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ ok: true }));
    await apiResponse("/v1/documents", { namespace: "kb", token: "prk_test" });
    const headers = requestArgs(0)[1].headers as Headers;
    expect(headers.get("Authorization")).toBe("Bearer prk_test");
  });

  it("runs the unauthorized handler only for non-auth 401 responses", async () => {
    const handler = vi.fn();
    setUnauthorizedHandler(handler);
    fetchMock.mockResolvedValueOnce(textResponse('{"detail":"会话无效"}', 401));
    await expect(apiResponse("/v1/documents", connection)).rejects.toThrow("HTTP 401");
    expect(handler).toHaveBeenCalledTimes(1);

    handler.mockClear();
    fetchMock.mockResolvedValueOnce(textResponse('{"detail":"用户名或密码错误"}', 401));
    await expect(apiResponse("/v1/auth/login", undefined, { method: "POST" })).rejects.toThrow(
      "HTTP 401",
    );
    expect(handler).not.toHaveBeenCalled();
  });

  it("surfaces the server detail in the thrown error", async () => {
    fetchMock.mockResolvedValueOnce(
      textResponse('{"detail":"没有该知识库的修改权限"}', 403),
    );
    await expect(apiResponse("/v1/documents", connection, { method: "DELETE" })).rejects.toThrow(
      "DELETE /v1/documents\nHTTP 403：没有该知识库的修改权限",
    );

    fetchMock.mockResolvedValueOnce(textResponse("plain text failure", 500));
    await expect(apiResponse("/v1/documents", connection)).rejects.toThrow(
      "plain text failure",
    );
  });
});

describe("sharing API paths and payloads", () => {
  it("fetches the access view for the encoded namespace", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ namespace: "team kb", can_read: true }));
    const view = await fetchAccess("team kb", connection);
    expect(view.can_read).toBe(true);
    expect(requestArgs(0)[0]).toBe("/v1/knowledge-bases/team%20kb/access");
    const headers = requestArgs(0)[1].headers as Headers;
    expect(headers.get("X-RAG-Namespace")).toBe("team kb");
  });

  it("posts invitations and normalizes a null document list", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ invitation: { ...invitation({ scope: "files" }), document_names: null } }),
    );
    const result = await createInvitation(
      "team-kb",
      { username: "vip", permission: "read", scope: "files", document_names: ["a.md"] },
      connection,
    );
    expect(result.invitation.document_names).toEqual([]);
    const [path, options] = requestArgs(0);
    expect(path).toBe("/v1/knowledge-bases/team-kb/invitations");
    expect(options.method).toBe("POST");
    expect(JSON.parse(String(options.body))).toEqual({
      username: "vip",
      permission: "read",
      scope: "files",
      document_names: ["a.md"],
    });
  });

  it("treats missing permission lists as empty", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ permissions: null }));
    await expect(fetchGrants("team-kb", connection)).resolves.toEqual([]);
    fetchMock.mockResolvedValue(jsonResponse({ invitations: [{ ...invitation(), document_names: null }] }));
    const inbox = await fetchInvitationInbox();
    expect(inbox).toHaveLength(1);
    expect(inbox[0].document_names).toEqual([]);
  });

  it("builds the invite link from the server path", () => {
    expect(invitationLink("/console#/invite?token=abc")).toBe(
      `${location.origin}/console#/invite?token=abc`,
    );
  });

  it("returns decoded JSON through api()", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ users: [{ id: "u1" }] }));
    await expect(api("/v1/users")).resolves.toEqual({ users: [{ id: "u1" }] });
  });
});
