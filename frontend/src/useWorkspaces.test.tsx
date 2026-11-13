import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  useWorkspaces,
  type LoadNamespaces,
  type NamespaceList,
  type NamespaceStorage,
  type UseWorkspacesOptions,
} from "./workspaces";

// memoryStorage keeps the per-user persistence assertions independent of
// window.localStorage and its cross-test state.
function memoryStorage(initial: Record<string, string> = {}) {
  const entries = new Map(Object.entries(initial));
  return {
    getItem: (key: string) => entries.get(key) ?? null,
    setItem: (key: string, value: string) => {
      entries.set(key, value);
    },
    removeItem: (key: string) => {
      entries.delete(key);
    },
    dump: () => Object.fromEntries(entries),
  } satisfies NamespaceStorage & { dump: () => Record<string, string> };
}

// deferred lets a test control exactly when an in-flight list request settles,
// so identity-switch races are deterministic.
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function renderWorkspaces(options: UseWorkspacesOptions) {
  return renderHook((props: UseWorkspacesOptions) => useWorkspaces(props), {
    initialProps: options,
  });
}

function sessionOptions(
  overrides: Partial<UseWorkspacesOptions> = {},
): UseWorkspacesOptions {
  return {
    enabled: true,
    sessionMode: true,
    userId: "u-1",
    loadNamespaces: vi.fn<LoadNamespaces>(async () => ({ namespaces: ["kb-a"] })),
    ...overrides,
  };
}

afterEach(() => {
  cleanup();
});

describe("useWorkspaces auto-load and restore", () => {
  it("loads as soon as the identity is known instead of waiting for a dropdown", async () => {
    const storage = memoryStorage();
    const loadNamespaces = vi.fn<LoadNamespaces>(async () => ({ namespaces: ["kb-a"] }));
    const { result, rerender } = renderWorkspaces(
      sessionOptions({ enabled: false, storage, loadNamespaces }),
    );

    expect(loadNamespaces).not.toHaveBeenCalled();
    expect(result.current.settled).toBe(false);

    rerender(sessionOptions({ enabled: true, storage, loadNamespaces }));
    await waitFor(() => expect(result.current.settled).toBe(true));

    expect(loadNamespaces).toHaveBeenCalledTimes(1);
    expect(result.current.loading).toBe(false);
    expect(result.current.error).toBe("");
    expect(result.current.namespace).toBe("kb-a");
    expect(result.current.namespaces).toEqual(["kb-a"]);
  });

  it("restores the remembered per-user selection and persists later choices", async () => {
    const storage = memoryStorage({ "rag.namespace.u-1": "kb-b" });
    const loadNamespaces = vi.fn<LoadNamespaces>(async () => ({ namespaces: ["kb-a", "kb-b"] }));
    const { result } = renderWorkspaces(sessionOptions({ storage, loadNamespaces }));

    await waitFor(() => expect(result.current.settled).toBe(true));
    expect(result.current.namespace).toBe("kb-b");
    expect(storage.dump()["rag.namespace.u-1"]).toBe("kb-b");

    act(() => result.current.select("kb-a"));
    expect(result.current.namespace).toBe("kb-a");
    expect(storage.dump()["rag.namespace.u-1"]).toBe("kb-a");

    // A selection that does not even match the namespace pattern is ignored.
    act(() => result.current.select("bad namespace"));
    expect(result.current.namespace).toBe("kb-a");
    expect(storage.dump()["rag.namespace.u-1"]).toBe("kb-a");
  });

  it("auto-corrects an inaccessible remembered selection to the first accessible one", async () => {
    const storage = memoryStorage({ "rag.namespace.u-1": "kb-deleted" });
    const loadNamespaces = vi.fn<LoadNamespaces>(async () => ({ namespaces: ["kb-a", "kb-b"] }));
    const { result } = renderWorkspaces(sessionOptions({ storage, loadNamespaces }));

    await waitFor(() => expect(result.current.settled).toBe(true));
    expect(result.current.namespace).toBe("kb-a");
    expect(storage.dump()["rag.namespace.u-1"]).toBe("kb-a");
  });

  it("never trusts a stored value that fails the namespace pattern", async () => {
    const storage = memoryStorage({ "rag.namespace.u-1": "kb with space" });
    const loadNamespaces = vi.fn<LoadNamespaces>(async () => ({ namespaces: ["kb-a"] }));
    const { result } = renderWorkspaces(sessionOptions({ storage, loadNamespaces }));

    await waitFor(() => expect(result.current.settled).toBe(true));
    expect(result.current.namespace).toBe("kb-a");
    expect(storage.dump()["rag.namespace.u-1"]).toBe("kb-a");
  });

  it("clears the selection and storage when no namespace is accessible", async () => {
    const storage = memoryStorage({ "rag.namespace.u-1": "kb-a" });
    const loadNamespaces = vi.fn<LoadNamespaces>(async () => ({
      namespaces: [],
      owned_namespaces: [],
    }));
    const { result } = renderWorkspaces(sessionOptions({ storage, loadNamespaces }));

    await waitFor(() => expect(result.current.settled).toBe(true));
    expect(result.current.empty).toBe(true);
    expect(result.current.loading).toBe(false);
    expect(result.current.error).toBe("");
    expect(result.current.namespace).toBe("");
    expect(result.current.namespaces).toEqual([]);
    expect(storage.dump()).toEqual({});
  });

  it("keeps the historical shared key and owned fallback in legacy no-session mode", async () => {
    const storage = memoryStorage({ "rag.namespace": "kb-legacy" });
    const loadNamespaces = vi.fn<LoadNamespaces>(async () => ({
      namespaces: ["kb-legacy", "kb-other"],
    }));
    const { result } = renderWorkspaces(
      sessionOptions({ sessionMode: false, userId: undefined, storage, loadNamespaces }),
    );

    await waitFor(() => expect(result.current.settled).toBe(true));
    expect(result.current.namespace).toBe("kb-legacy");
    // Legacy responses have no owned_namespaces: every accessible namespace
    // may be bound exactly like before.
    expect(result.current.ownedNamespaces).toEqual(["kb-legacy", "kb-other"]);

    act(() => result.current.select("kb-other"));
    expect(storage.dump()).toEqual({ "rag.namespace": "kb-other" });
  });
});

describe("useWorkspaces identity isolation", () => {
  it("aborts the old request and ignores a late response after an account switch", async () => {
    const storage = memoryStorage({
      "rag.namespace.ua": "kb-a1",
      "rag.namespace.ub": "kb-b2",
    });
    const aInitial = deferred<NamespaceList>();
    const aReload = deferred<NamespaceList>();
    const bLoad = deferred<NamespaceList>();
    const loadNamespaces = vi.fn<LoadNamespaces>()
      .mockImplementationOnce(() => aInitial.promise)
      .mockImplementationOnce(() => aReload.promise)
      .mockImplementationOnce(() => bLoad.promise);

    const { result, rerender } = renderWorkspaces(
      sessionOptions({ userId: "ua", storage, loadNamespaces }),
    );
    await waitFor(() => expect(loadNamespaces).toHaveBeenCalledTimes(1));
    await act(async () => {
      aInitial.resolve({ namespaces: ["kb-a1", "kb-a2"] });
    });
    await waitFor(() => expect(result.current.settled).toBe(true));
    expect(result.current.namespace).toBe("kb-a1");

    // Start a refresh for account A, then switch to account B while it is
    // still in flight.
    await act(async () => {
      void result.current.reload();
    });
    await waitFor(() => expect(loadNamespaces).toHaveBeenCalledTimes(2));
    const staleSignal = loadNamespaces.mock.calls[1][1].signal;

    rerender(sessionOptions({ userId: "ub", storage, loadNamespaces }));
    await waitFor(() => expect(loadNamespaces).toHaveBeenCalledTimes(3));

    expect(staleSignal.aborted).toBe(true);
    // The previous account's list and selection are dropped immediately.
    expect(result.current.namespaces).toEqual([]);
    expect(result.current.namespace).toBe("");
    expect(result.current.loading).toBe(true);
    expect(result.current.settled).toBe(false);

    await act(async () => {
      bLoad.resolve({ namespaces: ["kb-b1", "kb-b2"], owned_namespaces: ["kb-b2"] });
    });
    await waitFor(() => expect(result.current.settled).toBe(true));
    expect(result.current.namespace).toBe("kb-b2");
    expect(result.current.ownedNamespaces).toEqual(["kb-b2"]);
    expect(storage.dump()).toEqual({
      "rag.namespace.ua": "kb-a1",
      "rag.namespace.ub": "kb-b2",
    });

    // A response that belongs to the old identity arrives late: it must not
    // overwrite account B's list or selection.
    await act(async () => {
      aReload.resolve({ namespaces: ["kb-a1", "kb-a2", "kb-a3"] });
      await Promise.resolve();
    });
    expect(result.current.namespaces).toEqual(["kb-b1", "kb-b2"]);
    expect(result.current.namespace).toBe("kb-b2");
    expect(storage.dump()["rag.namespace.ub"]).toBe("kb-b2");
  });
});

describe("useWorkspaces error handling and refresh", () => {
  it("reports an explicit list/permission error and never keeps a stale list", async () => {
    const storage = memoryStorage();
    const loadNamespaces = vi.fn<LoadNamespaces>()
      .mockResolvedValueOnce({ namespaces: ["kb-a"] })
      .mockRejectedValueOnce(new Error("GET /v1/namespaces\nHTTP 403：无权访问该知识库"));
    const { result } = renderWorkspaces(sessionOptions({ storage, loadNamespaces }));

    await waitFor(() => expect(result.current.settled).toBe(true));
    expect(result.current.namespace).toBe("kb-a");

    await act(async () => {
      await result.current.reload();
    });
    await waitFor(() => expect(result.current.error).toContain("HTTP 403"));
    expect(result.current.settled).toBe(true);
    expect(result.current.namespaces).toEqual([]);
    expect(result.current.namespace).toBe("");
    expect(result.current.empty).toBe(false);
  });

  it("keeps a reloaded selection that is still accessible", async () => {
    const storage = memoryStorage();
    const loadNamespaces = vi.fn<LoadNamespaces>()
      .mockResolvedValueOnce({ namespaces: ["kb-a", "kb-b"] })
      .mockResolvedValueOnce({ namespaces: ["kb-b", "kb-a"] });
    const { result } = renderWorkspaces(sessionOptions({ storage, loadNamespaces }));

    await waitFor(() => expect(result.current.settled).toBe(true));
    expect(result.current.namespace).toBe("kb-a");

    await act(async () => {
      await result.current.reload();
    });
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.namespace).toBe("kb-a");
    expect(result.current.namespaces).toEqual(["kb-b", "kb-a"]);
  });

  it("auto-corrects to the first accessible namespace after a revocation", async () => {
    const storage = memoryStorage();
    const loadNamespaces = vi.fn<LoadNamespaces>()
      .mockResolvedValueOnce({ namespaces: ["kb-a", "kb-b"] })
      .mockResolvedValueOnce({ namespaces: ["kb-b", "kb-c"] });
    const { result } = renderWorkspaces(sessionOptions({ storage, loadNamespaces }));

    await waitFor(() => expect(result.current.settled).toBe(true));
    expect(result.current.namespace).toBe("kb-a");

    await act(async () => {
      await result.current.reload();
    });
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.namespace).toBe("kb-b");
    expect(storage.dump()["rag.namespace.u-1"]).toBe("kb-b");
  });
});
