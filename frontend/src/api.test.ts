import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  api,
  apiResponse,
  createInvitation,
  createModelProfile,
  currentCSRFToken,
  deleteApiKeys,
  deleteKnowledgeBases,
  deleteModelProfile,
  errorText,
  fetchAccess,
  fetchGrants,
  fetchInvitationInbox,
  fetchModelProfileModels,
  fetchModelProfiles,
  formatBytes,
  formatDateTime,
  formatScore,
  invitationLink,
  invitationStatus,
  invitationStatusLabels,
  knowledgeBaseModelKinds,
  modelKindLabels,
  modelKinds,
  permissionLabels,
  revealModelProfileKey,
  roleLabel,
  scopeLabels,
  setCSRFToken,
  setUnauthorizedHandler,
  updateModelProfile,
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

describe("model-profile API contract", () => {
  const profile = {
    id: "mp-1",
    kind: "embedding" as const,
    name: "默认 Embedding",
    base_url: "https://api.example.com/v1",
    model: "bge-m3",
    api_key_set: true,
    vector_size: 1024,
    created_at: "2026-11-09T10:00:00Z",
    updated_at: "2026-11-09T10:00:00Z",
  };

  it("exposes the frozen model-kind vocabulary", () => {
    // 知识库只能引用前四种；JEV 是 Console 专属的第五种配置类型。
    expect(knowledgeBaseModelKinds).toEqual(["embedding", "reranker", "vlm", "ocr"]);
    expect(modelKinds).toEqual(["embedding", "reranker", "vlm", "ocr", "jev"]);
    expect(modelKindLabels).toEqual({
      embedding: "Embedding",
      reranker: "Reranker",
      vlm: "VLM",
      ocr: "OCR",
      jev: "JEV",
    });
  });

  it("lists profiles and normalizes a null vector size / missing list", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ model_profiles: [{ ...profile, vector_size: null }] }),
    );
    const rows = await fetchModelProfiles();
    expect(rows).toHaveLength(1);
    expect(rows[0].vector_size).toBeNull();
    expect(requestArgs(0)[0]).toBe("/v1/admin/model-profiles");
    expect(requestArgs(0)[1].cache).toBe("no-store");

    fetchMock.mockResolvedValue(jsonResponse({ model_profiles: null }));
    await expect(fetchModelProfiles()).resolves.toEqual([]);
  });

  it("creates a profile with the documented body", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ profile }, 201));
    const created = await createModelProfile({
      kind: "embedding",
      name: "默认 Embedding",
      base_url: "https://api.example.com/v1",
      apikey: "sk-secret",
      model: "bge-m3",
    });
    expect(created.id).toBe("mp-1");
    expect(created.vector_size).toBe(1024);
    const [path, options] = requestArgs(0);
    expect(path).toBe("/v1/admin/model-profiles");
    expect(options.method).toBe("POST");
    expect(JSON.parse(String(options.body))).toEqual({
      kind: "embedding",
      name: "默认 Embedding",
      base_url: "https://api.example.com/v1",
      apikey: "sk-secret",
      model: "bge-m3",
    });
  });

  it("creates an OCR profile whose model is the engine id and whose connection is empty", async () => {
    const ocrProfile = {
      ...profile,
      kind: "ocr" as const,
      name: "本地 OCR",
      base_url: "",
      model: "easyocr",
      api_key_set: false,
      vector_size: null,
    };
    fetchMock.mockResolvedValue(jsonResponse({ profile: ocrProfile }, 201));
    const created = await createModelProfile({
      kind: "ocr",
      name: "本地 OCR",
      base_url: "",
      apikey: "",
      model: "easyocr",
    });
    expect(created.kind).toBe("ocr");
    expect(created.model).toBe("easyocr");
    expect(created.vector_size).toBeNull();
    expect(JSON.parse(String(requestArgs(0)[1].body))).toEqual({
      kind: "ocr",
      name: "本地 OCR",
      base_url: "",
      apikey: "",
      model: "easyocr",
    });
  });

  it("patches a profile by encoded id and normalizes the response", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ profile: { ...profile, vector_size: null, name: "生产 Embedding" } }),
    );
    const updated = await updateModelProfile("mp/1", { name: "生产 Embedding", model: "bge-large" });
    expect(updated.name).toBe("生产 Embedding");
    expect(updated.vector_size).toBeNull();
    const [path, options] = requestArgs(0);
    expect(path).toBe("/v1/admin/model-profiles/mp%2F1");
    expect(options.method).toBe("PATCH");
    expect(JSON.parse(String(options.body))).toEqual({
      name: "生产 Embedding",
      model: "bge-large",
    });
  });

  it("deletes a profile through the empty-body apiResponse path", async () => {
    fetchMock.mockResolvedValue(jsonResponse({}, 204));
    await expect(deleteModelProfile("mp-1")).resolves.toBeUndefined();
    const [path, options] = requestArgs(0);
    expect(path).toBe("/v1/admin/model-profiles/mp-1");
    expect(options.method).toBe("DELETE");
  });

  it("reveals a saved key and lists models of a saved profile", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ apikey: "sk-secret" }));
    await expect(revealModelProfileKey("mp-1")).resolves.toBe("sk-secret");
    expect(requestArgs(0)[0]).toBe("/v1/admin/model-profiles/mp-1/key");
    expect(requestArgs(0)[1].cache).toBe("no-store");

    fetchMock.mockResolvedValue(jsonResponse({ models: ["bge-m3", "bge-large"] }));
    await expect(fetchModelProfileModels("mp-1")).resolves.toEqual(["bge-m3", "bge-large"]);
    expect(requestArgs(1)[0]).toBe("/v1/admin/model-profiles/mp-1/models");
    expect(requestArgs(1)[1].method).toBe("POST");

    fetchMock.mockResolvedValue(jsonResponse({ models: null }));
    await expect(fetchModelProfileModels("mp-1")).resolves.toEqual([]);
  });

  it("surfaces duplicate-name conflicts with the server detail", async () => {
    fetchMock.mockResolvedValueOnce(
      textResponse('{"detail":"同名模型配置已存在"}', 409),
    );
    await expect(createModelProfile({
      kind: "vlm",
      name: "重复名称",
      base_url: "https://api.example.com/v1",
      apikey: "",
      model: "qwen-vl",
    })).rejects.toThrow(
      "POST /v1/admin/model-profiles\nHTTP 409：同名模型配置已存在",
    );
  });
});

describe("batch-delete API contract", () => {
  it("posts knowledge-base namespaces and normalizes the purge response", async () => {
    fetchMock.mockResolvedValue(jsonResponse({
      deleted: ["kb-a"],
      failed: [{
        namespace: "kb-b",
        error: "文档删除失败",
        failed_documents: [{ name: "a.pdf", error: "上传锁冲突" }],
      }],
      documents_deleted: 3,
      configurations_deleted: 1,
    }));

    const result = await deleteKnowledgeBases(["kb-a", "kb-b"]);

    const [path, options] = requestArgs(0);
    expect(path).toBe("/v1/admin/knowledge-bases/batch-delete");
    expect(options.method).toBe("POST");
    expect(JSON.parse(String(options.body))).toEqual({ namespaces: ["kb-a", "kb-b"] });
    expect(result).toEqual({
      deleted: ["kb-a"],
      missing: [],
      failed: [{ id: "kb-b", error: "文档删除失败" }],
    });
  });

  it("posts api-key ids and maps the not_found bucket to missing", async () => {
    fetchMock.mockResolvedValue(jsonResponse({
      deleted: ["key-1"],
      not_found: ["key-2"],
      failed: [{ id: "key-3", error: "删除失败，请稍后重试" }],
      deleted_count: 1,
    }));

    const result = await deleteApiKeys(["key-1", "key-2", "key-3"]);

    const [path, options] = requestArgs(0);
    expect(path).toBe("/v1/admin/api-keys/batch-delete");
    expect(options.method).toBe("POST");
    expect(JSON.parse(String(options.body))).toEqual({ ids: ["key-1", "key-2", "key-3"] });
    expect(result).toEqual({
      deleted: ["key-1"],
      missing: ["key-2"],
      failed: [{ id: "key-3", error: "删除失败，请稍后重试" }],
    });
  });

  it("normalizes omitted buckets and malformed failure entries", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ failed: [{ id: "key-1" }, "junk", null] }));
    await expect(deleteApiKeys(["key-1", "key-2"])).resolves.toEqual({
      deleted: [],
      missing: [],
      failed: [{ id: "key-1", error: "" }],
    });

    fetchMock.mockResolvedValue(jsonResponse({}));
    await expect(deleteKnowledgeBases(["kb-a"])).resolves.toEqual({
      deleted: [],
      missing: [],
      failed: [],
    });
  });

  it("merges failed-document detail into the namespace error text", async () => {
    fetchMock.mockResolvedValue(jsonResponse({
      failed: [{
        namespace: "kb-b",
        failed_documents: [
          { name: "a.pdf", error: "锁冲突" },
          { name: "b.pdf", error: "删除失败" },
        ],
      }],
    }));
    const result = await deleteKnowledgeBases(["kb-b"]);
    expect(result.failed).toEqual([{ id: "kb-b", error: "a.pdf：锁冲突；b.pdf：删除失败" }]);
  });

  it("surfaces request-level failures through the apiResponse error", async () => {
    fetchMock.mockResolvedValueOnce(textResponse('{"detail":"namespace 不存在"}', 404));
    await expect(deleteKnowledgeBases(["kb-x"])).rejects.toThrow(
      "POST /v1/admin/knowledge-bases/batch-delete\nHTTP 404：namespace 不存在",
    );
  });
});
