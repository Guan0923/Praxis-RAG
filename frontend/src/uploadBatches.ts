import {
  errorText,
  getDocumentBatch,
  isNotFoundError,
  listActiveDocumentBatches,
  submitDocumentBatch,
  type Connection,
  type DocumentBatch,
  type DocumentBatchItemStatus,
  type DocumentBatchStatus,
} from "./api";

// maxFilesPerBatch mirrors the server-side upload batch limit.
export const maxFilesPerBatch = 10;
const maxConcurrentUploads = 2;
export const uploadBatchPollIntervalMs = 1500;
const restoreLimit = 20;

// --- Store ------------------------------------------------------------------
// The upload state lives outside React so a documents page that stays mounted
// under `hidden` keeps polling while the user browses other pages. Components
// read it through useSyncExternalStore.

export type ItemView = {
  batchId: string;
  position: number;
  name: string;
  status: DocumentBatchItemStatus;
  reason: string | null;
  error: string | null;
  jobStatus: string | null;
  jobStage: string | null;
};
export type BatchView = {
  id: string;
  status: DocumentBatchStatus;
  total: number;
  accepted: number;
  skipped: number;
  failed: number;
  transferProgress: number;
  items: ItemView[];
};
export type UploadBatchesSnapshot = {
  batches: BatchView[];
  unfinishedCount: number;
  failedItems: ItemView[];
  settled: number;
};

const emptySnapshot: UploadBatchesSnapshot = {
  batches: [],
  unfinishedCount: 0,
  failedItems: [],
  settled: 0,
};

let snapshot: UploadBatchesSnapshot = emptySnapshot;
const listeners = new Set<() => void>();

export function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function getSnapshot() {
  return snapshot;
}

function commit(batches: BatchView[]) {
  snapshot = deriveSnapshot(batches);
  listeners.forEach((listener) => listener());
}

export function isBatchTerminal(status: DocumentBatchStatus) {
  return status === "completed" || status === "failed" || status === "interrupted";
}

export function isFailedItem(item: ItemView) {
  return item.status === "failed" || item.jobStatus === "failed";
}

function isSettledItem(batch: BatchView, item: ItemView) {
  // A terminal batch never changes again, so every row it still shows counts
  // as settled even when a job detail is missing (for example interrupted).
  if (isBatchTerminal(batch.status)) return true;
  if (item.status === "skipped" || item.status === "failed") return true;
  return item.status === "accepted"
    && (item.jobStatus === "succeeded" || item.jobStatus === "failed");
}

function deriveSnapshot(batches: BatchView[]): UploadBatchesSnapshot {
  let settled = 0;
  const failedItems: ItemView[] = [];
  for (const batch of batches) {
    for (const item of batch.items) {
      if (isSettledItem(batch, item)) settled += 1;
      if (isFailedItem(item)) failedItems.push(item);
    }
  }
  return {
    batches,
    unfinishedCount: batches.filter((batch) => !isBatchTerminal(batch.status)).length,
    failedItems,
    settled,
  };
}

export type UploadPhase = "saving" | "processing" | "completed";
export type UploadSummary = {
  phase: UploadPhase;
  label: string;
  settled: number;
  total: number;
  hasFailure: boolean;
};

// uploadSummary is the single derivation behind the upload chip label. It
// returns null when the store has no batches, which is when the toolbar shows
// the plain 上传文档 button.
export function uploadSummary(state: UploadBatchesSnapshot = snapshot): UploadSummary | null {
  if (!state.batches.length) return null;
  const total = state.batches.reduce((sum, batch) => sum + batch.total, 0);
  const saving = state.batches.some((batch) => batch.status === "saving");
  const processing = state.batches.some((batch) => batch.status === "processing");
  const phase: UploadPhase = saving ? "saving" : processing ? "processing" : "completed";
  const label =
    phase === "saving"
      ? `保存中 ${state.settled}/${total}`
      : phase === "processing"
        ? `处理中 ${state.settled}/${total}`
        : `已完成 ${state.settled}/${total}`;
  return {
    phase,
    label,
    settled: state.settled,
    total,
    hasFailure:
      state.failedItems.length > 0
      || state.batches.some(
        (batch) => batch.status === "failed" || batch.status === "interrupted",
      ),
  };
}

// itemStatusText is the per-file bubble line; transfer progress is batch-wide.
export function itemStatusText(item: ItemView, transferProgress = 0) {
  if (item.status === "saving") return `上传 ${Math.round(transferProgress)}%`;
  if (item.status === "skipped") {
    if (item.reason === "exists") return "已存在，已跳过";
    if (item.reason === "queued" || item.reason === "duplicate")
      return "同名文件在处理中，已跳过";
    return "已跳过";
  }
  if (isFailedItem(item)) return `失败：${item.error || "上传失败"}`;
  if (item.jobStatus === "queued" || item.jobStatus === "processing")
    return "已受理，解析中…";
  return "已受理";
}

// --- Timers (injectable for tests) -----------------------------------------

export type UploadBatchTimers = {
  setTimeout: (handler: () => void, timeoutMs: number) => unknown;
  clearTimeout: (handle: unknown) => void;
};

const defaultTimers: UploadBatchTimers = {
  setTimeout: (handler, timeoutMs) => globalThis.setTimeout(handler, timeoutMs) as unknown,
  clearTimeout: (handle) =>
    globalThis.clearTimeout(handle as ReturnType<typeof globalThis.setTimeout>),
};
let timers: UploadBatchTimers = defaultTimers;

export function __setTimersForTests(next?: Partial<UploadBatchTimers>) {
  timers = { ...defaultTimers, ...next };
}

// --- Internal state ---------------------------------------------------------

// filesByItem keeps the original File of every row so failed items can be
// retried without asking the user to pick the file again. Restored batches
// have no local File and therefore offer no retry action.
const filesByItem = new Map<string, File>();
// serverBatchIds separates rows that came from the server (pollable) from
// local placeholders that are still transferring.
const serverBatchIds = new Set<string>();
const inflightUploads = new Map<string, AbortController>();
const inflightPolls = new Map<string, AbortController>();
let pollTimer: unknown = null;
let pollConnection: Connection | undefined;
let cancelled = false;
let settledNotified = false;
let onSettledHandler: (() => void) | undefined;
let localSequence = 0;
// trackedNamespace is the knowledge base the stored rows belong to. The Console
// remounts the documents page on a workspace switch, so bindNamespace drops the
// previous workspace's rows before the new workspace restores its own.
let trackedNamespace: string | undefined;

function itemKey(item: { batchId: string; position: number }) {
  return `${item.batchId}:${item.position}`;
}

function randomId() {
  try {
    if (typeof globalThis.crypto?.randomUUID === "function")
      return globalThis.crypto.randomUUID();
  } catch {
    // Fall through to the timestamp fallback.
  }
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

// bindNamespace resets the store when the Console switches workspace. Workspace
// switching is locked while a batch is unfinished, so any rows here are
// terminal and safe to drop; unfinished rows (should they exist after a failed
// switch) stay polled instead of being silently abandoned.
export function bindNamespace(namespace: string) {
  if (trackedNamespace === namespace) return;
  trackedNamespace = namespace;
  if (getSnapshot().batches.length === 0) return;
  if (getSnapshot().unfinishedCount > 0) return;
  serverBatchIds.clear();
  filesByItem.clear();
  if (pollTimer !== null) {
    timers.clearTimeout(pollTimer);
    pollTimer = null;
  }
  settledNotified = false;
  commit([]);
}

export function setOnSettled(handler: (() => void) | undefined) {
  onSettledHandler = handler;
}

function notifySettled() {
  if (settledNotified) return;
  settledNotified = true;
  if (pollTimer !== null) {
    timers.clearTimeout(pollTimer);
    pollTimer = null;
  }
  onSettledHandler?.();
}

function maybeSettle() {
  const state = getSnapshot();
  if (!state.batches.length) return;
  if (state.unfinishedCount > 0) {
    schedulePoll();
    return;
  }
  notifySettled();
}

function createPlaceholder(files: File[]): BatchView {
  localSequence += 1;
  const id = `local-${localSequence}`;
  const items = files.map((file, index) => {
    const item: ItemView = {
      batchId: id,
      position: index,
      name: file.name,
      status: "saving",
      reason: null,
      error: null,
      jobStatus: null,
      jobStage: null,
    };
    filesByItem.set(itemKey(item), file);
    return item;
  });
  return {
    id,
    status: "saving",
    total: files.length,
    accepted: 0,
    skipped: 0,
    failed: 0,
    transferProgress: 0,
    items,
  };
}

function viewFromServer(
  batch: DocumentBatch,
  files?: File[],
  previousId?: string,
): BatchView {
  // interrupted means the service restarted before the admissions finished:
  // those files never entered the job queue, so surface them as retryable
  // failures instead of leaving them stuck on "上传中".
  const interrupted = batch.status === "interrupted";
  const items = batch.items.map((item) => {
    const interruptedItem = interrupted && item.status === "saving";
    const view: ItemView = {
      batchId: batch.batch_id,
      position: item.position,
      name: item.name,
      status: interruptedItem ? "failed" : item.status,
      reason: interruptedItem ? "interrupted" : item.reason ?? null,
      error: interruptedItem
        ? "已中断，请重试"
        : item.error ?? item.job?.error ?? null,
      jobStatus: item.job?.status ?? null,
      jobStage: item.job?.stage ?? null,
    };
    const file = files?.[item.position]
      ?? (previousId ? filesByItem.get(`${previousId}:${item.position}`) : undefined);
    if (file) filesByItem.set(itemKey(view), file);
    return view;
  });
  return {
    id: batch.batch_id,
    status: batch.status,
    total: batch.total,
    accepted: batch.accepted,
    skipped: batch.skipped,
    failed: batch.failed,
    transferProgress: 100,
    items,
  };
}

function applyServerBatch(
  batch: DocumentBatch,
  options: { files?: File[]; previousId?: string } = {},
) {
  serverBatchIds.add(batch.batch_id);
  const current = getSnapshot().batches;
  const nextView = viewFromServer(batch, options.files, options.previousId);
  const serverIndex = current.findIndex((view) => view.id === batch.batch_id);
  const placeholderIndex = options.previousId
    ? current.findIndex((view) => view.id === options.previousId)
    : -1;
  // A replayed Idempotency-Key returns the original batch object: replace the
  // existing row and drop the local placeholder instead of counting it twice.
  if (serverIndex >= 0) {
    const previousView = current[serverIndex];
    const next = current.slice();
    next[serverIndex] = {
      ...nextView,
      transferProgress: Math.max(previousView.transferProgress, nextView.transferProgress),
    };
    if (placeholderIndex >= 0 && placeholderIndex !== serverIndex) {
      const placeholderView = current[placeholderIndex];
      next.splice(placeholderIndex, 1);
      placeholderView.items.forEach((item) => filesByItem.delete(itemKey(item)));
    }
    commit(next);
    maybeSettle();
    return;
  }
  if (placeholderIndex >= 0) {
    const placeholderView = current[placeholderIndex];
    const next = current.slice();
    next[placeholderIndex] = nextView;
    commit(next);
    if (placeholderView.id !== nextView.id) {
      placeholderView.items.forEach((item) => filesByItem.delete(itemKey(item)));
    }
    maybeSettle();
    return;
  }
  commit([...current, nextView]);
  maybeSettle();
}

function updateTransferProgress(batchId: string, percent: number) {
  const clamped = Math.min(100, Math.max(0, Math.round(percent)));
  let changed = false;
  const next = getSnapshot().batches.map((batch) => {
    if (batch.id !== batchId || batch.transferProgress === clamped) return batch;
    changed = true;
    return { ...batch, transferProgress: clamped };
  });
  if (changed) commit(next);
}

function markBatchFailed(batchId: string, error: string) {
  const next = getSnapshot().batches.map((batch) => {
    if (batch.id !== batchId) return batch;
    return {
      ...batch,
      status: "failed" as const,
      failed: batch.items.length,
      items: batch.items.map((item) => ({
        ...item,
        status: "failed" as const,
        reason: "network",
        error,
      })),
    };
  });
  commit(next);
  maybeSettle();
}

// A 404 means the server no longer tracks the batch (pruned or foreign
// namespace), so its status can never resolve: record it as interrupted
// instead of polling forever. Files that never reached the queue become
// retryable failures.
function markBatchInterrupted(batchId: string) {
  let changed = false;
  const next = getSnapshot().batches.map((batch) => {
    if (batch.id !== batchId || isBatchTerminal(batch.status)) return batch;
    changed = true;
    return {
      ...batch,
      status: "interrupted" as const,
      items: batch.items.map((item) =>
        item.status === "saving"
          ? { ...item, status: "failed" as const, reason: "interrupted", error: "已中断，请重试" }
          : item,
      ),
    };
  });
  if (!changed) return;
  commit(next);
  maybeSettle();
}

function removeItem(target: ItemView) {
  let changed = false;
  const next = getSnapshot().batches.map((batch) => {
    if (batch.id !== target.batchId) return batch;
    const items = batch.items.filter(
      (item) => item.batchId !== target.batchId || item.position !== target.position,
    );
    if (items.length === batch.items.length) return batch;
    changed = true;
    return { ...batch, items, total: Math.max(0, batch.total - 1) };
  });
  if (!changed) return;
  filesByItem.delete(itemKey(target));
  commit(next);
}

// --- Polling ----------------------------------------------------------------

function hasPollableUnfinished() {
  return getSnapshot().batches.some(
    (batch) => serverBatchIds.has(batch.id) && !isBatchTerminal(batch.status),
  );
}

function schedulePoll() {
  if (cancelled || pollTimer !== null || !pollConnection) return;
  if (!hasPollableUnfinished()) return;
  pollTimer = timers.setTimeout(() => {
    pollTimer = null;
    void pollRound().catch(() => undefined);
  }, uploadBatchPollIntervalMs);
}

async function pollRound() {
  const connection = pollConnection;
  const pending = getSnapshot().batches.filter(
    (batch) => serverBatchIds.has(batch.id) && !isBatchTerminal(batch.status),
  );
  if (!connection || !pending.length) {
    if (getSnapshot().unfinishedCount === 0) notifySettled();
    return;
  }
  await Promise.all(
    pending.map(async (batch) => {
      const controller = new AbortController();
      inflightPolls.set(batch.id, controller);
      try {
        const data = await getDocumentBatch(batch.id, connection);
        if (!cancelled) applyServerBatch(data);
      } catch (error) {
        if (!cancelled && isNotFoundError(error)) markBatchInterrupted(batch.id);
        // Any other transient status failure keeps the batch polling.
      } finally {
        inflightPolls.delete(batch.id);
      }
    }),
  );
  if (cancelled) return;
  if (getSnapshot().unfinishedCount > 0) schedulePoll();
  else notifySettled();
}

// poll runs one status round immediately; uploadBatches schedules the same
// round every 1500 ms while a batch is unfinished.
export async function poll(connection: Connection) {
  pollConnection = connection;
  await pollRound();
}

// --- Upload queue -----------------------------------------------------------

async function submitChunk(files: File[], placeholder: BatchView, connection: Connection) {
  const controller = new AbortController();
  inflightUploads.set(placeholder.id, controller);
  try {
    const batch = await submitDocumentBatch(files, connection, {
      signal: controller.signal,
      idempotencyKey: randomId(),
      onProgress: (percent) => {
        if (!cancelled) updateTransferProgress(placeholder.id, percent);
      },
    });
    if (cancelled) return;
    applyServerBatch(batch, { files, previousId: placeholder.id });
    schedulePoll();
  } catch (error) {
    if (cancelled) return;
    markBatchFailed(placeholder.id, errorText(error));
  } finally {
    inflightUploads.delete(placeholder.id);
  }
}

// uploadFiles splits the selection into server-sized batches, uploads at most
// two batches at a time, and starts polling every accepted batch.
export async function uploadFiles(files: File[], connection: Connection): Promise<void> {
  if (!files.length) return;
  cancelled = false;
  settledNotified = false;
  pollConnection = connection;
  const chunks: File[][] = [];
  for (let index = 0; index < files.length; index += maxFilesPerBatch)
    chunks.push(files.slice(index, index + maxFilesPerBatch));
  const placeholders = chunks.map((chunk) => createPlaceholder(chunk));
  commit([...getSnapshot().batches, ...placeholders]);

  let next = 0;
  const workers = Array.from(
    { length: Math.min(maxConcurrentUploads, chunks.length) },
    async () => {
      while (true) {
        const index = next;
        next += 1;
        if (index >= chunks.length) return;
        await submitChunk(chunks[index], placeholders[index], connection);
      }
    },
  );
  await Promise.all(workers);
  maybeSettle();
}

// restore recovers batches that still need attention after a page reload
// (saving / processing / interrupted). It never touches batches this tab
// already tracks and never restores completed or failed history.
export async function restore(connection: Connection) {
  cancelled = false;
  let batches: DocumentBatch[];
  try {
    batches = await listActiveDocumentBatches(connection, restoreLimit);
  } catch {
    // Restoration is best effort: the documents page still loads without it.
    return;
  }
  if (cancelled) return;
  const current = getSnapshot().batches;
  const known = new Set(current.map((batch) => batch.id));
  const restored: BatchView[] = [];
  for (const batch of batches) {
    if (known.has(batch.batch_id)) continue;
    if (batch.status === "completed" || batch.status === "failed") continue;
    known.add(batch.batch_id);
    serverBatchIds.add(batch.batch_id);
    restored.push(viewFromServer(batch));
  }
  if (!restored.length) return;
  pollConnection = connection;
  settledNotified = false;
  commit([...current, ...restored]);
  schedulePoll();
}

// retryFailedItem resubmits exactly one failed file as a fresh single-file
// batch. The failed row is replaced by the retry batch so the bubble never
// lists the same file twice.
export async function retryFailedItem(item: ItemView, connection: Connection) {
  const file = filesByItem.get(itemKey(item));
  if (!file) throw new Error("缺少原始文件，无法重试");
  cancelled = false;
  settledNotified = false;
  pollConnection = connection;
  removeItem(item);
  const placeholder = createPlaceholder([file]);
  commit([...getSnapshot().batches, placeholder]);
  await submitChunk([file], placeholder, connection);
  maybeSettle();
}

export function canRetryItem(item: ItemView) {
  return filesByItem.has(itemKey(item));
}

export function clearFinished() {
  const current = getSnapshot().batches;
  const next = current.filter((batch) => !isBatchTerminal(batch.status));
  if (next.length === current.length) return;
  current.forEach((batch) => {
    if (!isBatchTerminal(batch.status)) return;
    serverBatchIds.delete(batch.id);
    batch.items.forEach((item) => filesByItem.delete(itemKey(item)));
  });
  commit(next);
}

// cancelAll stops in-flight transfers and polling. Leaving the documents page
// does not call it: uploads keep running while another Console page is shown.
export function cancelAll() {
  cancelled = true;
  inflightUploads.forEach((controller) => controller.abort());
  inflightUploads.clear();
  inflightPolls.forEach((controller) => controller.abort());
  inflightPolls.clear();
  if (pollTimer !== null) {
    timers.clearTimeout(pollTimer);
    pollTimer = null;
  }
}

export function __resetForTests() {
  cancelled = false;
  settledNotified = false;
  pollConnection = undefined;
  onSettledHandler = undefined;
  if (pollTimer !== null) {
    timers.clearTimeout(pollTimer);
    pollTimer = null;
  }
  inflightUploads.clear();
  inflightPolls.clear();
  serverBatchIds.clear();
  filesByItem.clear();
  localSequence = 0;
  timers = defaultTimers;
  snapshot = emptySnapshot;
  listeners.clear();
}
