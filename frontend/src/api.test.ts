import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  api,
  apiResponse,
  avatarUrl,
  changeEmail,
  changePassword,
  createInvitation,
  createModelProfile,
  currentCSRFToken,
  deleteApiKeys,
  deleteAvatar,
  deleteKnowledgeBases,
  deleteModelProfile,
  errorText,
  fetchAccess,
  fetchGrants,
  fetchInvitationInbox,
  fetchMailSettings,
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
  registerAccount,
  requestEmailChange,
  requestRegisterCode,
  revealModelProfileKey,
  roleLabel,
  saveMailSettings,
  scopeLabels,
  sendTestMail,
  setCSRFToken,
  setUnauthorizedHandler,
  submitDocumentBatch,
  subscribeUploadProgress,
  updateModelProfile,
  updateProfile,
  uploadAvatar,
  type Connection,
  type InvitationView,
  type UploadProgressEvent,
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

// streamResponse emulates a fetch Response whose body is an SSE byte stream
// delivered in fixed chunks; chunk boundaries deliberately split frames.
function streamResponse(chunks: string[]) {
  const encoder = new TextEncoder();
  let index = 0;
  const reader = {
    read: async () => {
      if (index >= chunks.length) return { done: true, value: undefined };
      const value = encoder.encode(chunks[index]);
      index += 1;
      return { done: false, value };
    },
    cancel: async () => undefined,
  };
  return {
    ok: true,
    status: 200,
    statusText: "OK",
    body: { getReader: () => reader },
  } as unknown as Response;
}

// flushMicrotasks lets the background SSE consumer advance through await hops
// without relying on real timers.
async function flushMicrotasks() {
  for (let index = 0; index < 10; index += 1) await Promise.resolve();
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

describe("subscribeUploadProgress", () => {
  it("parses SSE frames split across chunks and resolves ready", async () => {
    fetchMock.mockReturnValue(streamResponse([
      'data: {"type":"ready","upload_id":"upload/1"}\n\ndata: {"type":"prog',
      'ress","index":0,"received":1024}\r\n\r\n: ping\n\ndata: not-json\n\n'
        + 'data: {"type":"result","index":0,"status":"saved","reason":null,"error":null}\n\r',
      '\n\r\ndata: {"type":"done","batch_id":"batch-1","detail":null}\n\n',
    ]));
    const events: UploadProgressEvent[] = [];
    let finish!: () => void;
    const finished = new Promise<void>((resolve) => {
      finish = resolve;
    });
    const subscription = subscribeUploadProgress("upload/1", connection, (event) => {
      events.push(event);
      if (event.type === "done") finish();
    });

    await subscription.ready;
    await finished;

    expect(events).toEqual([
      { type: "ready", upload_id: "upload/1" },
      { type: "progress", index: 0, received: 1024 },
      { type: "result", index: 0, status: "saved", reason: null, error: null },
      { type: "done", batch_id: "batch-1", detail: null },
    ]);
    const [path, options] = requestArgs(0);
    expect(path).toBe("/v1/document-batches/upload-events/upload%2F1");
    const headers = options.headers as Headers;
    expect(headers.get("Accept")).toBe("text/event-stream");
    expect(headers.get("X-RAG-Namespace")).toBe("team-kb");
    expect(options.credentials).toBe("same-origin");
  });

  it("rejects ready when the server refuses the subscription", async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 503,
      statusText: "Service Unavailable",
      text: async () => '{"detail":"订阅通道已满"}',
    } as unknown as Response);
    const subscription = subscribeUploadProgress("u-2", connection, vi.fn());
    await expect(subscription.ready).rejects.toThrow("HTTP 503：订阅通道已满");
  });

  it("rejects ready when the response has no event stream body", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      statusText: "OK",
      body: null,
    } as unknown as Response);
    const subscription = subscribeUploadProgress("u-3", connection, vi.fn());
    await expect(subscription.ready).rejects.toThrow("响应没有事件流");
  });

  it("close aborts the stream and settles ready", async () => {
    const never = new Promise<never>(() => {});
    const read = vi.fn(() => never);
    const cancel = vi.fn(async () => undefined);
    fetchMock.mockImplementation(async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      body: { getReader: () => ({ read, cancel }) },
    }) as unknown as Response);
    const subscription = subscribeUploadProgress("u-4", connection, vi.fn());
    await flushMicrotasks();
    const [path, options] = requestArgs(0) as [string, RequestInit];
    expect(path).toBe("/v1/document-batches/upload-events/u-4");
    expect(read).toHaveBeenCalled();
    const signal = options.signal as AbortSignal;
    expect(signal.aborted).toBe(false);

    subscription.close();
    expect(signal.aborted).toBe(true);
    await expect(subscription.ready).rejects.toThrow("上传事件订阅已关闭");
  });
});

describe("submitDocumentBatch upload id", () => {
  // The batch POST goes through XMLHttpRequest because fetch() cannot report
  // upload progress. This fake captures the headers so the X-Upload-ID
  // correlation with the SSE stream is covered, not only type-checked.
  class FakeXHR {
    static instances: FakeXHR[] = [];
    method = "";
    url = "";
    headers: Record<string, string> = {};
    upload: { onprogress?: (event: ProgressEvent) => void } = {};
    status = 202;
    statusText = "Accepted";
    responseText = JSON.stringify({ batch_id: "batch-1" });
    onload: (() => void) | null = null;
    onerror: (() => void) | null = null;
    onabort: (() => void) | null = null;
    constructor() {
      FakeXHR.instances.push(this);
    }
    open(method: string, url: string) {
      this.method = method;
      this.url = url;
    }
    setRequestHeader(name: string, value: string) {
      this.headers[name] = value;
    }
    send() {
      this.upload.onprogress?.({ lengthComputable: true, loaded: 5, total: 10 } as ProgressEvent);
      this.onload?.();
    }
    abort() {
      this.onabort?.();
    }
  }

  beforeEach(() => {
    FakeXHR.instances = [];
    vi.stubGlobal("XMLHttpRequest", FakeXHR);
  });

  it("sends the same X-Upload-ID as the subscription", async () => {
    const batch = await submitDocumentBatch([new File(["hello"], "a.md")], connection, {
      idempotencyKey: "idem-1",
      uploadId: "upload-123",
    });

    expect(batch.batch_id).toBe("batch-1");
    const [xhr] = FakeXHR.instances;
    expect(xhr.method).toBe("POST");
    expect(xhr.url).toBe("/v1/document-batches");
    expect(xhr.headers["X-Upload-ID"]).toBe("upload-123");
    expect(xhr.headers["Idempotency-Key"]).toBe("idem-1");
    expect(xhr.headers["X-RAG-Namespace"]).toBe("team-kb");
  });

  it("omits X-Upload-ID for callers that ignore progress", async () => {
    await submitDocumentBatch([new File(["hello"], "a.md")], connection, {
      idempotencyKey: "idem-2",
    });
    expect(FakeXHR.instances[0].headers["X-Upload-ID"]).toBeUndefined();
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
    // 知识库只能引用前三种；JEV 是 Console 专属的第四种配置类型。
    expect(knowledgeBaseModelKinds).toEqual(["embedding", "reranker", "vlm"]);
    expect(modelKinds).toEqual(["embedding", "reranker", "vlm", "jev"]);
    expect(modelKindLabels).toEqual({
      embedding: "Embedding",
      reranker: "Reranker",
      vlm: "VLM",
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

  it("creates a VLM profile with the remote vision connection", async () => {
    const vlmProfile = {
      ...profile,
      kind: "vlm" as const,
      name: "视觉模型",
      base_url: "https://vlm.example.com/v1",
      model: "qwen2.5-vl-72b",
      vector_size: null,
    };
    fetchMock.mockResolvedValue(jsonResponse({ profile: vlmProfile }, 201));
    const created = await createModelProfile({
      kind: "vlm",
      name: "视觉模型",
      base_url: "https://vlm.example.com/v1",
      apikey: "",
      model: "qwen2.5-vl-72b",
    });
    expect(created.kind).toBe("vlm");
    expect(created.model).toBe("qwen2.5-vl-72b");
    expect(created.vector_size).toBeNull();
    expect(JSON.parse(String(requestArgs(0)[1].body))).toEqual({
      kind: "vlm",
      name: "视觉模型",
      base_url: "https://vlm.example.com/v1",
      apikey: "",
      model: "qwen2.5-vl-72b",
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

describe("account profile and mail settings", () => {
  function noContent() {
    return {
      ok: true,
      status: 204,
      statusText: "No Content",
      json: async () => { throw new Error("没有响应体"); },
      text: async () => "",
    } as unknown as Response;
  }

  function user(overrides: Record<string, unknown> = {}) {
    return { id: "u-1", username: "alice", role: "user", enabled: true, ...overrides };
  }

  it("sends the email verification code with a registration request", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ user: user(), csrf_token: "csrf-register" }));

    const session = await registerAccount({
      username: " alice ",
      password: "password123",
      email: " a@b.com ",
      code: " 123456 ",
    });

    const [path, options] = requestArgs(0);
    expect(path).toBe("/v1/auth/register");
    expect(options.method).toBe("POST");
    expect(JSON.parse(String(options.body))).toEqual({
      username: "alice",
      password: "password123",
      email: "a@b.com",
      code: "123456",
    });
    expect(session.csrf_token).toBe("csrf-register");
    expect(currentCSRFToken()).toBe("csrf-register");
  });

  it("omits empty optional registration fields", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ user: user(), csrf_token: "csrf-plain" }));

    await registerAccount({ username: "bob", password: "password123", email: "", code: "" });

    expect(JSON.parse(String(requestArgs(0)[1].body))).toEqual({
      username: "bob",
      password: "password123",
    });
  });

  it("requests a registration code and accepts an empty 204 body", async () => {
    fetchMock.mockResolvedValue(noContent());

    await requestRegisterCode(" a@b.com ");

    const [path, options] = requestArgs(0);
    expect(path).toBe("/v1/auth/register/code");
    expect(options.method).toBe("POST");
    expect(JSON.parse(String(options.body))).toEqual({ email: "a@b.com" });
  });

  it("updates all profile fields in one PATCH", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ user: user({ username: "alice2", age: 30, gender: "female" }) }));

    const updated = await updateProfile({ username: " alice2 ", age: 30, gender: "female" });

    const [path, options] = requestArgs(0);
    expect(path).toBe("/v1/auth/profile");
    expect(options.method).toBe("PATCH");
    expect(JSON.parse(String(options.body))).toEqual({
      username: "alice2",
      age: 30,
      gender: "female",
    });
    expect(updated).toMatchObject({ username: "alice2", age: 30, gender: "female" });
  });

  it("clears age and gender when they are empty", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ user: user() }));

    await updateProfile({ username: "alice", age: null, gender: "" });

    expect(JSON.parse(String(requestArgs(0)[1].body))).toEqual({
      username: "alice",
      age: null,
      gender: "",
    });
  });

  it("posts the password, email-code and email-change requests", async () => {
    fetchMock.mockResolvedValue(noContent());
    await changePassword("oldpass123", "newpass123");
    expect(requestArgs(0)[0]).toBe("/v1/auth/password");
    expect(JSON.parse(String(requestArgs(0)[1].body))).toEqual({
      current_password: "oldpass123",
      new_password: "newpass123",
    });

    await requestEmailChange(" new@b.com ", "oldpass123");
    expect(requestArgs(1)[0]).toBe("/v1/auth/email/change/code");
    expect(JSON.parse(String(requestArgs(1)[1].body))).toEqual({
      email: "new@b.com",
      password: "oldpass123",
    });

    fetchMock.mockResolvedValue(jsonResponse({ user: user({ email: "new@b.com", email_verified: true }) }));
    const updated = await changeEmail(" new@b.com ", " 654321 ");
    expect(requestArgs(2)[0]).toBe("/v1/auth/email/change");
    expect(JSON.parse(String(requestArgs(2)[1].body))).toEqual({ email: "new@b.com", code: "654321" });
    expect(updated.email_verified).toBe(true);
  });

  it("uploads the avatar as multipart data without a JSON content type", async () => {
    fetchMock.mockResolvedValue(jsonResponse({ user: user({ has_avatar: true, avatar_updated_at: "2026-11-13T10:00:00Z" }) }));

    const file = new File(["avatar-bytes"], "avatar.png", { type: "image/png" });
    const updated = await uploadAvatar(file);

    const [path, options] = requestArgs(0);
    expect(path).toBe("/v1/auth/avatar");
    expect(options.method).toBe("PUT");
    expect(options.body).toBeInstanceOf(FormData);
    expect((options.body as FormData).get("avatar")).toBeTruthy();
    expect(new Headers(options.headers).has("Content-Type")).toBe(false);
    expect(updated.has_avatar).toBe(true);
  });

  it("deletes the avatar and builds a cache-busting avatar URL", async () => {
    fetchMock.mockResolvedValue(noContent());

    await deleteAvatar();

    expect(requestArgs(0)[0]).toBe("/v1/auth/avatar");
    expect(requestArgs(0)[1].method).toBe("DELETE");
    expect(avatarUrl()).toBe("/v1/auth/avatar");
    expect(avatarUrl("2026-11-13T10:00:00Z")).toBe(
      "/v1/auth/avatar?v=2026-11-13T10%3A00%3A00Z",
    );
  });

  it("loads, saves and tests the mail settings", async () => {
    const settings = {
      enabled: true,
      host: "smtp.example.com",
      port: 587,
      tls: "starttls" as const,
      username: "mailer",
      password_set: true,
      from: "no-reply@example.com",
    };
    fetchMock.mockResolvedValue(jsonResponse({ settings }));
    await expect(fetchMailSettings()).resolves.toEqual(settings);
    expect(requestArgs(0)[0]).toBe("/v1/admin/mail-settings");

    fetchMock.mockResolvedValue(jsonResponse({ settings: { ...settings, host: "smtp2.example.com" } }));
    const saved = await saveMailSettings({
      enabled: true,
      host: "smtp2.example.com",
      port: 465,
      tls: "tls",
      username: "mailer",
      password: "",
      from: "no-reply@example.com",
    });
    expect(requestArgs(1)[0]).toBe("/v1/admin/mail-settings");
    expect(requestArgs(1)[1].method).toBe("PUT");
    expect(JSON.parse(String(requestArgs(1)[1].body))).toEqual({
      enabled: true,
      host: "smtp2.example.com",
      port: 465,
      tls: "tls",
      username: "mailer",
      password: "",
      from: "no-reply@example.com",
    });
    expect(saved.host).toBe("smtp2.example.com");

    fetchMock.mockResolvedValue(noContent());
    await sendTestMail(" ops@example.com ");
    expect(requestArgs(2)[0]).toBe("/v1/admin/mail-settings/test");
    expect(JSON.parse(String(requestArgs(2)[1].body))).toEqual({ to: "ops@example.com" });
  });
});
