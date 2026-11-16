import {
  api,
  apiResponse,
  getDocumentBatch,
  uploadJobLabel,
  type Connection,
  type DocumentBatchJob,
} from "./api";

// --- Feishu document import (从飞书导入) -----------------------------------
// All browsing, search and reads use the signed-in account's own Feishu
// user_access_token. The app credentials live only in the deployment
// configuration; the browser never receives them.

export type FeishuAccount = {
  open_id: string;
  union_id?: string;
  name: string;
  avatar_url?: string;
};

export type FeishuStatus = {
  enabled: boolean;
  connected: boolean;
  needs_reauth: boolean;
  reconnect_reason?: string;
  account?: FeishuAccount;
  scopes: string[];
  missing_scopes: string[];
  required_scopes: string[];
  access_expires_at?: number;
  refresh_expires_at?: number;
  notice?: string;
};

export type FeishuFile = {
  token: string;
  name: string;
  type: string;
  target_type?: string;
  url?: string;
  modified_time?: string;
  importable: boolean;
  navigable: boolean;
  unsupported_reason?: string;
};

export type FeishuFilePage = {
  folder_token: string;
  files: FeishuFile[];
  has_more: boolean;
  next_page_token?: string;
};

export type FeishuSearchItem = {
  token: string;
  title: string;
  type: string;
  owner_id?: string;
  importable: boolean;
  note?: string;
};

export type FeishuSearchPage = {
  query: string;
  items: FeishuSearchItem[];
  has_more: boolean;
  total: number;
  offset: number;
  count: number;
};

export type FeishuImportDocument = { document_id: string; title: string };
export type FeishuImportItemStatus = "accepted" | "skipped" | "failed";
export type FeishuImportItem = {
  document_id: string;
  title: string;
  name: string;
  status: FeishuImportItemStatus;
  reason?: string;
  error?: string;
  artifact_id?: string | null;
  job?: DocumentBatchJob | null;
};
export type FeishuImportResult = {
  batch_id?: string;
  namespace: string;
  total: number;
  accepted: number;
  skipped: number;
  failed: number;
  items: FeishuImportItem[];
};

export const feishuImportStatusLabels: Record<FeishuImportItemStatus, string> = {
  accepted: "已受理",
  skipped: "已跳过",
  failed: "失败",
};
export const feishuImportStatusColors: Record<FeishuImportItemStatus, string> = {
  accepted: "processing",
  skipped: "default",
  failed: "error",
};
export const feishuReasonLabels: Record<string, string> = {
  duplicate: "本次重复选择",
  exists: "同名文档已存在",
  validation: "文件名校验未通过",
  empty: "没有正文",
  too_large: "正文超过 10 MiB",
  unsupported: "暂不支持",
  permission: "没有飞书文档权限",
  not_found: "文档不存在或已删除",
  reauth: "飞书授权失效",
  rate_limited: "飞书接口限流",
  network: "飞书接口网络错误",
  timeout: "读取超时",
  read_failed: "读取失败",
  config: "集成配置错误",
  conflict: "知识库状态冲突",
  internal: "服务内部错误",
};

// feishuImportReason renders the stable reason code, falling back to the
// server's safe error text.
export function feishuImportReason(item: FeishuImportItem): string {
  if (item.error) return item.error;
  if (item.reason) return feishuReasonLabels[item.reason] || item.reason;
  return "";
}

// feishuJobLabel reuses the upload vocabulary: only a succeeded job is
// searchable; queued/processing are still being indexed.
export function feishuJobLabel(
  job: DocumentBatchJob | null | undefined,
  resultStatus: FeishuImportItemStatus,
): string {
  if (resultStatus === "skipped") return "未入库";
  if (resultStatus === "failed") return "未入库";
  if (!job) return "等待索引";
  return uploadJobLabel(job.status, job.stage);
}

export async function fetchFeishuStatus(
  connection: Connection,
): Promise<FeishuStatus> {
  return await api<FeishuStatus>("/v1/feishu/status", connection, {
    cache: "no-store",
  });
}

export async function startFeishuOAuth(connection: Connection): Promise<string> {
  const payload = await api<{ authorize_url: string }>(
    "/v1/feishu/oauth/start",
    connection,
    { method: "POST" },
  );
  return payload.authorize_url;
}

export async function disconnectFeishu(connection: Connection): Promise<void> {
  await apiResponse("/v1/feishu/connection", connection, { method: "DELETE" });
}

export async function listFeishuFiles(
  folderToken: string,
  pageToken: string | undefined,
  connection: Connection,
  pageSize = 50,
): Promise<FeishuFilePage> {
  const query = new URLSearchParams();
  if (folderToken) query.set("folder_token", folderToken);
  if (pageToken) query.set("page_token", pageToken);
  query.set("page_size", String(pageSize));
  return await api<FeishuFilePage>(`/v1/feishu/files?${query.toString()}`, connection, {
    cache: "no-store",
  });
}

export async function searchFeishuDocuments(
  keyword: string,
  connection: Connection,
  offset = 0,
  limit = 20,
): Promise<FeishuSearchPage> {
  const query = new URLSearchParams({
    q: keyword,
    offset: String(offset),
    limit: String(limit),
  });
  return await api<FeishuSearchPage>(
    `/v1/feishu/search?${query.toString()}`,
    connection,
    { cache: "no-store" },
  );
}

export async function importFeishuDocuments(
  documents: FeishuImportDocument[],
  connection: Connection,
  signal?: AbortSignal,
): Promise<FeishuImportResult> {
  return await api<FeishuImportResult>("/v1/feishu/import", connection, {
    method: "POST",
    body: JSON.stringify({ documents }),
    signal,
  });
}

// refreshFeishuImportJobs reads the batch once and returns the latest job for
// each accepted artifact. The batch is the same async pipeline as an ordinary
// upload; only a succeeded job is searchable.
export async function refreshFeishuImportJobs(
  batchId: string,
  connection: Connection,
): Promise<Map<string, DocumentBatchJob>> {
  const batch = await getDocumentBatch(batchId, connection);
  const jobs = new Map<string, DocumentBatchJob>();
  for (const item of batch.items) {
    if (item.artifact_id && item.job) jobs.set(item.artifact_id, item.job);
  }
  return jobs;
}
