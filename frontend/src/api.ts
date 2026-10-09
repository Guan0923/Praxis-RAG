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
export type ProviderSettings = { base_url: string; api_key_set: boolean };
export type KnowledgeBase = Chunking & {
  embedding: ProviderSettings;
  reranker: ProviderSettings;
  updated_at?: string;
  reembedded_at?: string;
  namespace: string;
  vector_size: number;
  embedding_model: string;
  reranker_model: string;
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
export type SearchAsset = { mime_type?: string; data?: string; caption?: string;
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
  metadata?: { retrieval_score?: number; location?: WordLocation;
    captions?: { text: string }[]; footnotes?: { text: string }[] };
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
};

export function readStorage(storage: Storage, key: string, fallback = "") {
  try {
    return storage.getItem(key) || fallback;
  } catch {
    return fallback;
  }
}

export async function apiResponse(
  path: string,
  connection?: Connection,
  options: RequestInit = {},
): Promise<Response> {
  const headers = new Headers(options.headers);
  if (!headers.has("Accept")) headers.set("Accept", "application/json");
  if (connection) {
    headers.set("X-RAG-Namespace", connection.namespace);
    if (connection.token)
      headers.set("Authorization", `Bearer ${connection.token}`);
  }
  if (typeof options.body === "string")
    headers.set("Content-Type", "application/json");
  const response = await fetch(path, { ...options, headers });
  if (response.ok) return response;
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
