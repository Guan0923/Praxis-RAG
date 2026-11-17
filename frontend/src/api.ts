export type Connection = { namespace: string; token: string };
export type Role = "admin" | "vip" | "user";
// Gender keeps the lowercase literals stored by the server; "" means unset.
export type Gender = "male" | "female" | "other";
export const genderLabels: Record<Gender, string> = {
  male: "男",
  female: "女",
  other: "其他",
};
export type AuthUser = {
  id: string;
  username: string;
  role: Role;
  enabled: boolean;
  created_at?: string;
  last_login_at?: string;
  // Profile fields stay optional: an account without an email or avatar still
  // returns the rest of the shared user projection.
  email?: string;
  email_verified?: boolean;
  age?: number | null;
  gender?: Gender | "";
  has_avatar?: boolean;
  avatar_updated_at?: string;
};
export const roleLabels: Record<Role, string> = {
  admin: "管理员",
  vip: "VIP",
  user: "普通用户",
};
// roleLabel keeps unknown forward-compatible roles readable instead of blank.
export function roleLabel(role: string | undefined) {
  return role && role in roleLabels ? roleLabels[role as Role] : "普通用户";
}
export type AuthStatus = {
  auth_enabled: boolean;
  registration_enabled: boolean;
  authenticated: boolean;
  // email_verification_enabled mirrors the SMTP configuration: when the admin
  // has configured a mail server, registration requires email + code.
  email_verification_enabled?: boolean;
  user?: AuthUser;
};
export type AuthSession = { user: AuthUser; csrf_token: string; expires_at?: string };
export type UserUpdate = { role: AuthUser["role"]; enabled: boolean };

// --- Knowledge-base sharing types (roles / permissions / invitations) ------
export type PermissionLevel = "read" | "write";
export type ShareScope = "all" | "files";
// "expired" is computed by the server from expires_at; it is never persisted.
export type InvitationStatus = "pending" | "accepted" | "declined" | "revoked" | "expired";
// ScopeView is one effective permission set inside a knowledge base.
export type ScopeView = {
  whole_read: boolean;
  whole_write: boolean;
  read_documents: string[];
  write_documents: string[];
};
// AccessView is the caller's effective access in one knowledge base.
export type AccessView = {
  namespace: string;
  owner: boolean;
  can_manage: boolean;
  can_share: boolean;
  role: string;
  can_read: boolean;
  can_write: boolean;
  scope: ScopeView;
};
export type GrantView = {
  user_id: string;
  username: string;
  document_name: string;
  permission: PermissionLevel;
  granted_by: string;
  granted_by_username: string;
  created_at: string;
  updated_at: string;
};
export type InvitationView = {
  id: string;
  namespace: string;
  inviter_user_id: string;
  inviter_username: string;
  invitee_user_id: string;
  invitee_username: string;
  permission: PermissionLevel;
  scope: ShareScope;
  document_names: string[];
  status: InvitationStatus;
  created_at: string;
  expires_at: string;
  accepted_at: string | null;
};
// InviteUser is the minimal account projection of GET /v1/users.
export type InviteUser = { id: string; username: string; role: Role };
export type CreateInvitationInput = {
  username: string;
  permission: PermissionLevel;
  scope: ShareScope;
  document_names: string[];
};
export type CreateInvitationResult = {
  invitation: InvitationView;
  invite_path: string;
};

export const permissionLabels: Record<PermissionLevel, string> = {
  read: "只读",
  write: "读写",
};
export const scopeLabels: Record<ShareScope, string> = {
  all: "全库",
  files: "指定文件",
};
export const invitationStatusLabels: Record<string, string> = {
  pending: "待处理",
  accepted: "已接受",
  declined: "已拒绝",
  revoked: "已撤销",
  expired: "已过期",
};
export const invitationStatusColors: Record<string, string> = {
  pending: "processing",
  accepted: "success",
  declined: "default",
  revoked: "default",
  expired: "error",
};
// invitationStatus renders the display status: a pending invitation whose
// expires_at already passed is expired for the invitee even before the server
// records it on accept.
export function invitationStatus(invitation: InvitationView, now = Date.now()) {
  const expires = Date.parse(invitation.expires_at);
  if (invitation.status === "pending" && Number.isFinite(expires) && expires <= now)
    return "expired";
  return invitation.status;
}
export function formatDateTime(value?: string | null) {
  if (!value) return "—";
  const time = Date.parse(value);
  return Number.isFinite(time) ? new Date(time).toLocaleString() : value;
}

export type Strategy = {
  id: string;
  label: string;
  unit: string;
  chunk_size: number;
  chunk_overlap: number;
};
export type Chunking = {
  strategy: string;
  chunk_size: number;
  chunk_overlap: number;
};
export type ProviderSettings = { base_url: string; api_key_set: boolean };
export type KnowledgeBase = Chunking & {
  embedding: ProviderSettings;
  reranker: ProviderSettings;
  vlm: ProviderSettings;
  updated_at?: string;
  reembedded_at?: string;
  namespace: string;
  vector_size: number;
  embedding_model: string;
  reranker_model: string;
  vlm_model?: string;
  collection: string;
  tokenizer: string;
  created_at: string;
};
export type DocumentInfo = {
  name: string;
  size: number;
  chunk_count: number;
  collection: string;
  chunking?: Chunking;
  diagnostics?: WordDiagnostic[];
  skipped_object_count?: number;
  operation_id?: string;
};
export type WordLocation = { part?: string; paragraph?: number; headings?: string[]; scope?: string };
export type WordDiagnostic = { kind: string; message: string; location?: WordLocation; text?: string; asset_ids?: string[] };
export type AssetTable = {
  rows: string[][];
  cells?: { row?: number; col?: number; row_span?: number; col_span?: number; text?: string;
    start_row_offset_idx?: number; end_row_offset_idx?: number; start_col_offset_idx?: number;
    end_col_offset_idx?: number; nested_tables?: string[] }[];
  num_rows?: number; num_cols?: number; nested_tables?: Record<string, AssetTable>;
  caption?: string; footnotes?: string[];
};
export type ArtifactReference = {
  artifact_id: string;
  document_id: string;
  version: string | null;
  kind: string;
  name: string;
  mime_type: string;
  expires_at: number;
  resource_uri: string;
};
export type SearchAsset = { mime_type?: string; data?: string; caption?: string;
  artifact_id?: string; resource_uri?: string; expires_at?: number;
  location?: WordLocation; page?: number } & ({ type: "image" } | ({ type: "table" } & AssetTable));
export type Health = {
  backend?: string;
  collection?: string;
};
export type Retrieval = "bm25" | "embedding" | "hybrid" | "sag2";
export type Rerank = "none" | "cross_encoder";
export type Hit = {
  rank: number;
  title?: string;
  source: string;
  chunk_id: string;
  document_id: string;
  text?: string;
  score: number;
  asset?: SearchAsset;
  artifacts?: ArtifactReference[];
  metadata?: { retrieval_score?: number; location?: WordLocation;
    captions?: { text: string }[]; footnotes?: { text: string }[] };
};
export type SearchResult = {
  artifact_warning?: string;
  artifacts?: ArtifactReference[];
  retrieval: Retrieval;
  rerank: Rerank;
  evidence_status: string;
  results: Hit[];
  debug?: {
    candidate_count?: number;
    retrieval_ms?: number;
    rerank_ms?: number;
  };
};
export type UploadFailure = { name: string; error: string; operation_id?: string };
export type UploadJobStatus = "queued" | "processing" | "succeeded" | "failed";
export type UploadJob = {
  name: string;
  artifact_id: string;
  status: UploadJobStatus;
  reused?: boolean;
};
export type DocumentJobStage =
  | "receiving"
  | "parsing"
  | "chunking"
  | "staging"
  | "embedding"
  | "indexing"
  | "publishing"
  | "retry_wait"
  | "completed"
  | "failed";
export type DocumentJobStatus = {
  artifact_id: string;
  status: UploadJobStatus;
  stage?: DocumentJobStage | string;
  error?: string | null;
};
export type SyncUploadResult = {
  mode?: "sync";
  imported: DocumentInfo[];
  skipped: (DocumentInfo & { requested_name?: string })[];
  failed: UploadFailure[];
};
export type AsyncUploadResult = {
  mode: "async";
  jobs: UploadJob[];
  failed: UploadFailure[];
};
// UploadResult is discriminated by mode: an absent mode keeps the historical
// synchronous response, while mode "async" returns background jobs to poll.
export type UploadResult = SyncUploadResult | AsyncUploadResult;

// --- Document batches (批次上传) -------------------------------------------
// A batch groups the files of one POST /v1/document-batches request. The
// Console polls GET /v1/document-batches/{id} for per-file progress instead of
// tracking every artifact id itself.
export type DocumentBatchStatus =
  | "saving"
  | "processing"
  | "completed"
  | "failed"
  | "interrupted";
export type DocumentBatchItemStatus = "accepted" | "skipped" | "failed" | "saving";
export type DocumentBatchJobStatus = "queued" | "processing" | "succeeded" | "failed";
// DocumentBatchItemReason explains a skipped item; failed items keep a safe
// error category instead of a raw server message.
export type DocumentBatchItemReason =
  | "exists"
  | "queued"
  | "duplicate"
  | "validation"
  | "config"
  | "conflict"
  | "internal";
export type DocumentBatchJob = {
  status: DocumentBatchJobStatus;
  stage: string | null;
  error: string | null;
};
export type DocumentBatchItem = {
  position: number;
  name: string;
  status: DocumentBatchItemStatus;
  reason: DocumentBatchItemReason | null;
  artifact_id: string | null;
  error: string | null;
  // job is projected only for accepted items.
  job?: DocumentBatchJob;
};
export type DocumentBatch = {
  batch_id: string;
  namespace: string;
  status: DocumentBatchStatus;
  total: number;
  accepted: number;
  skipped: number;
  failed: number;
  created_at: number;
  items: DocumentBatchItem[];
};

export const uploadJobStatusLabels: Record<UploadJobStatus, string> = {
  queued: "排队中",
  processing: "处理中",
  succeeded: "已索引",
  failed: "失败",
};
export const uploadJobStageLabels: Record<string, string> = {
  receiving: "接收中",
  parsing: "解析中",
  chunking: "切分中",
  staging: "暂存中",
  embedding: "向量化中",
  indexing: "写入索引中",
  publishing: "发布中",
  retry_wait: "等待重试",
  completed: "已索引",
  failed: "失败",
};

// uploadJobLabel resolves the Chinese label shown for one background job:
// terminal states win, otherwise the finer-grained stage is preferred.
export function uploadJobLabel(status: UploadJobStatus, stage?: string | null) {
  if (status === "succeeded" || status === "failed")
    return uploadJobStatusLabels[status];
  return (
    (stage && uploadJobStageLabels[stage]) ||
    uploadJobStatusLabels[status] ||
    "处理中"
  );
}

export function isUploadJobTerminal(status: UploadJobStatus) {
  return status === "succeeded" || status === "failed";
}

// isNotFoundError recognises the HTTP 404 shape produced by apiResponse().
export function isNotFoundError(error: unknown) {
  return /\bHTTP 404\b/.test(errorText(error));
}

export const retrievalLabels = {
  bm25: "BM25",
  embedding: "Embedding",
  hybrid: "Hybrid",
  sag2: "SAG2",
};
export const rerankLabels = {
  none: "不重排",
  cross_encoder: "Cross Encoder",
};

export function readStorage(storage: Storage, key: string, fallback = "") {
  try {
    return storage.getItem(key) || fallback;
  } catch {
    return fallback;
  }
}

// csrfToken is the double-submit token of the current login session. It is
// kept in memory only: the session cookie itself is HttpOnly.
let csrfToken = "";
let unauthorizedHandler: (() => void) | undefined;

export function setCSRFToken(token: string) {
  csrfToken = token;
}

export function currentCSRFToken() {
  return csrfToken;
}

// setUnauthorizedHandler installs the fallback that runs when a request is
// rejected because the session expired.
export function setUnauthorizedHandler(handler: (() => void) | undefined) {
  unauthorizedHandler = handler;
}

export async function apiResponse(
  path: string,
  connection?: Connection,
  options: RequestInit = {},
): Promise<Response> {
  const method = (options.method || "GET").toUpperCase();
  const headers = new Headers(options.headers);
  if (!headers.has("Accept")) headers.set("Accept", "application/json");
  if (connection) {
    // An empty namespace means "this request is not namespace-scoped" (used by
    // the namespace listing, which must keep working with a stale remembered
    // namespace); omit the header instead of sending an invalid empty value.
    if (connection.namespace)
      headers.set("X-RAG-Namespace", connection.namespace);
    if (connection.token)
      headers.set("Authorization", `Bearer ${connection.token}`);
  }
  if (csrfToken && method !== "GET" && method !== "HEAD")
    headers.set("X-CSRF-Token", csrfToken);
  if (typeof options.body === "string")
    headers.set("Content-Type", "application/json");
  const response = await fetch(path, { ...options, headers, credentials: "same-origin" });
  if (response.ok) return response;
  if (response.status === 401 && !path.startsWith("/v1/auth/"))
    unauthorizedHandler?.();

  const body = await response.text();
  let detail = body || response.statusText;
  try {
    const payload = JSON.parse(body);
    detail = typeof payload.detail === "string"
      ? payload.detail
      : JSON.stringify(payload.detail ?? payload);
  } catch {
    // Keep non-JSON server diagnostics intact.
  }
  throw new Error(`${options.method || "GET"} ${path}\nHTTP ${response.status}：${detail}`);
}

export async function api<T>(
  path: string,
  connection?: Connection,
  options: RequestInit = {},
): Promise<T> {
  const response = await apiResponse(path, connection, options);
  return await response.json() as T;
}

// --- Document batches (批次上传) -------------------------------------------

// getDocumentBatch reads one batch, including every item outcome and the
// accepted items' job stage.
export async function getDocumentBatch(
  batchId: string,
  connection?: Connection,
): Promise<DocumentBatch> {
  return await api<DocumentBatch>(
    `/v1/document-batches/${encodeURIComponent(batchId)}`,
    connection,
    { cache: "no-store" },
  );
}

// listActiveDocumentBatches restores the batches a page reload lost; the
// server returns only unfinished (saving / processing) batches.
export async function listActiveDocumentBatches(
  connection?: Connection,
  limit?: number,
): Promise<DocumentBatch[]> {
  const query = limit && limit > 0 ? `?limit=${encodeURIComponent(String(limit))}` : "";
  const payload = await api<{ batches: DocumentBatch[] }>(
    `/v1/document-batches${query}`,
    connection,
    { cache: "no-store" },
  );
  return payload.batches ?? [];
}

export type DocumentBatchSubmitOptions = {
  signal?: AbortSignal;
  idempotencyKey: string;
  // uploadId is the client-generated ID of this request: the same value is
  // passed to subscribeUploadProgress() and sent as X-Upload-ID so the server
  // can tie SSE events to this exact POST.
  uploadId?: string;
  onProgress?: (percent: number) => void;
};

// submitDocumentBatch posts one multipart batch through XMLHttpRequest because
// fetch() cannot report upload progress. Headers mirror apiResponse() so the
// request is authenticated and scoped exactly like the JSON endpoints.
export function submitDocumentBatch(
  files: File[],
  connection: Connection | undefined,
  options: DocumentBatchSubmitOptions,
): Promise<DocumentBatch> {
  const method = "POST";
  const path = "/v1/document-batches";
  return new Promise<DocumentBatch>((resolve, reject) => {
    const signal = options.signal;
    let settled = false;
    const cleanup = () => {
      signal?.removeEventListener("abort", onAbort);
    };
    const finish = (action: () => void) => {
      if (settled) return;
      settled = true;
      cleanup();
      action();
    };
    const fail = (error: Error) => finish(() => reject(error));
    const succeed = (batch: DocumentBatch) => finish(() => resolve(batch));

    if (signal?.aborted) {
      reject(new Error(`${method} ${path}\n上传已取消`));
      return;
    }

    const xhr = new XMLHttpRequest();
    function onAbort() {
      xhr.abort();
    }
    xhr.open(method, path, true);
    // Same-origin XHR requests carry cookies by default, matching the
    // credentials: "same-origin" policy used by apiResponse().
    xhr.setRequestHeader("Accept", "application/json");
    if (connection) {
      xhr.setRequestHeader("X-RAG-Namespace", connection.namespace);
      if (connection.token)
        xhr.setRequestHeader("Authorization", `Bearer ${connection.token}`);
    }
    const token = currentCSRFToken();
    if (token) xhr.setRequestHeader("X-CSRF-Token", token);
    if (options.idempotencyKey)
      xhr.setRequestHeader("Idempotency-Key", options.idempotencyKey);
    if (options.uploadId) xhr.setRequestHeader("X-Upload-ID", options.uploadId);
    xhr.upload.onprogress = (event) => {
      if (!event.lengthComputable || !event.total) return;
      const percent = Math.min(
        100,
        Math.max(0, Math.round((event.loaded / event.total) * 100)),
      );
      options.onProgress?.(percent);
    };
    xhr.onload = () => {
      const status = xhr.status;
      const text = xhr.responseText || "";
      if (status >= 200 && status < 300) {
        try {
          succeed(JSON.parse(text) as DocumentBatch);
        } catch {
          fail(new Error(`${method} ${path}\nHTTP ${status}：响应解析失败`));
        }
        return;
      }
      if (status === 401) unauthorizedHandler?.();
      let detail = text || xhr.statusText;
      try {
        const payload = JSON.parse(text);
        detail = typeof payload.detail === "string"
          ? payload.detail
          : JSON.stringify(payload.detail ?? payload);
      } catch {
        // Keep non-JSON server diagnostics intact.
      }
      fail(new Error(`${method} ${path}\nHTTP ${status}：${detail}`));
    };
    xhr.onerror = () => fail(new Error(`${method} ${path}\n网络请求失败`));
    xhr.onabort = () => fail(new Error(`${method} ${path}\n上传已取消`));
    signal?.addEventListener("abort", onAbort, { once: true });

    const body = new FormData();
    files.forEach((file) => body.append("files", file, file.name));
    try {
      xhr.send(body);
    } catch (error) {
      fail(error instanceof Error ? error : new Error(String(error)));
    }
  });
}

// --- Upload progress SSE -----------------------------------------------------
// POST /v1/document-batches can push per-file transfer progress and save
// results over GET /v1/document-batches/upload-events/{upload_id}. The Console
// opens the subscription before the POST (and waits for `ready`) so that no
// event can be missed; fetch() is used instead of EventSource because the
// stream needs the same credential/namespace headers as the JSON API.

export type UploadResultStatus = "saved" | "skipped" | "failed";

export type UploadProgressEvent =
  | { type: "ready"; upload_id: string }
  | { type: "progress"; index: number; received: number }
  | {
    type: "result";
    index: number;
    status: UploadResultStatus;
    reason: string | null;
    error: string | null;
  }
  | { type: "done"; batch_id: string | null; detail: string | null };

export type UploadProgressSubscription = {
  // ready resolves once the server confirmed the subscription; it rejects when
  // the stream cannot be established (callers may still fall back to POST).
  ready: Promise<void>;
  close: () => void;
};

export function uploadProgressPath(uploadId: string) {
  return `/v1/document-batches/upload-events/${encodeURIComponent(uploadId)}`;
}

// parseUploadProgressEvent keeps only well-formed events; a stray frame must
// never break the whole stream.
function parseUploadProgressEvent(value: unknown): UploadProgressEvent | null {
  if (!value || typeof value !== "object") return null;
  const record = value as Record<string, unknown>;
  switch (record.type) {
    case "ready":
      return {
        type: "ready",
        upload_id: typeof record.upload_id === "string" ? record.upload_id : "",
      };
    case "progress":
      if (typeof record.index !== "number" || !Number.isInteger(record.index)) return null;
      if (typeof record.received !== "number" || !Number.isFinite(record.received)) return null;
      return { type: "progress", index: record.index, received: record.received };
    case "result": {
      const status = record.status;
      if (status !== "saved" && status !== "skipped" && status !== "failed") return null;
      if (typeof record.index !== "number" || !Number.isInteger(record.index)) return null;
      return {
        type: "result",
        index: record.index,
        status,
        reason: typeof record.reason === "string" ? record.reason : null,
        error: typeof record.error === "string" ? record.error : null,
      };
    }
    case "done":
      return {
        type: "done",
        batch_id: typeof record.batch_id === "string" ? record.batch_id : null,
        detail: typeof record.detail === "string" ? record.detail : null,
      };
    default:
      return null;
  }
}

// nextSSEFrame finds one complete frame (terminated by a blank line) in the
// buffered stream text, tolerating both LF and CRLF line endings and frames
// split across read() chunks.
function nextSSEFrame(buffer: string): { frame: string; rest: string } | null {
  const lf = buffer.indexOf("\n\n");
  const crlf = buffer.indexOf("\r\n\r\n");
  if (lf < 0 && crlf < 0) return null;
  if (crlf >= 0 && (lf < 0 || crlf < lf))
    return { frame: buffer.slice(0, crlf), rest: buffer.slice(crlf + 4) };
  return { frame: buffer.slice(0, lf), rest: buffer.slice(lf + 2) };
}

// subscribeUploadProgress opens the event stream of one upload request. Events
// are delivered through onEvent; comments/heartbeats are ignored, and every
// `data:` payload is parsed as JSON. The stream is closed by close(), by the
// caller's signal, or after the server sends `done`.
export function subscribeUploadProgress(
  uploadId: string,
  connection: Connection | undefined,
  onEvent: (event: UploadProgressEvent) => void,
  options: { signal?: AbortSignal } = {},
): UploadProgressSubscription {
  const path = uploadProgressPath(uploadId);
  const controller = new AbortController();
  const headers = new Headers({ Accept: "text/event-stream" });
  if (connection?.namespace) headers.set("X-RAG-Namespace", connection.namespace);
  if (connection?.token) headers.set("Authorization", `Bearer ${connection.token}`);
  const signal = options.signal;
  const abort = () => controller.abort();
  if (signal) {
    if (signal.aborted) abort();
    else signal.addEventListener("abort", abort, { once: true });
  }

  let closed = false;
  let readySettled = false;
  let resolveReady: () => void = () => undefined;
  let rejectReady: (error: Error) => void = () => undefined;
  const ready = new Promise<void>((resolve, reject) => {
    resolveReady = resolve;
    rejectReady = reject;
  });
  // Callers put their own deadline on `ready`; keep an abandoned rejection from
  // surfacing as an unhandled promise rejection.
  ready.catch(() => undefined);

  const settleReady = (error?: Error) => {
    if (readySettled) return;
    readySettled = true;
    if (error) rejectReady(error);
    else resolveReady();
  };

  const close = () => {
    if (closed) return;
    closed = true;
    signal?.removeEventListener("abort", abort);
    controller.abort();
    settleReady(new Error(`GET ${path}\n上传事件订阅已关闭`));
  };

  function handleFrame(frame: string) {
    const lines = frame.split("\n");
    const data: string[] = [];
    for (const rawLine of lines) {
      const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
      // Empty lines terminate frames (already split) and ":" lines are
      // comments/heartbeats; neither carries data.
      if (!line || line.startsWith(":")) continue;
      if (!line.startsWith("data:")) continue;
      let value = line.slice(5);
      if (value.startsWith(" ")) value = value.slice(1);
      data.push(value);
    }
    if (!data.length) return;
    let payload: unknown;
    try {
      payload = JSON.parse(data.join("\n"));
    } catch {
      return;
    }
    const event = parseUploadProgressEvent(payload);
    if (!event) return;
    if (event.type === "ready") {
      settleReady();
    }
    onEvent(event);
    if (event.type === "done") close();
  }

  const consume = async () => {
    let response: Response;
    try {
      response = await fetch(path, {
        method: "GET",
        headers,
        credentials: "same-origin",
        cache: "no-store",
        signal: controller.signal,
      });
    } catch (error) {
      settleReady(error instanceof Error ? error : new Error(String(error)));
      return;
    }
    if (!response.ok) {
      if (response.status === 401) unauthorizedHandler?.();
      let detail = response.statusText;
      try {
        const body = await response.text();
        detail = body || detail;
        try {
          const payload = JSON.parse(body);
          detail = typeof payload.detail === "string"
            ? payload.detail
            : JSON.stringify(payload.detail ?? payload);
        } catch {
          // Keep the raw response body.
        }
      } catch {
        // Keep the status text.
      }
      settleReady(new Error(`GET ${path}\nHTTP ${response.status}：${detail}`));
      return;
    }
    if (!response.body) {
      settleReady(new Error(`GET ${path}\nHTTP ${response.status}：响应没有事件流`));
      return;
    }
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    try {
      while (!closed) {
        const chunk = await reader.read();
        if (closed) break;
        if (chunk.done) break;
        buffer += decoder.decode(chunk.value, { stream: true });
        let frame = nextSSEFrame(buffer);
        while (frame) {
          buffer = frame.rest;
          handleFrame(frame.frame);
          if (closed) break;
          frame = nextSSEFrame(buffer);
        }
      }
      if (!closed) {
        buffer += decoder.decode();
        if (buffer.trim()) handleFrame(buffer);
      }
    } catch (error) {
      settleReady(error instanceof Error ? error : new Error(String(error)));
    } finally {
      try {
        await reader.cancel();
      } catch {
        // The stream may already be aborted by close().
      }
      signal?.removeEventListener("abort", abort);
      settleReady(new Error(`GET ${path}\n上传事件订阅已结束`));
    }
  };
  void consume();

  return { ready, close };
}

// --- Console authentication -------------------------------------------------

export async function fetchAuthStatus(): Promise<AuthStatus> {
  return await api<AuthStatus>("/v1/auth/status", undefined, { cache: "no-store" });
}

export async function fetchCurrentUser(): Promise<AuthSession> {
  const session = await api<AuthSession>("/v1/auth/me", undefined, { cache: "no-store" });
  setCSRFToken(session.csrf_token);
  return session;
}

export async function login(username: string, password: string): Promise<AuthSession> {
  const session = await api<AuthSession>("/v1/auth/login", undefined, {
    method: "POST",
    body: JSON.stringify({ username, password }),
  });
  setCSRFToken(session.csrf_token);
  return session;
}

export type RegisterAccountInput = {
  username: string;
  password: string;
  email?: string;
  code?: string;
};

// registerAccount keeps the legacy username/password flow while the server has
// no mail server configured; otherwise the email verification code is required.
export async function registerAccount(input: RegisterAccountInput): Promise<AuthSession> {
  const body: Record<string, string> = {
    username: input.username.trim(),
    password: input.password,
  };
  const email = input.email?.trim() || "";
  const code = input.code?.trim() || "";
  if (email) body.email = email;
  if (code) body.code = code;
  const session = await api<AuthSession>("/v1/auth/register", undefined, {
    method: "POST",
    body: JSON.stringify(body),
  });
  setCSRFToken(session.csrf_token);
  return session;
}

// requestRegisterCode mails a 6-digit code to the address being registered.
export async function requestRegisterCode(email: string): Promise<void> {
  await apiResponse("/v1/auth/register/code", undefined, {
    method: "POST",
    body: JSON.stringify({ email: email.trim() }),
  });
}

export type ProfileUpdate = {
  username: string;
  age: number | null;
  gender: Gender | "";
};

// updateProfile always sends all three fields: age:null and gender:"" clear
// the corresponding profile value on the server.
export async function updateProfile(input: ProfileUpdate): Promise<AuthUser> {
  const payload = await api<{ user: AuthUser }>("/v1/auth/profile", undefined, {
    method: "PATCH",
    body: JSON.stringify({
      username: input.username.trim(),
      age: input.age,
      gender: input.gender,
    }),
  });
  return payload.user;
}

export async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
  await apiResponse("/v1/auth/password", undefined, {
    method: "POST",
    body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
  });
}

// requestEmailChange verifies the current password and mails a code to the new
// address. A wrong password is a validation error, not a session expiry.
export async function requestEmailChange(email: string, password: string): Promise<void> {
  await apiResponse("/v1/auth/email/change/code", undefined, {
    method: "POST",
    body: JSON.stringify({ email: email.trim(), password }),
  });
}

export async function changeEmail(email: string, code: string): Promise<AuthUser> {
  const payload = await api<{ user: AuthUser }>("/v1/auth/email/change", undefined, {
    method: "POST",
    body: JSON.stringify({ email: email.trim(), code: code.trim() }),
  });
  return payload.user;
}

// uploadAvatar posts one multipart part. apiResponse() only sets a Content-Type
// for string bodies, so FormData keeps its own multipart boundary.
export async function uploadAvatar(file: File): Promise<AuthUser> {
  const body = new FormData();
  body.append("avatar", file, file.name);
  const payload = await api<{ user: AuthUser }>("/v1/auth/avatar", undefined, {
    method: "PUT",
    body,
  });
  return payload.user;
}

export async function deleteAvatar(): Promise<void> {
  await apiResponse("/v1/auth/avatar", undefined, { method: "DELETE" });
}

// avatarUrl points at the session-scoped avatar route. The version busts the
// browser cache after an upload or removal.
export function avatarUrl(version?: string) {
  return version ? `/v1/auth/avatar?v=${encodeURIComponent(version)}` : "/v1/auth/avatar";
}

// --- Mail server settings (admin) ------------------------------------------
export type MailTLS = "none" | "starttls" | "tls";
export const mailTLSLabels: Record<MailTLS, string> = {
  none: "无",
  starttls: "STARTTLS",
  tls: "SSL",
};
// MailSettings is the admin projection: it never carries the SMTP password,
// only password_set so the form can hint that a password is stored. The server
// also derives `configured` (enabled + host + from) for convenience.
export type MailSettings = {
  enabled: boolean;
  host: string;
  port: number;
  tls: MailTLS;
  username: string;
  password_set: boolean;
  from: string;
  configured?: boolean;
};
export type MailSettingsInput = {
  enabled: boolean;
  host: string;
  port: number;
  tls: MailTLS;
  username: string;
  password: string;
  from: string;
};

export async function fetchMailSettings(): Promise<MailSettings> {
  const payload = await api<{ settings: MailSettings }>("/v1/admin/mail-settings", undefined, {
    cache: "no-store",
  });
  return payload.settings;
}

// saveMailSettings sends an empty password to keep the stored one.
export async function saveMailSettings(input: MailSettingsInput): Promise<MailSettings> {
  const payload = await api<{ settings: MailSettings }>("/v1/admin/mail-settings", undefined, {
    method: "PUT",
    body: JSON.stringify(input),
  });
  return payload.settings;
}

export async function sendTestMail(to: string): Promise<void> {
  await apiResponse("/v1/admin/mail-settings/test", undefined, {
    method: "POST",
    body: JSON.stringify({ to: to.trim() }),
  });
}

export async function logoutSession() {
  try {
    await apiResponse("/v1/auth/logout", undefined, { method: "POST" });
  } catch {
    // Logout stays best effort; the local session is cleared regardless.
  } finally {
    setCSRFToken("");
  }
}

export async function fetchUsers(): Promise<AuthUser[]> {
  const payload = await api<{ users: AuthUser[] }>("/v1/admin/users", undefined, { cache: "no-store" });
  return payload.users;
}

export async function updateUser(id: string, update: UserUpdate): Promise<AuthUser> {
  const payload = await api<{ user: AuthUser }>(`/v1/admin/users/${encodeURIComponent(id)}`, undefined, {
    method: "PATCH",
    body: JSON.stringify(update),
  });
  return payload.user;
}

export async function deleteUser(id: string): Promise<void> {
  await apiResponse(`/v1/admin/users/${encodeURIComponent(id)}`, undefined, { method: "DELETE" });
}

// --- Model profiles (模型配置) ----------------------------------------------
// A model profile is a reusable Embedding / Reranker / VLM / JEV connection
// stored by an administrator; knowledge bases reference one profile per
// knowledge-base-selectable kind instead of carrying their own base URL, key and
// model id. VLM is the single remote OpenAI-compatible vision model for PDF
// parsing: it also handles layout, OCR, tables, formulas and picture
// descriptions in the parsing service. JEV (TypeSafe System One decision model)
// is a Console-only kind: it is configured for later use, is never offered by
// the knowledge-base forms, and has no OpenAI /models route.
export type KnowledgeBaseModelKind = "embedding" | "reranker" | "vlm";
export type ModelKind = KnowledgeBaseModelKind | "jev";
export type ModelProfile = {
  id: string;
  kind: ModelKind;
  name: string;
  base_url: string;
  model: string;
  api_key_set: boolean;
  vector_size: number | null;
  created_at?: string;
  updated_at?: string;
};
export type ModelProfileInput = {
  kind: ModelKind;
  name: string;
  base_url: string;
  apikey: string;
  model: string;
};
// ModelProfileUpdate omits kind: an existing profile cannot change its kind.
export type ModelProfileUpdate = Partial<Omit<ModelProfileInput, "kind">>;
// knowledgeBaseModelKinds are the kinds a knowledge base can reference; JEV stays
// Console-only and is therefore appended to modelKinds instead.
export const knowledgeBaseModelKinds: KnowledgeBaseModelKind[] = ["embedding", "reranker", "vlm"];
export const modelKinds: ModelKind[] = [...knowledgeBaseModelKinds, "jev"];
export const modelKindLabels: Record<ModelKind, string> = {
  embedding: "Embedding",
  reranker: "Reranker",
  vlm: "VLM",
  jev: "JEV",
};

// normalizeModelProfile keeps a nullable vector_size stable for the table and
// profile pickers.
function normalizeModelProfile(profile: ModelProfile): ModelProfile {
  return { ...profile, vector_size: profile.vector_size ?? null };
}

export async function fetchModelProfiles(signal?: AbortSignal): Promise<ModelProfile[]> {
  const payload = await api<{ model_profiles: ModelProfile[] }>(
    "/v1/admin/model-profiles",
    undefined,
    { signal, cache: "no-store" },
  );
  return (payload.model_profiles || []).map(normalizeModelProfile);
}

export async function createModelProfile(input: ModelProfileInput): Promise<ModelProfile> {
  const payload = await api<{ profile: ModelProfile }>("/v1/admin/model-profiles", undefined, {
    method: "POST",
    body: JSON.stringify(input),
  });
  return normalizeModelProfile(payload.profile);
}

export async function updateModelProfile(
  id: string,
  update: ModelProfileUpdate,
): Promise<ModelProfile> {
  const payload = await api<{ profile: ModelProfile }>(
    `/v1/admin/model-profiles/${encodeURIComponent(id)}`,
    undefined,
    { method: "PATCH", body: JSON.stringify(update) },
  );
  return normalizeModelProfile(payload.profile);
}

export async function deleteModelProfile(id: string): Promise<void> {
  await apiResponse(`/v1/admin/model-profiles/${encodeURIComponent(id)}`, undefined, {
    method: "DELETE",
  });
}

export async function revealModelProfileKey(
  id: string,
  signal?: AbortSignal,
): Promise<string> {
  const payload = await api<{ apikey: string }>(
    `/v1/admin/model-profiles/${encodeURIComponent(id)}/key`,
    undefined,
    { signal, cache: "no-store" },
  );
  return payload.apikey || "";
}

export async function fetchModelProfileModels(
  id: string,
  signal?: AbortSignal,
): Promise<string[]> {
  const payload = await api<{ models: string[] }>(
    `/v1/admin/model-profiles/${encodeURIComponent(id)}/models`,
    undefined,
    { method: "POST", signal },
  );
  return payload.models || [];
}

// --- System monitor (系统监控) ----------------------------------------------
// MonitorOverview is the read-only snapshot served by
// GET /v1/admin/monitor/overview. dependencies always lists the five built-in
// dependencies in backend order (mysql / redis / qdrant / kafka / pdf-service);
// queues, library and activity are null when the backend could not read the
// state database, and errors carries the backend's Chinese diagnostics.
// MonitorOverallStatus is the top-level three-state health of one snapshot,
// ranked by the backend from the observed dependencies and delivery views. An
// older backend omits it, so consumers fall back to the ok boolean.
export type MonitorOverallStatus = "ok" | "degraded" | "error";
export type MonitorDependencyStatus = "ok" | "degraded" | "error" | "disabled" | "unknown";
export type MonitorDependency = {
  name: string;
  label: string;
  status: MonitorDependencyStatus;
  latency_ms: number;
  detail: string;
  // points only exists for vector stores (Qdrant) that can count stored points.
  points?: number;
};
export type MonitorDocumentJobs = {
  by_status: Record<string, number>;
  by_stage: Record<string, number>;
  oldest_queued_seconds: number;
  stuck_processing: number;
  failed_recent: number;
};
// MonitorUploads counts upload *operations*, not files currently uploading.
// unfinished stays for compatibility and unions the four disjoint buckets:
// in_progress (INTENT/RUNNING), failed (FAILED/ROLLBACK_FAILED), rolling_back
// (ROLLING_BACK) and cleanup_pending (committed operations waiting for their
// old-version cleanup). Missing numbers mean unknown, never zero.
export type MonitorUploads = {
  unfinished: number;
  by_status: Record<string, number>;
  in_progress: number;
  failed: number;
  rolling_back: number;
  cleanup_pending: number;
};
export type MonitorQueues = {
  document_jobs: MonitorDocumentJobs;
  uploads: MonitorUploads;
};
// MonitorQueueState addresses one queue of the snapshot without naming the
// whole MonitorQueues object.
export type MonitorQueueState =
  | MonitorDocumentJobs
  | MonitorUploads;
// MonitorPipelineConnectorTask is one Kafka Connect task's observed state and
// the worker that owns it. A task can move to another worker after failover.
export type MonitorPipelineConnectorTask = {
  id: number;
  state: string;
  worker_id: string;
  trace?: string;
};
// MonitorPipelineConnector reports one named Kafka Connect connector. The two
// Debezium connectors are independent capture paths with their own schema
// history and source-offset namespace, so the backend reports them separately.
export type MonitorPipelineConnector = {
  status: MonitorDependencyStatus;
  detail: string;
  // capture_progress is always null: the binlog position lives in Kafka
  // Connect's offset topic, so the page renders 未知 instead of inventing one.
  capture_progress: number | null;
  // connector/connector_state identify the fixed connector name and its
  // cluster-wide state (RUNNING/PAUSED/FAILED/...).
  connector: string;
  connector_state: string;
  // worker_id is the worker hosting the connector (or first task) — it changes
  // after a worker failover.
  worker_id: string;
  tasks: MonitorPipelineConnectorTask[];
  failed_tasks: number;
  failure_reason: string;
  // The restart supervisor's view: whether a retry is in flight, how many
  // attempts it has made, whether the finite budget is exhausted (needs an
  // operator), and the last recovery result.
  restarting: boolean;
  restart_attempts: number;
  restart_exhausted: boolean;
  recovery: "restarting" | "exhausted" | "recovered" | "";
  last_restart_at: string;
  last_recovered_at: string;
  next_restart_at: string;
  supervised: boolean;
};
export type MonitorPipelineDocumentJobs = {
  status: MonitorDependencyStatus;
  detail: string;
};
// MonitorPipelineWorker reports whether a document worker process is actually
// alive, read from its heartbeat. The deployment always runs the document
// worker as an independent process, so only a fresh heartbeat row proves it.
export type MonitorPipelineWorker = {
  status: MonitorDependencyStatus;
  detail: string;
};
export type MonitorPipelineDebezium = {
  // tasks captures document_jobs (INSERT + schedule_version changes); cache
  // captures the read-model business tables (all c/u/d).
  tasks: MonitorPipelineConnector;
  cache: MonitorPipelineConnector;
};
// MonitorPipelineKafkaGroup is one consumer group of the independent
// pipelines: document and cache. Offsets stay null when Kafka does not
// report them, so the page prints 未知 instead of 0.
export type MonitorPipelineKafkaGroup = {
  status: MonitorDependencyStatus;
  detail: string;
  topic: string;
  group: string;
  committed_offset: number | null;
  end_offset: number | null;
  consumer_lag: number | null;
};
export type MonitorPipelineKafka = {
  status: MonitorDependencyStatus;
  detail: string;
  // topic/group/offsets mirror the document group for compatibility; the page
  // reads groups.* so all three pipelines are visible.
  topic: string;
  group: string;
  committed_offset: number | null;
  end_offset: number | null;
  consumer_lag: number | null;
  groups: {
    document: MonitorPipelineKafkaGroup;
    cache: MonitorPipelineKafkaGroup;
  };
};
// MonitorPipelineCache reports the read-model revision watermark: dirty keys
// have refreshed_revision below revision, failures keep last_error. Both
// counters are null when the state database could not be read.
export type MonitorPipelineCache = {
  status: MonitorDependencyStatus;
  dirty_keys: number | null;
  refresh_failures: number | null;
};
// MonitorPipeline keeps the delivery/consumption views apart: document job
// processing, worker heartbeat, the two Debezium connectors, the Kafka
// consumer groups and the cache revision watermark. A null field means unknown,
// never zero.
export type MonitorPipeline = {
  document_jobs: MonitorPipelineDocumentJobs;
  // worker is optional so a pre-upgrade backend still type-checks; the page
  // renders a missing worker row as 未知.
  worker?: MonitorPipelineWorker;
  debezium: MonitorPipelineDebezium;
  kafka: MonitorPipelineKafka;
  cache: MonitorPipelineCache;
};
export type MonitorLibrary = {
  documents: number;
  knowledge_bases: number;
  artifact_snapshots: number;
};
export type MonitorActivity = {
  window_hours: number;
  jobs_succeeded: number;
  jobs_failed: number;
  searches: number;
};
export type MonitorService = {
  name: string;
  version: string;
  backend: string;
  collection: string;
  started_at: string;
  uptime_seconds: number;
  upload_mode: string;
  // delivery is a fixed "debezium" marker: the service never publishes task
  // notifications itself, both Debezium connectors do.
  delivery: string;
  pdf_engine: string;
};
// MonitorSystem is the host resource snapshot served as overview.system. The
// collector samples CPU/network rates between two polls, so the first response
// after startup carries null rates; load averages are null on Windows, which
// does not report them. A null system section means the collector itself is
// unavailable, never "all zero".
export type MonitorSystemCpu = {
  usage_percent: number | null;
  cores: number;
  model: string;
  load1: number | null;
  load5: number | null;
  load15: number | null;
};
export type MonitorSystemMemory = {
  total_bytes: number;
  used_bytes: number;
  available_bytes: number;
  used_percent: number;
  swap_total_bytes: number;
  swap_used_bytes: number;
  swap_used_percent: number;
};
// MonitorSystemInterface is one NIC sample; per-interface rates are null until
// the collector has two samples for that interface.
export type MonitorSystemInterface = {
  name: string;
  rx_bytes_per_second: number | null;
  tx_bytes_per_second: number | null;
  rx_bytes_total: number;
  tx_bytes_total: number;
};
export type MonitorSystemNetwork = {
  rx_bytes_per_second: number | null;
  tx_bytes_per_second: number | null;
  rx_bytes_total: number;
  tx_bytes_total: number;
  interfaces: MonitorSystemInterface[];
};
export type MonitorSystemProcess = {
  pid: number;
  rss_bytes: number;
  vms_bytes: number;
  goroutines: number;
  heap_alloc_bytes: number;
};
export type MonitorSystem = {
  collected_at: string;
  window_seconds: number;
  // notes carries collector diagnostics (for example an unreadable swap file);
  // the panel shows them verbatim instead of guessing.
  notes: string[];
  cpu: MonitorSystemCpu;
  memory: MonitorSystemMemory;
  network: MonitorSystemNetwork;
  process: MonitorSystemProcess;
};
export type MonitorOverview = {
  generated_at: string;
  // status ranks the whole snapshot. Older backends omit it; the page then
  // falls back to the ok boolean.
  status?: MonitorOverallStatus;
  ok: boolean;
  service: MonitorService;
  dependencies: MonitorDependency[];
  queues: MonitorQueues | null;
  // pipeline is always present in the wire contract; null only when the whole
  // snapshot could not be built, in which case the page shows 未知.
  pipeline: MonitorPipeline | null;
  library: MonitorLibrary | null;
  activity: MonitorActivity | null;
  // system is optional (older backends omit it) and null when the host
  // collector is unavailable; the page renders 系统资源：未知（采集不可用）.
  system?: MonitorSystem | null;
  // actions is the operator-action capability block: which Console buttons
  // the backend accepts. Older backends omit it; the page then hides the
  // buttons instead of calling endpoints that do not exist.
  actions?: MonitorActionCapabilities;
  errors: string[];
};

// MonitorActionCapabilities reports whether each operator action is wired on
// the backend. The requeue action needs the state store only; the connector
// restart needs Kafka Connect; worker scaling needs the mounted Kubernetes
// service account.
export type MonitorActionCapabilities = {
  requeue_jobs: boolean;
  restart_connectors: boolean;
  scale_worker: boolean;
  worker_deployment: boolean;
};

// MonitorWorkerDeployment is the document worker Deployment replica state the
// scale card shows. Numbers are null-safe through unknownOrNumber on the page.
export type MonitorWorkerDeployment = {
  name: string;
  namespace: string;
  desired_replicas: number;
  ready_replicas: number;
  available_replicas: number;
  observed_generation: number;
};

export const monitorDependencyStatusLabels: Record<MonitorDependencyStatus, string> = {
  ok: "正常",
  degraded: "降级",
  error: "异常",
  disabled: "未启用",
  unknown: "未知",
};
// monitorDependencyStatusColors reuses the antd Tag palette that the existing
// pages use for statuses; disabled/unknown stay neutral on purpose.
export const monitorDependencyStatusColors: Record<MonitorDependencyStatus, string> = {
  ok: "success",
  degraded: "warning",
  error: "error",
  disabled: "default",
  unknown: "default",
};

// formatDuration renders a second count as a short Chinese duration. The
// monitor page shows uptimes and queue ages, so raw float seconds never reach
// the screen.
export function formatDuration(seconds?: number) {
  if (typeof seconds !== "number" || !Number.isFinite(seconds) || seconds < 0) return "—";
  const total = Math.floor(seconds);
  const days = Math.floor(total / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  if (days > 0) return `${days} 天 ${hours} 小时 ${minutes} 分`;
  if (hours > 0) return `${hours} 小时 ${minutes} 分`;
  if (minutes > 0) return `${minutes} 分 ${total % 60} 秒`;
  return `${total} 秒`;
}

export async function fetchMonitorOverview(signal?: AbortSignal): Promise<MonitorOverview> {
  return await api<MonitorOverview>("/v1/admin/monitor/overview", undefined, {
    signal,
    cache: "no-store",
  });
}

// --- Monitor operator actions (监控操作) -------------------------------------
//
// These mirror the four admin endpoints behind the Console monitor buttons.
// All are POSTs, so the shared CSRF header applies automatically for
// cookie-authenticated sessions.

// requeueDocumentJobs triggers the immediate requeue of due retries and
// expired leases (the「立即重投」button). The backend keeps the attempt budget
// and terminalization rules, so this never creates a fourth execution.
export async function requeueDocumentJobs(): Promise<{ requeued: number }> {
  return await api<{ requeued: number }>("/v1/admin/monitor/actions/requeue-jobs", undefined, {
    method: "POST",
  });
}

// restartMonitorConnector restarts one capture connector (with its tasks) and
// resets the supervisor's restart budget for that connector. The backend only
// accepts the two fixed connector names.
export async function restartMonitorConnector(connector: string): Promise<{ restarted: boolean }> {
  return await api<{ restarted: boolean }>(
    `/v1/admin/monitor/actions/connectors/${encodeURIComponent(connector)}/restart`,
    undefined,
    { method: "POST" },
  );
}

// fetchWorkerDeployment reads the document worker Deployment replica state.
// The backend answers 503 when no Kubernetes service account is mounted.
export async function fetchWorkerDeployment(signal?: AbortSignal): Promise<MonitorWorkerDeployment> {
  const payload = await api<{ deployment: MonitorWorkerDeployment }>(
    "/v1/admin/monitor/actions/worker-deployment",
    undefined,
    { signal, cache: "no-store" },
  );
  return payload.deployment;
}

// scaleWorker sets the document worker replica count (0..64).
export async function scaleWorker(replicas: number): Promise<{ replicas: number }> {
  return await api<{ replicas: number }>("/v1/admin/monitor/actions/worker-deployment/scale", undefined, {
    method: "POST",
    body: JSON.stringify({ replicas }),
  });
}

// monitorComponentLabels is the frozen component vocabulary shared by the
// overview snapshot and GET /v1/admin/monitor/logs. Labels are UI copy, not
// server data, so an unknown future component still stays readable.
export const monitorComponentLabels: Record<string, string> = {
  mysql: "MySQL",
  redis: "Redis",
  qdrant: "Qdrant",
  kafka: "Kafka",
  pdf: "PDF 解析服务",
  worker: "文档 Worker",
  debezium: "Debezium",
  model: "模型调用",
  search: "检索",
  http: "HTTP API",
  monitor: "系统监控",
  service: "服务",
  other: "其他",
};
// monitorTabComponents drives the component tabs of the monitor page; the
// 概览 and 全部日志 tabs are page chrome, not log components.
export const monitorTabComponents = [
  "mysql",
  "redis",
  "qdrant",
  "kafka",
  "pdf",
  "worker",
  "debezium",
] as const;
export type MonitorTabComponent = (typeof monitorTabComponents)[number];

// MonitorLogSource selects the in-memory ring buffer or the configured file.
export type MonitorLogSource = "memory" | "file";
// MonitorLogQuery is the read-only filter accepted by the log endpoint. Empty
// values are omitted by fetchMonitorLogs so the server defaults apply.
export type MonitorLogQuery = {
  source?: MonitorLogSource;
  component?: string;
  level?: string;
  query?: string;
  limit?: number;
  offset?: number;
};
export type MonitorLogEntry = {
  seq: number;
  time: string;
  level: string;
  component: string;
  component_label: string;
  message: string;
  // attrs are the structured fields attached to the record (job id, route,
  // duration); they are values only, never query text or credentials.
  attrs: Record<string, string>;
  source: string;
};
export type MonitorLogComponent = {
  id: string;
  label: string;
  count: number;
};
export type MonitorLogFile = {
  configured: boolean;
  path: string;
  size_bytes: number;
  truncated: boolean;
  note: string;
};
export type MonitorLogPage = {
  generated_at: string;
  source: MonitorLogSource;
  // available is false when the selected source has no usable buffer/file;
  // notes then explains why in Chinese.
  available: boolean;
  entries: MonitorLogEntry[];
  total: number;
  has_more: boolean;
  components: MonitorLogComponent[];
  file: MonitorLogFile;
  notes: string[];
};

// fetchMonitorLogs reads the bounded, read-only log page. Only non-empty
// filters and positive bounds are sent, so the server defaults (memory source,
// 100 rows, first page) stay authoritative. Log content is never cached.
export async function fetchMonitorLogs(
  query: MonitorLogQuery = {},
  signal?: AbortSignal,
): Promise<MonitorLogPage> {
  const params = new URLSearchParams();
  if (query.source) params.set("source", query.source);
  if (query.component) params.set("component", query.component);
  if (query.level) params.set("level", query.level);
  if (query.query) params.set("query", query.query);
  if (typeof query.limit === "number" && query.limit > 0) {
    params.set("limit", String(query.limit));
  }
  if (typeof query.offset === "number" && query.offset > 0) {
    params.set("offset", String(query.offset));
  }
  const search = params.toString();
  return await api<MonitorLogPage>(
    `/v1/admin/monitor/logs${search ? `?${search}` : ""}`,
    undefined,
    { signal, cache: "no-store" },
  );
}

// formatPercent renders a 0-100 percentage with one decimal; a missing sample
// stays 未知 instead of 0%.
export function formatPercent(value?: number | null) {
  if (typeof value !== "number" || !Number.isFinite(value)) return "未知";
  return `${value.toFixed(1)}%`;
}

// formatRate renders a byte-per-second rate through formatBytes; null means the
// rate is not sampled yet (first poll), so it stays 未知.
export function formatRate(value?: number | null) {
  if (typeof value !== "number" || !Number.isFinite(value)) return "未知";
  return `${formatBytes(value)}/s`;
}

// --- Knowledge-base permissions and invitations -----------------------------

// fetchAccess returns the caller's effective access view for one knowledge
// base (a session-only endpoint; legacy API-key mode has no session).
export async function fetchAccess(namespace: string, connection: Connection): Promise<AccessView> {
  return await api<AccessView>(
    `/v1/knowledge-bases/${encodeURIComponent(namespace)}/access`,
    { ...connection, namespace },
    { cache: "no-store" },
  );
}

export async function fetchGrants(namespace: string, connection: Connection): Promise<GrantView[]> {
  const payload = await api<{ permissions: GrantView[] }>(
    `/v1/knowledge-bases/${encodeURIComponent(namespace)}/permissions`,
    { ...connection, namespace },
    { cache: "no-store" },
  );
  return payload.permissions || [];
}

export async function revokeGrant(namespace: string, userId: string, connection: Connection): Promise<void> {
  await apiResponse(
    `/v1/knowledge-bases/${encodeURIComponent(namespace)}/permissions/${encodeURIComponent(userId)}`,
    { ...connection, namespace },
    { method: "DELETE" },
  );
}

export async function fetchNamespaceInvitations(
  namespace: string,
  connection: Connection,
): Promise<InvitationView[]> {
  const payload = await api<{ invitations: InvitationView[] }>(
    `/v1/knowledge-bases/${encodeURIComponent(namespace)}/invitations`,
    { ...connection, namespace },
    { cache: "no-store" },
  );
  return (payload.invitations || []).map(normalizeInvitation);
}

export async function createInvitation(
  namespace: string,
  input: CreateInvitationInput,
  connection: Connection,
): Promise<CreateInvitationResult> {
  const result = await api<CreateInvitationResult>(
    `/v1/knowledge-bases/${encodeURIComponent(namespace)}/invitations`,
    { ...connection, namespace },
    { method: "POST", body: JSON.stringify(input) },
  );
  return { ...result, invitation: normalizeInvitation(result.invitation) };
}

// normalizeInvitation tolerates a null document_names slice in JSON.
function normalizeInvitation(invitation: InvitationView): InvitationView {
  return {
    ...invitation,
    document_names: Array.isArray(invitation.document_names) ? invitation.document_names : [],
  };
}

// fetchInvitationInbox lists the current account's invitations (pending first).
export async function fetchInvitationInbox(
  options: { signal?: AbortSignal } = {},
): Promise<InvitationView[]> {
  const payload = await api<{ invitations: InvitationView[] }>(
    "/v1/invitations",
    undefined,
    { cache: "no-store", signal: options.signal },
  );
  return (payload.invitations || []).map(normalizeInvitation);
}

export async function previewInvitation(token: string): Promise<InvitationView> {
  const payload = await api<{ invitation: InvitationView }>("/v1/invitations/preview", undefined, {
    method: "POST",
    body: JSON.stringify({ token }),
  });
  return normalizeInvitation(payload.invitation);
}

export async function acceptInvitation(
  token: string,
): Promise<{ invitation: InvitationView; namespace: string }> {
  const payload = await api<{ invitation: InvitationView; namespace: string }>(
    "/v1/invitations/accept",
    undefined,
    { method: "POST", body: JSON.stringify({ token }) },
  );
  return { ...payload, invitation: normalizeInvitation(payload.invitation) };
}

export async function declineInvitation(token: string): Promise<InvitationView> {
  const payload = await api<{ invitation: InvitationView }>("/v1/invitations/decline", undefined, {
    method: "POST",
    body: JSON.stringify({ token }),
  });
  return normalizeInvitation(payload.invitation);
}

export async function revokeInvitation(invitationId: string): Promise<void> {
  await apiResponse(`/v1/invitations/${encodeURIComponent(invitationId)}`, undefined, { method: "DELETE" });
}

// fetchInvitableUsers lists enabled accounts for the invitation picker.
export async function fetchInvitableUsers(): Promise<InviteUser[]> {
  const payload = await api<{ users: InviteUser[] }>("/v1/users", undefined, { cache: "no-store" });
  return payload.users || [];
}

// invitationLink makes the shareable absolute URL from the invite_path returned
// by the server (raw token appears only in this URL).
export function invitationLink(invitePath: string) {
  return `${location.origin}${invitePath}`;
}

export function errorText(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
export function formatScore(value: unknown) {
  return typeof value === "number" && Number.isFinite(value)
    ? value.toFixed(4)
    : "—";
}
export function formatBytes(value: number) {
  if (value < 1024) return `${value} B`;
  if (value < 1048576) return `${(value / 1024).toFixed(1)} KB`;
  if (value < 1073741824) return `${(value / 1048576).toFixed(1)} MB`;
  return `${(value / 1073741824).toFixed(1)} GB`;
}
export function chunkLabel(
  config: Chunking | undefined,
  strategies: Strategy[],
) {
  if (!config) return "—";
  const strategy = strategies.find((item) => item.id === config.strategy);
  return `${strategy?.label || config.strategy} · ${config.chunk_size}/${config.chunk_overlap} ${strategy?.unit === "characters" || config.strategy === "character" ? "字符" : "token"}`;
}

// --- Batch delete (批量删除) ------------------------------------------------
// Both batch-delete endpoints answer with the same envelope so the Console can
// reuse one result shape: successful items, items that were already gone and
// items the server refused are reported in separate buckets.
export type BatchDeleteFailure = { id: string; error: string };
export type BatchDeleteResult = {
  deleted: string[];
  missing: string[];
  failed: BatchDeleteFailure[];
};

function normalizeStringList(value: unknown): string[] {
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === "string")
    : [];
}

// normalizeBatchFailure accepts both transports: the API-key endpoint reports a
// failed id as {id, error}, while the knowledge-base endpoint reports the
// namespace as {namespace, error, failed_documents}. The extra document detail
// is intentionally folded into the caller-visible error text.
function normalizeBatchFailure(value: unknown): BatchDeleteFailure | null {
  if (!value || typeof value !== "object") return null;
  const record = value as Record<string, unknown>;
  const id = typeof record.id === "string"
    ? record.id
    : typeof record.namespace === "string" ? record.namespace : "";
  if (!id) return null;
  let error = typeof record.error === "string" ? record.error : "";
  if (!error && Array.isArray(record.failed_documents)) {
    const details = record.failed_documents
      .map((item) => {
        if (!item || typeof item !== "object") return "";
        const entry = item as Record<string, unknown>;
        const name = typeof entry.name === "string" ? entry.name : "";
        const reason = typeof entry.error === "string" ? entry.error : "";
        return name || reason ? `${name}：${reason}` : "";
      })
      .filter(Boolean);
    if (details.length) error = details.join("；");
  }
  return { id, error };
}

// normalizeBatchDeleteResult tolerates omitted or renamed buckets: `not_found`
// is the API-key spelling of `missing`, and a partial JSON body must not crash
// the page after the deletions already happened on the server.
function normalizeBatchDeleteResult(payload: unknown): BatchDeleteResult {
  const record = payload && typeof payload === "object"
    ? payload as Record<string, unknown>
    : {};
  return {
    deleted: normalizeStringList(record.deleted),
    missing: normalizeStringList(record.missing ?? record.not_found),
    failed: Array.isArray(record.failed)
      ? record.failed
        .map(normalizeBatchFailure)
        .filter((item): item is BatchDeleteFailure => item !== null)
      : [],
  };
}

// deleteKnowledgeBases purges the selected knowledge bases (every document plus
// the configuration row) in one request.
export async function deleteKnowledgeBases(namespaces: string[]): Promise<BatchDeleteResult> {
  const payload = await api<unknown>("/v1/admin/knowledge-bases/batch-delete", undefined, {
    method: "POST",
    body: JSON.stringify({ namespaces }),
  });
  return normalizeBatchDeleteResult(payload);
}

// deleteApiKeys removes the selected API keys in one request.
export async function deleteApiKeys(ids: string[]): Promise<BatchDeleteResult> {
  const payload = await api<unknown>("/v1/admin/api-keys/batch-delete", undefined, {
    method: "POST",
    body: JSON.stringify({ ids }),
  });
  return normalizeBatchDeleteResult(payload);
}
