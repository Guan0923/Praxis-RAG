export type Connection = { namespace: string; token: string };
export type Role = "admin" | "vip" | "user";
export type AuthUser = {
  id: string;
  username: string;
  role: Role;
  enabled: boolean;
  created_at?: string;
  last_login_at?: string;
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
  // ocr_model 是所选 OCR 引擎 id（auto|easyocr|tesseract|rapidocr），空串表示未启用
  // OCR；它和 embedding/reranker/vlm 不同，没有服务地址与 Key。
  ocr_model?: string;
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
export type Retrieval = "bm25" | "embedding" | "hybrid";
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
  result_id?: string;
  created_at?: number;
  expires_at?: number;
  snapshot_available?: boolean;
  snapshot_error?: string;
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

export async function registerAccount(username: string, password: string): Promise<AuthSession> {
  const session = await api<AuthSession>("/v1/auth/register", undefined, {
    method: "POST",
    body: JSON.stringify({ username, password }),
  });
  setCSRFToken(session.csrf_token);
  return session;
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
// A model profile is a reusable Embedding / Reranker / VLM / OCR / JEV connection
// stored by an administrator; knowledge bases reference one profile per
// knowledge-base-selectable kind instead of carrying their own base URL, key and
// model id. OCR profiles are special: they keep the engine id
// (auto|easyocr|tesseract|rapidocr) in model and use an empty base_url/apikey
// because OCR runs in the local parsing service. JEV (TypeSafe System One decision
// model) is a Console-only kind: it is configured for later use, is never offered
// by the knowledge-base forms, and has no OpenAI /models route.
export type KnowledgeBaseModelKind = "embedding" | "reranker" | "vlm" | "ocr";
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
export const knowledgeBaseModelKinds: KnowledgeBaseModelKind[] = ["embedding", "reranker", "vlm", "ocr"];
export const modelKinds: ModelKind[] = [...knowledgeBaseModelKinds, "jev"];
export const modelKindLabels: Record<ModelKind, string> = {
  embedding: "Embedding",
  reranker: "Reranker",
  vlm: "VLM",
  ocr: "OCR",
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
export async function fetchInvitationInbox(): Promise<InvitationView[]> {
  const payload = await api<{ invitations: InvitationView[] }>(
    "/v1/invitations",
    undefined,
    { cache: "no-store" },
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
  return `${(value / 1048576).toFixed(1)} MB`;
}
export function chunkLabel(
  config: Chunking | undefined,
  strategies: Strategy[],
) {
  if (!config) return "—";
  const strategy = strategies.find((item) => item.id === config.strategy);
  return `${strategy?.label || config.strategy} · ${config.chunk_size}/${config.chunk_overlap} ${strategy?.unit === "characters" || config.strategy === "character" ? "字符" : "token"}`;
}
