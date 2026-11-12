import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Connection, DocumentBatch, DocumentBatchItem } from "./api";

// Keep the real errorText()/type helpers, replace only the network calls; the
// upload store itself must never reach the network in tests.
vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return {
    ...actual,
    submitDocumentBatch: vi.fn(),
    getDocumentBatch: vi.fn(),
    listActiveDocumentBatches: vi.fn(),
  };
});

import {
  getDocumentBatch,
  listActiveDocumentBatches,
  submitDocumentBatch,
} from "./api";
import {
  __resetForTests,
  bindNamespace,
  clearFinished,
  getSnapshot,
  itemStatusText,
  maxFilesPerBatch,
  restore,
  retryFailedItem,
  setOnSettled,
  uploadFiles,
  uploadSummary,
} from "./uploadBatches";

const connection: Connection = { namespace: "team-kb", token: "" };

function makeFile(name: string) {
  return new File(["content"], name, { type: "text/markdown" });
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

beforeEach(() => {
  __resetForTests();
  vi.useFakeTimers();
  vi.mocked(submitDocumentBatch).mockReset();
  vi.mocked(getDocumentBatch).mockReset();
  vi.mocked(listActiveDocumentBatches).mockReset();
});

afterEach(() => {
  __resetForTests();
  vi.clearAllTimers();
  vi.useRealTimers();
});

describe("uploadFiles", () => {
  it("splits a 20-file selection into two batches of at most ten", async () => {
    let submitted = 0;
    vi.mocked(submitDocumentBatch).mockImplementation(async (files) => {
      submitted += 1;
      return serverBatch({
        batch_id: `batch-${submitted}`,
        items: files.map((_, index) => serverItem(index)),
      });
    });

    const files = Array.from({ length: 20 }, (_, index) => makeFile(`file-${index}.md`));
    await uploadFiles(files, connection);

    const calls = vi.mocked(submitDocumentBatch).mock.calls;
    expect(calls).toHaveLength(2);
    expect(calls[0][0]).toHaveLength(maxFilesPerBatch);
    expect(calls[1][0]).toHaveLength(maxFilesPerBatch);
    expect(calls[0][2].idempotencyKey).toBeTruthy();
    expect(calls[0][2].idempotencyKey).not.toBe(calls[1][2].idempotencyKey);
    expect(getSnapshot().batches.map((batch) => batch.id)).toEqual([
      "batch-1",
      "batch-2",
    ]);
  });

  it("marks only the files of the failed batch as failed", async () => {
    let submitted = 0;
    vi.mocked(submitDocumentBatch).mockImplementation(async () => {
      submitted += 1;
      if (submitted === 1)
        throw new Error("POST /v1/document-batches\nHTTP 500：服务不可用");
      return serverBatch({ batch_id: "batch-ok" });
    });

    const files = Array.from({ length: 11 }, (_, index) => makeFile(`file-${index}.md`));
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
    const acceptedBatch = state.batches.find((batch) => batch.status === "processing");
    expect(acceptedBatch?.items).toHaveLength(1);
    expect(acceptedBatch?.items[0].status).toBe("accepted");
  });

  it("deduplicates a replayed batch by batch id", async () => {
    vi.mocked(submitDocumentBatch).mockResolvedValue(
      serverBatch({ batch_id: "batch-replay", total: 1, items: [serverItem(0)] }),
    );
    await uploadFiles([makeFile("a.md")], connection);
    await uploadFiles([makeFile("b.md")], connection);

    const state = getSnapshot();
    expect(state.batches).toHaveLength(1);
    expect(state.batches[0].id).toBe("batch-replay");
    expect(state.batches[0].total).toBe(1);
    expect(state.batches[0].items).toHaveLength(1);
  });

  it("derives aggregate counts and chip labels from batch status", async () => {    vi.mocked(submitDocumentBatch).mockResolvedValueOnce(
      serverBatch({
        batch_id: "batch-mixed",
        items: [
          serverItem(0, { job: { status: "succeeded", stage: "completed", error: null } }),
          // retry_wait is a worker backoff stage: keep the Chinese fallback
          // instead of leaking the raw stage name into the bubble.
          serverItem(1, { job: { status: "queued", stage: "retry_wait", error: null } }),
          serverItem(2, {
            status: "skipped",
            reason: "exists",
            artifact_id: null,
            job: undefined,
          }),
          serverItem(3, {
            status: "failed",
            reason: "validation",
            artifact_id: null,
            error: "不支持的文件类型",
            job: undefined,
          }),
        ],
      }),
    );
    await uploadFiles([makeFile("a.md")], connection);

    const state = getSnapshot();
    expect(state.settled).toBe(3);
    expect(state.failedItems).toHaveLength(1);
    expect(uploadSummary(state)).toMatchObject({
      phase: "processing",
      label: "处理中 3/4",
      settled: 3,
      total: 4,
      hasFailure: true,
    });
    expect(itemStatusText(state.batches[0].items[2], 100)).toBe("已存在，已跳过");
    expect(itemStatusText(state.batches[0].items[3], 100)).toBe(
      "失败：不支持的文件类型",
    );
    expect(itemStatusText(state.batches[0].items[1], 100)).toBe("已受理，解析中…");

    __resetForTests();
    vi.mocked(submitDocumentBatch).mockResolvedValueOnce(
      serverBatch({ batch_id: "batch-saving", status: "saving" }),
    );
    await uploadFiles([makeFile("b.md")], connection);
    expect(uploadSummary()?.label).toBe("保存中 0/1");

    __resetForTests();
    vi.mocked(submitDocumentBatch).mockResolvedValueOnce(
      serverBatch({
        batch_id: "batch-completed",
        status: "completed",
        items: [
          serverItem(0, { job: { status: "succeeded", stage: "completed", error: null } }),
        ],
      }),
    );
    await uploadFiles([makeFile("c.md")], connection);
    expect(uploadSummary()).toMatchObject({
      phase: "completed",
      label: "已完成 1/1",
      hasFailure: false,
    });
  });
});

describe("polling", () => {
  it("stops after the batch settles and notifies onSettled once", async () => {
    vi.mocked(submitDocumentBatch).mockResolvedValue(
      serverBatch({ batch_id: "batch-poll" }),
    );
    vi.mocked(getDocumentBatch).mockResolvedValue(
      serverBatch({
        batch_id: "batch-poll",
        status: "completed",
        items: [
          serverItem(0, { job: { status: "succeeded", stage: "completed", error: null } }),
        ],
      }),
    );
    const onSettled = vi.fn();
    setOnSettled(onSettled);

    await uploadFiles([makeFile("a.md")], connection);
    expect(vi.mocked(getDocumentBatch)).not.toHaveBeenCalled();

    await vi.advanceTimersByTimeAsync(1500);
    expect(vi.mocked(getDocumentBatch)).toHaveBeenCalledTimes(1);
    expect(getSnapshot().batches[0].status).toBe("completed");
    expect(onSettled).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(5000);
    expect(vi.mocked(getDocumentBatch)).toHaveBeenCalledTimes(1);
    expect(onSettled).toHaveBeenCalledTimes(1);
  });
});

describe("restore", () => {
  it("restores unfinished batches, keeps interrupted ones retryable, and does not duplicate them", async () => {
    vi.mocked(listActiveDocumentBatches).mockResolvedValue([
      serverBatch({ batch_id: "batch-active", status: "processing" }),
      serverBatch({ batch_id: "batch-done", status: "completed" }),
      serverBatch({
        batch_id: "batch-interrupted",
        status: "interrupted",
        items: [serverItem(0, { status: "saving", job: undefined })],
      }),
    ]);

    await restore(connection);
    expect(getSnapshot().batches.map((batch) => batch.id)).toEqual([
      "batch-active",
      "batch-interrupted",
    ]);
    expect(getSnapshot().unfinishedCount).toBe(1);
    expect(getSnapshot().failedItems).toHaveLength(1);
    expect(itemStatusText(getSnapshot().failedItems[0], 100)).toBe(
      "失败：已中断，请重试",
    );

    await restore(connection);
    expect(getSnapshot().batches.map((batch) => batch.id)).toEqual([
      "batch-active",
      "batch-interrupted",
    ]);
  });
});

describe("retryFailedItem", () => {
  it("resubmits only the selected failed file", async () => {
    vi.mocked(submitDocumentBatch).mockRejectedValueOnce(
      new Error("POST /v1/document-batches\n网络请求失败"),
    );
    await uploadFiles([makeFile("a.md"), makeFile("b.md")], connection);
    const failed = getSnapshot().failedItems;
    expect(failed.map((item) => item.name)).toEqual(["a.md", "b.md"]);

    vi.mocked(submitDocumentBatch).mockResolvedValueOnce(
      serverBatch({ batch_id: "batch-retry", items: [serverItem(0, { name: "a.md" })] }),
    );
    await retryFailedItem(failed[0], connection);

    const calls = vi.mocked(submitDocumentBatch).mock.calls;
    expect(calls).toHaveLength(2);
    expect(calls[1][0]).toHaveLength(1);
    expect(calls[1][0][0].name).toBe("a.md");
    expect(calls[1][2].idempotencyKey).not.toBe(calls[0][2].idempotencyKey);
    expect(getSnapshot().failedItems.map((item) => item.name)).toEqual(["b.md"]);
    expect(getSnapshot().batches.map((batch) => batch.id)).toEqual([
      "local-1",
      "batch-retry",
    ]);
  });
});

describe("clearFinished", () => {
  it("keeps unfinished batches and drops terminal ones", async () => {
    vi.mocked(submitDocumentBatch).mockResolvedValue(
      serverBatch({
        batch_id: "batch-finished",
        status: "completed",
        items: [
          serverItem(0, { job: { status: "succeeded", stage: "completed", error: null } }),
        ],
      }),
    );
    await uploadFiles([makeFile("done.md")], connection);
    vi.mocked(submitDocumentBatch).mockResolvedValue(
      serverBatch({ batch_id: "batch-running", status: "processing" }),
    );
    await uploadFiles([makeFile("running.md")], connection);

    clearFinished();
    expect(getSnapshot().batches.map((batch) => batch.id)).toEqual([
      "batch-running",
    ]);
    expect(uploadSummary()?.label).toBe("处理中 0/1");
  });
});

describe("bindNamespace", () => {
  it("drops the previous workspace's finished rows when the workspace changes", async () => {
    vi.mocked(submitDocumentBatch).mockResolvedValueOnce(
      serverBatch({
        batch_id: "batch-done",
        status: "completed",
        items: [
          serverItem(0, { job: { status: "succeeded", stage: "completed", error: null } }),
        ],
      }),
    );
    await uploadFiles([makeFile("a.md")], connection);
    expect(uploadSummary()?.label).toBe("已完成 1/1");

    bindNamespace("other-kb");
    expect(getSnapshot().batches).toHaveLength(0);
    expect(uploadSummary()).toBeNull();

    // Re-binding the same namespace is a no-op, not a second reset.
    bindNamespace("other-kb");
    expect(getSnapshot().batches).toHaveLength(0);
  });

  it("keeps unfinished rows when a workspace switch somehow races an upload", async () => {
    vi.mocked(submitDocumentBatch).mockResolvedValueOnce(
      serverBatch({ batch_id: "batch-live", status: "processing" }),
    );
    await uploadFiles([makeFile("a.md")], connection);
    expect(getSnapshot().unfinishedCount).toBe(1);

    bindNamespace("other-kb");
    expect(getSnapshot().batches).toHaveLength(1);
  });
});
