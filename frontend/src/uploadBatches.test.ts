import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";
import type {
  Connection,
  DocumentBatch,
  DocumentBatchItem,
  UploadProgressEvent,
} from "./api";

// Keep the real errorText()/type helpers, replace only the network calls; the
// upload store itself must never reach the network in tests.
vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    submitDocumentBatch: vi.fn(),
    getDocumentBatch: vi.fn(),
    listActiveDocumentBatches: vi.fn(),
    subscribeUploadProgress: vi.fn(),
  };
});

import {
  getDocumentBatch,
  listActiveDocumentBatches,
  submitDocumentBatch,
  subscribeUploadProgress,
} from "./api";
import {
  __resetForTests,
  bindNamespace,
  canRetryItem,
  clearFinished,
  getSnapshot,
  isFailedItem,
  isUploadSettled,
  itemStatusText,
  maxFilesPerBatch,
  perFileProgress,
  restore,
  retryFailedItem,
  setOnSettled,
  uploadBatchPollIntervalMs,
  uploadFiles,
  uploadReadyTimeoutMs,
  uploadSummary,
} from "./uploadBatches";

const connection: Connection = { namespace: "team-kb", token: "" };

// makeFile builds a File with a known size so progress percentages are exact.
function makeFile(name: string, size = 7) {
  return new File([new Uint8Array(size)], name, { type: "application/octet-stream" });
}

function serverItem(
  position: number,
  overrides: Partial<DocumentBatchItem> = {},
): DocumentBatchItem {
  return {
    position,
    name: `file-${position}.md`,
    status: "accepted",
    reason: null,
    artifact_id: `artifact-${position}`,
    error: null,
    job: { status: "queued", stage: "queued", error: null },
    ...overrides,
  };
}

function serverBatch(overrides: Partial<DocumentBatch> = {}): DocumentBatch {
  const items = overrides.items ?? [serverItem(0)];
  return {
    batch_id: "batch-1",
    namespace: "team-kb",
    status: "processing",
    total: items.length,
    accepted: items.filter((item) => item.status === "accepted").length,
    skipped: items.filter((item) => item.status === "skipped").length,
    failed: items.filter((item) => item.status === "failed").length,
    created_at: 1762934400.5,
    items,
    ...overrides,
  };
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

// The upload store's own promises (ready -> POST, POST -> apply) hop through a
// bounded number of microtasks; flushing them keeps the tests synchronous with
// fake timers.
async function flushMicrotasks() {
  for (let index = 0; index < 30; index += 1) await Promise.resolve();
}

type Subscriber = {
  uploadId: string;
  connection: Connection | undefined;
  emit: (event: UploadProgressEvent) => void;
  close: Mock<() => void>;
  resolveReady: () => void;
  rejectReady: (error: Error) => void;
};

let subscribers: Subscriber[] = [];

// installSubscriptionMock replaces the SSE client. autoReady=false keeps the
// subscription pending so a test can assert "subscribe first, POST later".
function installSubscriptionMock(options: { autoReady?: boolean } = {}) {
  const autoReady = options.autoReady ?? true;
  vi.mocked(subscribeUploadProgress).mockImplementation((uploadId, conn, onEvent) => {
    let resolveReady = () => {};
    let rejectReady = (_error: Error) => {};
    const ready = autoReady
      ? Promise.resolve()
      : new Promise<void>((resolve, reject) => {
        resolveReady = resolve;
        rejectReady = reject;
      });
    ready.catch(() => undefined);
    const close = vi.fn<() => void>();
    const subscriber: Subscriber = {
      uploadId,
      connection: conn,
      emit: onEvent,
      close,
      resolveReady: () => resolveReady(),
      rejectReady: (error) => rejectReady(error),
    };
    subscribers.push(subscriber);
    return { ready, close: () => close() };
  });
}

function latestSubscriber() {
  const subscriber = subscribers[subscribers.length - 1];
  if (!subscriber) throw new Error("no upload subscription was opened");
  return subscriber;
}

beforeEach(() => {
  __resetForTests();
  vi.useFakeTimers();
  subscribers = [];
  vi.mocked(submitDocumentBatch).mockReset();
  vi.mocked(getDocumentBatch).mockReset();
  vi.mocked(listActiveDocumentBatches).mockReset();
  vi.mocked(subscribeUploadProgress).mockReset();
  installSubscriptionMock();
  // Default: one accepted item per submitted file.
  vi.mocked(submitDocumentBatch).mockImplementation(async (files) =>
    serverBatch({
      batch_id: "batch-auto",
      items: files.map((_file, index) => serverItem(index)),
    }));
});

afterEach(() => {
  __resetForTests();
  vi.clearAllTimers();
  vi.useRealTimers();
});

describe("uploadFiles", () => {
  it("subscribes (and waits for ready) before POSTing with the same upload id", async () => {
    installSubscriptionMock({ autoReady: false });
    const upload = uploadFiles([makeFile("a.md")], connection);
    await flushMicrotasks();

    const subscriber = latestSubscriber();
    expect(vi.mocked(submitDocumentBatch)).not.toHaveBeenCalled();
    // The placeholder row is already visible at 0% while ready is pending.
    const placeholder = getSnapshot().batches[0];
    expect(placeholder.uploadId).toBe(subscriber.uploadId);
    expect(placeholder.items[0].status).toBe("saving");
    expect(perFileProgress(placeholder.items[0])).toBe(0);
    expect(itemStatusText(placeholder.items[0])).toBe("上传中…");

    subscriber.resolveReady();
    await flushMicrotasks();
    expect(vi.mocked(submitDocumentBatch)).toHaveBeenCalledTimes(1);
    const options = vi.mocked(submitDocumentBatch).mock.calls[0][2];
    expect(options.uploadId).toBeTruthy();
    expect(options.uploadId).toBe(subscriber.uploadId);
    expect(vi.mocked(subscribeUploadProgress).mock.invocationCallOrder[0])
      .toBeLessThan(vi.mocked(submitDocumentBatch).mock.invocationCallOrder[0]);

    await upload;
    expect(getSnapshot().batches[0].items[0].status).toBe("accepted");
    // The subscription is closed as soon as the POST settled.
    expect(subscriber.close).toHaveBeenCalled();
  });

  it("splits a 20-file selection into two uploads with distinct upload ids", async () => {
    let submitted = 0;
    vi.mocked(submitDocumentBatch).mockImplementation(async (files) => {
      submitted += 1;
      return serverBatch({
        batch_id: `batch-${submitted}`,
        items: files.map((_file, index) => serverItem(index)),
      });
    });

    const files = Array.from({ length: 20 }, (_unused, index) => makeFile(`file-${index}.md`));
    await uploadFiles(files, connection);

    const calls = vi.mocked(submitDocumentBatch).mock.calls;
    expect(calls).toHaveLength(2);
    expect(calls[0][0]).toHaveLength(maxFilesPerBatch);
    expect(calls[1][0]).toHaveLength(maxFilesPerBatch);
    expect(calls[0][2].idempotencyKey).toBeTruthy();
    expect(calls[0][2].idempotencyKey).not.toBe(calls[1][2].idempotencyKey);
    const uploadIds = calls.map((call) => call[2].uploadId);
    expect(uploadIds[0]).toBeTruthy();
    expect(uploadIds[0]).not.toBe(uploadIds[1]);
    expect(vi.mocked(subscribeUploadProgress).mock.calls.map((call) => call[0]))
      .toEqual(uploadIds);
    expect(getSnapshot().batches.map((batch) => batch.id)).toEqual([
      "batch-1",
      "batch-2",
    ]);
  });

  it("updates each row by (uploadId, position) even when two files share a name", async () => {
    const submit = deferred<DocumentBatch>();
    vi.mocked(submitDocumentBatch).mockReturnValueOnce(submit.promise);
    const upload = uploadFiles(
      [makeFile("same.md", 200), makeFile("same.md", 100)],
      connection,
    );
    await flushMicrotasks();
    const subscriber = latestSubscriber();

    subscriber.emit({ type: "progress", index: 0, received: 100 });
    let items = getSnapshot().batches[0].items;
    expect(perFileProgress(items[0])).toBe(50);
    expect(perFileProgress(items[1])).toBe(0);
    expect(itemStatusText(items[0])).toBe("上传中…");

    subscriber.emit({ type: "progress", index: 1, received: 50 });
    items = getSnapshot().batches[0].items;
    // index 1 must never move the first same-named row.
    expect(perFileProgress(items[0])).toBe(50);
    expect(perFileProgress(items[1])).toBe(50);

    subscriber.emit({ type: "progress", index: 0, received: 200 });
    items = getSnapshot().batches[0].items;
    expect(perFileProgress(items[0])).toBe(100);
    expect(items[0].transferComplete).toBe(true);
    expect(itemStatusText(items[0])).toBe("保存中…");
    expect(itemStatusText(items[1])).toBe("上传中…");

    // A replayed/out-of-order progress frame never regresses the row.
    subscriber.emit({ type: "progress", index: 0, received: 10 });
    expect(perFileProgress(getSnapshot().batches[0].items[0])).toBe(100);

    submit.resolve(serverBatch({
      batch_id: "batch-progress",
      total: 2,
      items: [
        serverItem(0, { name: "same.md" }),
        serverItem(1, {
          name: "same.md",
          status: "failed",
          reason: "validation",
          artifact_id: null,
          error: "不支持的文件类型",
          job: undefined,
        }),
      ],
    }));
    await upload;

    const finished = getSnapshot().batches[0];
    expect(finished.id).toBe("batch-progress");
    expect(finished.items.map((item) => item.status)).toEqual(["accepted", "failed"]);
    expect(finished.items.map((item) => perFileProgress(item))).toEqual([100, 100]);
    expect(itemStatusText(finished.items[0])).toBe("已保存");
    expect(itemStatusText(finished.items[1])).toBe("保存失败：不支持的文件类型");
    expect(uploadSummary()).toMatchObject({
      phase: "completed",
      label: "已完成 2/2",
      hasFailure: true,
    });
  });

  it("applies per-file results without leaking one failure into other rows", async () => {
    const submit = deferred<DocumentBatch>();
    vi.mocked(submitDocumentBatch).mockReturnValueOnce(submit.promise);
    const upload = uploadFiles(
      [makeFile("ok.md", 10), makeFile("bad.md", 10)],
      connection,
    );
    await flushMicrotasks();
    const subscriber = latestSubscriber();

    subscriber.emit({ type: "progress", index: 0, received: 10 });
    subscriber.emit({ type: "progress", index: 1, received: 10 });
    subscriber.emit({ type: "result", index: 0, status: "saved", reason: null, error: null });
    expect(itemStatusText(getSnapshot().batches[0].items[0])).toBe("已保存");

    // The POST fails later: only the row without a result becomes a failure.
    submit.reject(new Error("POST /v1/document-batches\nHTTP 500：服务不可用"));
    await upload;

    const batch = getSnapshot().batches[0];
    expect(batch.status).toBe("failed");
    expect(batch.items[0].status).toBe("accepted");
    expect(itemStatusText(batch.items[0])).toBe("已保存");
    expect(batch.items[1].status).toBe("failed");
    expect(itemStatusText(batch.items[1])).toContain("HTTP 500");
    expect(getSnapshot().failedItems.map((item) => item.name)).toEqual(["bad.md"]);
    expect(isUploadSettled(batch)).toBe(true);
  });

  it("reconciles the response without demoting rows SSE already settled", async () => {
    const submit = deferred<DocumentBatch>();
    vi.mocked(submitDocumentBatch).mockReturnValueOnce(submit.promise);
    const upload = uploadFiles(
      [makeFile("a.md", 10), makeFile("b.md", 10)],
      connection,
    );
    await flushMicrotasks();
    const subscriber = latestSubscriber();

    subscriber.emit({ type: "result", index: 0, status: "saved", reason: null, error: null });
    submit.resolve(serverBatch({
      batch_id: "batch-reconcile",
      total: 2,
      // The response still calls row 0 saving and omits row 1 entirely.
      items: [serverItem(0, { status: "saving", artifact_id: null, job: undefined })],
    }));
    await upload;

    const items = getSnapshot().batches[0].items;
    expect(items[0].status).toBe("accepted");
    expect(itemStatusText(items[0])).toBe("已保存");
    // A result the response never delivered falls back to a retryable failure.
    expect(items[1].status).toBe("failed");
    expect(items[1].reason).toBe("network");
    expect(itemStatusText(items[1])).toBe("保存失败：未收到保存结果，请重试");
    expect(canRetryItem(items[1])).toBe(true);
    expect(items.map((item) => perFileProgress(item))).toEqual([100, 100]);
  });

  it("posts even when the progress subscription fails", async () => {
    vi.mocked(subscribeUploadProgress).mockImplementation(() => ({
      ready: Promise.reject(
        new Error("GET /v1/document-batches/upload-events/x\nHTTP 500：订阅不可用"),
      ),
      close: vi.fn(),
    }));
    vi.mocked(submitDocumentBatch).mockResolvedValueOnce(
      serverBatch({ batch_id: "batch-fallback" }),
    );

    await uploadFiles([makeFile("a.md")], connection);

    expect(vi.mocked(submitDocumentBatch)).toHaveBeenCalledTimes(1);
    const item = getSnapshot().batches[0].items[0];
    expect(item.status).toBe("accepted");
    expect(item.transferComplete).toBe(true);
    expect(itemStatusText(item)).toBe("已保存");
  });

  it("posts after the ready deadline even if the subscription never becomes ready", async () => {
    installSubscriptionMock({ autoReady: false });
    const submit = deferred<DocumentBatch>();
    vi.mocked(submitDocumentBatch).mockReturnValueOnce(submit.promise);
    const upload = uploadFiles([makeFile("a.md")], connection);
    await flushMicrotasks();
    expect(vi.mocked(submitDocumentBatch)).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(uploadReadyTimeoutMs);
    expect(vi.mocked(submitDocumentBatch)).toHaveBeenCalledTimes(1);

    submit.resolve(serverBatch({ batch_id: "batch-timeout" }));
    await upload;
    expect(getSnapshot().batches[0].items[0].status).toBe("accepted");
  });

  it("maps skipped and failed results to Chinese status lines", async () => {
    vi.mocked(submitDocumentBatch).mockResolvedValueOnce(serverBatch({
      batch_id: "batch-status",
      items: [
        serverItem(0, { status: "skipped", reason: "exists", artifact_id: null, job: undefined }),
        serverItem(1, {
          status: "skipped",
          reason: "duplicate",
          artifact_id: null,
          job: undefined,
        }),
        serverItem(2, {
          status: "failed",
          reason: "validation",
          artifact_id: null,
          error: "不支持的文件类型",
          job: undefined,
        }),
      ],
    }));

    await uploadFiles(
      [makeFile("exists.md"), makeFile("dupe.md"), makeFile("bad.md")],
      connection,
    );

    const items = getSnapshot().batches[0].items;
    expect(itemStatusText(items[0])).toBe("已存在，已跳过");
    expect(itemStatusText(items[1])).toBe("同名文件在处理中，已跳过");
    expect(itemStatusText(items[2])).toBe("保存失败：不支持的文件类型");
    expect(items.map((item) => perFileProgress(item))).toEqual([100, 100, 100]);
    expect(isFailedItem(items[2])).toBe(true);
    expect(uploadSummary()).toMatchObject({
      phase: "completed",
      settled: 3,
      total: 3,
      hasFailure: true,
    });
  });

  it("ends at the save result and never polls the parsing stage", async () => {
    await uploadFiles([makeFile("a.md")], connection);
    expect(getSnapshot().unfinishedCount).toBe(0);
    expect(uploadSummary()?.phase).toBe("completed");
    await vi.advanceTimersByTimeAsync(uploadBatchPollIntervalMs * 4);
    expect(vi.mocked(getDocumentBatch)).not.toHaveBeenCalled();
  });

  it("marks only the files of the failed chunk as failed", async () => {
    let submitted = 0;
    vi.mocked(submitDocumentBatch).mockImplementation(async (files) => {
      submitted += 1;
      if (submitted === 1)
        throw new Error("POST /v1/document-batches\nHTTP 500：服务不可用");
      return serverBatch({
        batch_id: "batch-ok",
        items: files.map((_file, index) => serverItem(index)),
      });
    });

    const files = Array.from({ length: 11 }, (_unused, index) => makeFile(`file-${index}.md`));
    await uploadFiles(files, connection);

    const state = getSnapshot();
    expect(state.batches).toHaveLength(2);
    const failedBatch = state.batches.find((batch) => batch.status === "failed");
    expect(failedBatch?.items).toHaveLength(10);
    expect(
      failedBatch?.items.every(
        (item) => item.status === "failed" && item.reason === "network",
      ),
    ).toBe(true);
    expect(failedBatch?.items[0].error).toContain("HTTP 500");
    const acceptedBatch = state.batches.find((batch) => batch.id === "batch-ok");
    expect(acceptedBatch?.items).toHaveLength(1);
    expect(acceptedBatch?.items[0].status).toBe("accepted");
    expect(state.unfinishedCount).toBe(0);
  });
});

describe("restore", () => {
  it("restores saving/interrupted batches and polls until each row has a result", async () => {
    vi.mocked(listActiveDocumentBatches).mockResolvedValue([
      serverBatch({
        batch_id: "batch-active",
        status: "saving",
        items: [serverItem(0, { status: "saving", artifact_id: null, job: undefined })],
      }),
      serverBatch({ batch_id: "batch-processing", status: "processing" }),
      serverBatch({ batch_id: "batch-done", status: "completed" }),
      serverBatch({
        batch_id: "batch-interrupted",
        status: "interrupted",
        items: [serverItem(0, { status: "saving", job: undefined })],
      }),
    ]);

    await restore(connection);

    const state = getSnapshot();
    expect(state.batches.map((batch) => batch.id)).toEqual([
      "batch-active",
      "batch-interrupted",
    ]);
    expect(state.unfinishedCount).toBe(1);
    expect(state.failedItems).toHaveLength(1);
    const restoredRow = state.batches[0].items[0];
    expect(restoredRow.status).toBe("saving");
    // A server batch row means the bytes were already received.
    expect(restoredRow.transferComplete).toBe(true);
    expect(perFileProgress(restoredRow)).toBe(100);
    expect(itemStatusText(restoredRow)).toBe("保存中…");
    expect(itemStatusText(state.failedItems[0])).toBe("保存失败：已中断，请重试");
    expect(canRetryItem(state.failedItems[0])).toBe(false);

    const onSettled = vi.fn();
    setOnSettled(onSettled);
    vi.mocked(getDocumentBatch).mockResolvedValue(
      serverBatch({ batch_id: "batch-active", status: "completed" }),
    );
    await vi.advanceTimersByTimeAsync(uploadBatchPollIntervalMs);

    expect(vi.mocked(getDocumentBatch)).toHaveBeenCalledWith("batch-active", connection);
    expect(getSnapshot().batches[0].items[0].status).toBe("accepted");
    expect(itemStatusText(getSnapshot().batches[0].items[0])).toBe("已保存");
    expect(getSnapshot().unfinishedCount).toBe(0);
    expect(onSettled).toHaveBeenCalledTimes(1);

    // A second restore never duplicates rows already tracked.
    await restore(connection);
    expect(getSnapshot().batches.map((batch) => batch.id)).toEqual([
      "batch-active",
      "batch-interrupted",
    ]);
  });

  it("keeps polling while a restored batch still reports saving rows", async () => {
    vi.mocked(listActiveDocumentBatches).mockResolvedValue([
      serverBatch({
        batch_id: "batch-saving",
        status: "saving",
        items: [serverItem(0, { status: "saving", artifact_id: null, job: undefined })],
      }),
    ]);
    await restore(connection);
    expect(getSnapshot().unfinishedCount).toBe(1);

    vi.mocked(getDocumentBatch).mockResolvedValueOnce(serverBatch({
      batch_id: "batch-saving",
      status: "saving",
      items: [serverItem(0, { status: "saving", artifact_id: null, job: undefined })],
    }));
    await vi.advanceTimersByTimeAsync(uploadBatchPollIntervalMs);
    expect(getSnapshot().unfinishedCount).toBe(1);
    expect(getSnapshot().batches[0].items[0].status).toBe("saving");

    vi.mocked(getDocumentBatch).mockResolvedValueOnce(serverBatch({
      batch_id: "batch-saving",
      status: "completed",
    }));
    await vi.advanceTimersByTimeAsync(uploadBatchPollIntervalMs);
    expect(getSnapshot().unfinishedCount).toBe(0);
    expect(getSnapshot().batches[0].items[0].status).toBe("accepted");
  });
});

describe("retryFailedItem", () => {
  it("resubmits only the selected failed file with a fresh upload id", async () => {
    vi.mocked(submitDocumentBatch).mockRejectedValueOnce(
      new Error("POST /v1/document-batches\n网络请求失败"),
    );
    await uploadFiles([makeFile("a.md"), makeFile("b.md")], connection);
    const failed = getSnapshot().failedItems;
    expect(failed.map((item) => item.name)).toEqual(["a.md", "b.md"]);
    expect(itemStatusText(failed[0])).toBe("保存失败：POST /v1/document-batches\n网络请求失败");

    vi.mocked(submitDocumentBatch).mockResolvedValueOnce(
      serverBatch({ batch_id: "batch-retry", items: [serverItem(0, { name: "a.md" })] }),
    );
    await retryFailedItem(failed[0], connection);

    const calls = vi.mocked(submitDocumentBatch).mock.calls;
    expect(calls).toHaveLength(2);
    expect(calls[1][0]).toHaveLength(1);
    expect(calls[1][0][0].name).toBe("a.md");
    expect(calls[1][2].uploadId).toBeTruthy();
    expect(calls[1][2].uploadId).not.toBe(calls[0][2].uploadId);
    expect(getSnapshot().failedItems.map((item) => item.name)).toEqual(["b.md"]);
    expect(getSnapshot().batches.map((batch) => batch.id)).toEqual([
      "local-1",
      "batch-retry",
    ]);
  });
});

describe("clearFinished", () => {
  it("drops settled batches and keeps rows that still lack a result", async () => {
    vi.mocked(submitDocumentBatch).mockResolvedValueOnce(
      serverBatch({ batch_id: "batch-finished", status: "completed" }),
    );
    await uploadFiles([makeFile("done.md")], connection);
    expect(uploadSummary()).toMatchObject({ phase: "completed", label: "已完成 1/1" });

    const submit = deferred<DocumentBatch>();
    vi.mocked(submitDocumentBatch).mockReturnValueOnce(submit.promise);
    const upload = uploadFiles([makeFile("running.md")], connection);
    await flushMicrotasks();
    expect(uploadSummary()).toMatchObject({ phase: "saving", label: "保存中 1/2" });

    clearFinished();
    const remaining = getSnapshot().batches;
    expect(remaining).toHaveLength(1);
    expect(remaining[0].items[0].name).toBe("running.md");
    expect(uploadSummary()).toMatchObject({ phase: "saving", label: "保存中 0/1" });

    submit.resolve(serverBatch({
      batch_id: "batch-running",
      items: [serverItem(0, { name: "running.md" })],
    }));
    await upload;
    expect(uploadSummary()).toMatchObject({ phase: "completed", label: "已完成 1/1" });

    clearFinished();
    expect(getSnapshot().batches).toHaveLength(0);
    expect(uploadSummary()).toBeNull();
  });
});

describe("bindNamespace", () => {
  it("drops the previous workspace's finished rows and keeps live ones", async () => {
    vi.mocked(submitDocumentBatch).mockResolvedValueOnce(
      serverBatch({ batch_id: "batch-done", status: "completed" }),
    );
    await uploadFiles([makeFile("a.md")], connection);
    expect(uploadSummary()?.label).toBe("已完成 1/1");

    bindNamespace("other-kb");
    expect(getSnapshot().batches).toHaveLength(0);
    expect(uploadSummary()).toBeNull();

    // Re-binding the same namespace is a no-op, not a second reset.
    bindNamespace("other-kb");
    expect(getSnapshot().batches).toHaveLength(0);

    // An in-flight upload keeps its rows across a (raced) switch instead of
    // being silently abandoned.
    const submit = deferred<DocumentBatch>();
    vi.mocked(submitDocumentBatch).mockReturnValueOnce(submit.promise);
    const upload = uploadFiles([makeFile("b.md")], connection);
    await flushMicrotasks();
    expect(getSnapshot().unfinishedCount).toBe(1);
    bindNamespace("third-kb");
    expect(getSnapshot().batches).toHaveLength(1);

    submit.resolve(serverBatch({
      batch_id: "batch-live",
      items: [serverItem(0, { name: "b.md" })],
    }));
    await upload;
    expect(getSnapshot().batches).toHaveLength(1);
  });
});
