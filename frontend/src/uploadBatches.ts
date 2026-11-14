import {
  errorText,
  getDocumentBatch,
  isNotFoundError,
  listActiveDocumentBatches,
  submitDocumentBatch,
  subscribeUploadProgress,
  type Connection,
  type DocumentBatch,
  type DocumentBatchItem,
  type DocumentBatchItemStatus,
  type DocumentBatchStatus,
  type UploadProgressEvent,
  type UploadProgressSubscription,
  type UploadResultStatus,
} from "./api";

// maxFilesPerBatch mirrors the server-side upload batch limit.
export const maxFilesPerBatch = 10;
const maxConcurrentUploads = 2;
export const uploadBatchPollIntervalMs = 1500;
// uploadReadyTimeoutMs bounds how long one upload waits for the SSE `ready`
// frame; after that the POST still goes out and the panel relies on the
// response (plus any events that do arrive).
export const uploadReadyTimeoutMs = 5000;
const restoreLimit = 20;
// fallbackError is shown when neither SSE nor the batch response reported a
// save result for a row.
const fallbackError = "未收到保存结果，请重试";

// --- Store ------------------------------------------------------------------
// The upload state lives outside React so a documents page that stays mounted
// under `hidden` keeps polling while the user browses other pages. Components
// read it through useSyncExternalStore.

export type ItemView = {
  batchId: string;
  position: number;
  name: string;
  // saving = bytes still transferring or save result still unknown; the other
  // three are definitive per-file save outcomes.
  status: DocumentBatchItemStatus;
  reason: string | null;
  error: string | null;
  receivedBytes: number;
  totalBytes: number;
  // transferComplete means the request body of this file was fully read; the
  // row is then waiting for its save result (保存中…).
  transferComplete: boolean;
};
export type BatchView = {
  id: string;
  status: DocumentBatchStatus;
  total: number;
  accepted: number;
  skipped: number;
  failed: number;
  // uploadId is the client-generated request ID of this batch; restored
  // batches have none. Rows are always matched by (uploadId, position), never
  // by file name, so two same-named files cannot share progress.
  uploadId: string | null;
  items: ItemView[];
};
export type UploadBatchesSnapshot = {
  batches: BatchView[];
  // unfinishedCount counts batches that still have a row without a result.
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
  return item.status === "failed";
}

function hasSavingItem(batch: BatchView) {
  return batch.items.some((item) => item.status === "saving");
}

// isUploadSettled is the upload-panel notion of "done": every row has a save
// result. It is deliberately independent of the batch status because the panel
// no longer tracks the background parsing stage.
export function isUploadSettled(batch: BatchView) {
  return !hasSavingItem(batch);
}

function deriveSnapshot(batches: BatchView[]): UploadBatchesSnapshot {
  let settled = 0;
  const failedItems: ItemView[] = [];
  let unfinishedCount = 0;
  for (const batch of batches) {
    if (hasSavingItem(batch)) unfinishedCount += 1;
    for (const item of batch.items) {
      if (item.status !== "saving") settled += 1;
      if (isFailedItem(item)) failedItems.push(item);
    }
  }
  return {
    batches,
    unfinishedCount,
    failedItems,
    settled,
  };
}

export type UploadPhase = "saving" | "completed";
export type UploadSummary = {
  phase: UploadPhase;
  label: string;
  settled: number;
  total: number;
  hasFailure: boolean;
};

// uploadSummary is the single derivation behind the upload chip label. It
// returns null when the store has no batches, which is when the toolbar shows
// the plain 上传文档 button. The panel ends at the save result, so there is no
// 处理中 phase anymore.
export function uploadSummary(state: UploadBatchesSnapshot = snapshot): UploadSummary | null {
  if (!state.batches.length) return null;
  const total = state.batches.reduce((sum, batch) => sum + batch.total, 0);
  const saving = state.unfinishedCount > 0;
  const phase: UploadPhase = saving ? "saving" : "completed";
  const label = phase === "saving"
    ? `保存中 ${state.settled}/${total}`
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

// perFileProgress maps one row to its progress-bar percentage. The server never
// sends a file total, so the local File.size is the denominator; a row with a
// save result (or fully transferred bytes) is 100%.
export function perFileProgress(item: ItemView) {
  if (item.transferComplete || item.status !== "saving") return 100;
  if (item.totalBytes <= 0) return 0;
  if (item.receivedBytes >= item.totalBytes) return 100;
  const percent = Math.round((item.receivedBytes / item.totalBytes) * 100);
  return Math.min(100, Math.max(0, percent));
}

// itemStatusText is the per-file bubble line below the progress bar.
export function itemStatusText(item: ItemView) {
  if (item.status === "saving")
    return item.transferComplete ? "保存中…" : "上传中…";
  if (item.status === "skipped") {
    if (item.reason === "exists") return "已存在，已跳过";
    if (item.reason === "queued" || item.reason === "duplicate")
      return "同名文件在处理中，已跳过";
    return "已跳过";
  }
  if (item.status === "failed") return `保存失败：${item.error || "上传失败"}`;
  return "已保存";
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

function createPlaceholder(files: File[], uploadId: string): BatchView {
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
      receivedBytes: 0,
      totalBytes: file.size,
      transferComplete: false,
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
    uploadId,
    items,
  };
}

// viewFromServer merges one server batch projection with the rows this tab
// already tracks. `finalize` distinguishes the two callers:
//  - POST response (finalize): every row must end with a definitive result;
//    a row the response still reports as saving (or omits entirely) becomes a
//    retryable failure — unless SSE already delivered its result.
//  - restore/poll (not finalize): rows still saving stay saving so the panel
//    keeps polling until their result arrives.
function viewFromServer(
  batch: DocumentBatch,
  options: {
    files?: File[];
    uploadId?: string | null;
    previous?: BatchView;
    finalize?: boolean;
  } = {},
): BatchView {
  const finalize = options.finalize ?? false;
  const interrupted = batch.status === "interrupted";
  const previousItems = new Map<number, ItemView>();
  options.previous?.items.forEach((item) => previousItems.set(item.position, item));
  const localFiles = new Map<number, File>();
  options.files?.forEach((file, index) => localFiles.set(index, file));

  const serverItems = new Map<number, DocumentBatchItem>();
  (batch.items ?? []).forEach((item) => serverItems.set(item.position, item));

  const positions = new Set<number>();
  serverItems.forEach((_item, position) => positions.add(position));
  previousItems.forEach((_item, position) => positions.add(position));

  const items = [...positions].sort((left, right) => left - right).map((position) => {
    const serverItem = serverItems.get(position);
    const previous = previousItems.get(position);
    // Carry the local File across a placeholder -> server-batch replacement so
    // failed rows stay retryable.
    const file = localFiles.get(position)
      ?? (previous ? filesByItem.get(itemKey(previous)) : undefined);
    const totalBytes = previous?.totalBytes ?? file?.size ?? 0;
    const previousResult = previous && previous.status !== "saving" ? previous : undefined;
    let status: DocumentBatchItemStatus;
    let reason: string | null;
    let error: string | null;
    if (serverItem && serverItem.status !== "saving") {
      status = serverItem.status;
      reason = serverItem.reason ?? null;
      error = serverItem.error ?? serverItem.job?.error ?? null;
    } else if (interrupted && serverItem) {
      // interrupted means the service restarted before admissions finished:
      // those files never entered the job queue, so surface them as retryable
      // failures instead of leaving them stuck on 保存中.
      status = "failed";
      reason = "interrupted";
      error = "已中断，请重试";
    } else if (!finalize && serverItem) {
      status = "saving";
      reason = null;
      error = null;
    } else if (previousResult) {
      // Never overwrite a result SSE already delivered with a failure.
      status = previousResult.status;
      reason = previousResult.reason;
      error = previousResult.error;
    } else {
      status = "failed";
      reason = "network";
      error = fallbackError;
    }
    const item: ItemView = {
      batchId: batch.batch_id,
      position,
      name: serverItem?.name ?? previous?.name ?? file?.name ?? "未命名文件",
      status,
      reason,
      error,
      // A server batch row only exists once the request bytes were read, so
      // the transfer is complete even while a save result is still missing.
      receivedBytes: totalBytes,
      totalBytes,
      transferComplete: true,
    };
    if (file) filesByItem.set(itemKey(item), file);
    return item;
  });

  return {
    id: batch.batch_id,
    status: batch.status,
    total: batch.total,
    accepted: batch.accepted,
    skipped: batch.skipped,
    failed: batch.failed,
    uploadId: options.uploadId ?? options.previous?.uploadId ?? null,
    items,
  };
}

function applyServerBatch(
  batch: DocumentBatch,
  options: {
    files?: File[];
    previousId?: string;
    uploadId?: string | null;
    finalize?: boolean;
  } = {},
) {
  serverBatchIds.add(batch.batch_id);
  const current = getSnapshot().batches;
  const serverIndex = current.findIndex((view) => view.id === batch.batch_id);
  const placeholderIndex = options.previousId
    ? current.findIndex((view) => view.id === options.previousId)
    : -1;
  const previous = (placeholderIndex >= 0 ? current[placeholderIndex] : undefined)
    ?? (serverIndex >= 0 ? current[serverIndex] : undefined);
  const nextView = viewFromServer(batch, {
    files: options.files,
    uploadId: options.uploadId ?? previous?.uploadId ?? null,
    previous,
    finalize: options.finalize,
  });
  // A replayed Idempotency-Key returns the original batch object: replace the
  // existing row and drop the local placeholder instead of counting it twice.
  if (serverIndex >= 0) {
    const next = current.slice();
    next[serverIndex] = nextView;
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

// --- SSE event application --------------------------------------------------

function resultStatus(status: UploadResultStatus): DocumentBatchItemStatus {
  return status === "saved" ? "accepted" : status;
}

function applyProgressEvent(uploadId: string, index: number, received: number) {
  if (!Number.isInteger(index) || index < 0) return;
  if (!Number.isFinite(received) || received < 0) return;
  let changed = false;
  const next = getSnapshot().batches.map((batch) => {
    if (batch.uploadId !== uploadId) return batch;
    const itemIndex = batch.items.findIndex((item) => item.position === index);
    if (itemIndex < 0) return batch;
    const item = batch.items[itemIndex];
    // Progress is transfer-only: a row with a result never goes backwards.
    if (item.status !== "saving") return batch;
    const clamped = item.totalBytes > 0
      ? Math.min(received, item.totalBytes)
      : received;
    const receivedBytes = Math.max(item.receivedBytes, clamped);
    const transferComplete = item.totalBytes <= 0 || receivedBytes >= item.totalBytes;
    if (receivedBytes === item.receivedBytes && transferComplete === item.transferComplete)
      return batch;
    changed = true;
    const items = batch.items.slice();
    items[itemIndex] = { ...item, receivedBytes, transferComplete };
    return { ...batch, items };
  });
  if (changed) commit(next);
}

function applyResultEvent(
  uploadId: string,
  index: number,
  status: UploadResultStatus,
  reason: string | null,
  error: string | null,
) {
  if (!Number.isInteger(index) || index < 0) return;
  let changed = false;
  const nextStatus = resultStatus(status);
  const next = getSnapshot().batches.map((batch) => {
    if (batch.uploadId !== uploadId) return batch;
    const itemIndex = batch.items.findIndex((item) => item.position === index);
    if (itemIndex < 0) return batch;
    const item = batch.items[itemIndex];
    if (
      item.status === nextStatus
      && item.reason === reason
      && item.error === error
      && item.transferComplete
    ) {
      return batch;
    }
    changed = true;
    const items = batch.items.slice();
    items[itemIndex] = {
      ...item,
      status: nextStatus,
      reason,
      error,
      receivedBytes: item.totalBytes,
      transferComplete: true,
    };
    return { ...batch, items };
  });
  if (changed) commit(next);
}

// handleUploadEvent maps one SSE event onto the batch identified by its
// uploadId. (uploadId, position) is the only row identity, so same-named files
// in one batch never cross-update each other.
function handleUploadEvent(uploadId: string, event: UploadProgressEvent) {
  if (event.type === "progress") {
    applyProgressEvent(uploadId, event.index, event.received);
    return;
  }
  if (event.type === "result") {
    applyResultEvent(uploadId, event.index, event.status, event.reason, event.error);
  }
}

// --- Polling ----------------------------------------------------------------

function hasPollableUnfinished() {
  return getSnapshot().batches.some(
    (batch) => serverBatchIds.has(batch.id) && hasSavingItem(batch),
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
    (batch) => serverBatchIds.has(batch.id) && hasSavingItem(batch),
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
// round every 1500 ms while a batch still has rows without results.
export async function poll(connection: Connection) {
  pollConnection = connection;
  await pollRound();
}

// --- Upload queue -----------------------------------------------------------

// awaitReady races the subscription's ready promise against a deadline; a
// failed or slow subscription must never block the upload itself.
async function awaitReady(ready: Promise<void>) {
  let timer: unknown = null;
  const timeout = new Promise<void>((resolve) => {
    timer = timers.setTimeout(resolve, uploadReadyTimeoutMs);
  });
  try {
    await Promise.race([ready.catch(() => undefined), timeout]);
  } finally {
    if (timer !== null) timers.clearTimeout(timer);
  }
}

async function submitChunk(files: File[], placeholder: BatchView, connection: Connection) {
  const controller = new AbortController();
  inflightUploads.set(placeholder.id, controller);
  const uploadId = placeholder.uploadId;
  let subscription: UploadProgressSubscription | undefined;
  try {
    if (uploadId) {
      try {
        subscription = subscribeUploadProgress(
          uploadId,
          connection,
          (event) => {
            if (!cancelled) handleUploadEvent(uploadId, event);
          },
          { signal: controller.signal },
        );
      } catch {
        // A subscription that cannot even be opened falls back to the POST
        // response, exactly like a rejected ready promise.
        subscription = undefined;
      }
      if (subscription) await awaitReady(subscription.ready);
    }
    if (cancelled) return;
    const batch = await submitDocumentBatch(files, connection, {
      signal: controller.signal,
      idempotencyKey: randomId(),
      uploadId: uploadId ?? undefined,
    });
    if (cancelled) return;
    // The response is the final word: it reconciles lost/missing events and
    // never leaves a row without a result.
    applyServerBatch(batch, {
      files,
      previousId: placeholder.id,
      uploadId,
      finalize: true,
    });
  } catch (error) {
    if (cancelled) return;
    markBatchFailed(placeholder.id, errorText(error));
  } finally {
    subscription?.close();
    inflightUploads.delete(placeholder.id);
  }
}

// uploadFiles splits the selection into server-sized batches, uploads at most
// two batches at a time, and tracks per-file progress through the SSE stream of
// each request. The panel ends at the save result; nothing polls the parsing
// stage afterwards.
export async function uploadFiles(files: File[], connection: Connection): Promise<void> {
  if (!files.length) return;
  cancelled = false;
  settledNotified = false;
  pollConnection = connection;
  const chunks: File[][] = [];
  for (let index = 0; index < files.length; index += maxFilesPerBatch)
    chunks.push(files.slice(index, index + maxFilesPerBatch));
  const placeholders = chunks.map((chunk) => createPlaceholder(chunk, randomId()));
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

function markBatchFailed(batchId: string, error: string) {
  let changed = false;
  const next = getSnapshot().batches.map((batch) => {
    if (batch.id !== batchId) return batch;
    const items = batch.items.map((item) =>
      item.status === "saving"
        ? {
          ...item,
          status: "failed" as const,
          reason: "network",
          error,
          transferComplete: true,
        }
        : item);
    if (items.every((item, index) => item === batch.items[index])) return batch;
    changed = true;
    return {
      ...batch,
      status: "failed" as const,
      failed: items.filter((item) => item.status === "failed").length,
      items,
    };
  });
  if (!changed) return;
  commit(next);
  maybeSettle();
}

// markBatchInterrupted handles a 404 from the status endpoint: the server no
// longer tracks the batch, so rows that never got a result cannot resolve
// anymore and become retryable failures.
function markBatchInterrupted(batchId: string) {
  let changed = false;
  const next = getSnapshot().batches.map((batch) => {
    if (batch.id !== batchId || isBatchTerminal(batch.status)) return batch;
    const items = batch.items.map((item) =>
      item.status === "saving"
        ? { ...item, status: "failed" as const, reason: "interrupted", error: "已中断，请重试" }
        : item);
    if (items.every((item, index) => item === batch.items[index])) return batch;
    changed = true;
    return {
      ...batch,
      status: "interrupted" as const,
      failed: items.filter((item) => item.status === "failed").length,
      items,
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

// restore recovers batches that still need attention after a page reload
// (saving / interrupted). It never touches batches this tab already tracks and
// never restores completed, failed or background parsing batches.
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
    // Only batches whose save outcome is still missing belong to the upload
    // panel now; processing/completed rows live in the document list instead.
    if (batch.status !== "saving" && batch.status !== "interrupted") continue;
    known.add(batch.batch_id);
    serverBatchIds.add(batch.batch_id);
    restored.push(viewFromServer(batch, { finalize: false }));
  }
  if (!restored.length) return;
  pollConnection = connection;
  settledNotified = false;
  commit([...current, ...restored]);
  maybeSettle();
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
  const placeholder = createPlaceholder([file], randomId());
  commit([...getSnapshot().batches, placeholder]);
  await submitChunk([file], placeholder, connection);
  maybeSettle();
}

export function canRetryItem(item: ItemView) {
  return filesByItem.has(itemKey(item));
}

export function clearFinished() {
  const current = getSnapshot().batches;
  const next = current.filter((batch) => !isUploadSettled(batch));
  if (next.length === current.length) return;
  current.forEach((batch) => {
    if (!isUploadSettled(batch)) return;
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
  trackedNamespace = undefined;
}
