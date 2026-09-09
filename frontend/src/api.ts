export type Connection = { namespace: string; token: string };
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
export type DocumentInfo = {
  name: string;
  size: number;
  chunk_count: number;
  collection: string;
  chunking?: Chunking;
};
export type ModelState = { name: string; device: string; loaded: boolean };
export type Health = {
  backend?: string;
  collection?: string;
  models?: { embedding: ModelState; reranker: ModelState };
};
export type Retrieval = "bm25" | "embedding" | "hybrid";
export type Rerank = "none" | "cross_encoder" | "bi_encoder";
export type Hit = {
  rank: number;
  title?: string;
  source: string;
  chunk_id: string;
  document_id: string;
  text: string;
  score: number;
  metadata?: { retrieval_score?: number };
};
export type SearchResult = {
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
export type UploadResult = {
  imported: DocumentInfo[];
  skipped: (DocumentInfo & { requested_name?: string })[];
  failed: { name: string; error: string }[];
};

export const retrievalLabels = {
  bm25: "BM25",
  embedding: "Embedding",
  hybrid: "Hybrid",
};
export const rerankLabels = {
  none: "不重排",
  cross_encoder: "Cross Encoder",
  bi_encoder: "Bi Encoder（Mock）",
};

export function readStorage(storage: Storage, key: string, fallback = "") {
  try {
    return storage.getItem(key) || fallback;
  } catch {
    return fallback;
  }
}

export async function api<T>(
  path: string,
  connection?: Connection,
  options: RequestInit = {},
): Promise<T> {
  const headers = new Headers(options.headers);
  headers.set("Accept", "application/json");
  if (connection) {
    headers.set("X-RAG-Namespace", connection.namespace);
    if (connection.token)
      headers.set("Authorization", `Bearer ${connection.token}`);
  }
  if (typeof options.body === "string")
    headers.set("Content-Type", "application/json");
  const response = await fetch(path, { ...options, headers });
  let payload;
  try {
    payload = await response.json();
  } catch {
    throw new Error(`HTTP ${response.status} 返回了无效 JSON`);
  }
  if (!response.ok) {
    const detail =
      typeof payload.detail === "string"
        ? payload.detail
        : JSON.stringify(payload.detail || "请求失败");
    throw new Error(`HTTP ${response.status}：${detail}`);
  }
  return payload as T;
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
