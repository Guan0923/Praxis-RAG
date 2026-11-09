export type Connection = { namespace: string; token: string };
export type AuthUser = {
  id: string;
  username: string;
  role: "admin" | "user";
  enabled: boolean;
  created_at?: string;
  last_login_at?: string;
};
export type AuthStatus = {
  auth_enabled: boolean;
  registration_enabled: boolean;
  authenticated: boolean;
  user?: AuthUser;
};
export type AuthSession = { user: AuthUser; csrf_token: string; expires_at?: string };
export type UserUpdate = { role: AuthUser["role"]; enabled: boolean };

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
