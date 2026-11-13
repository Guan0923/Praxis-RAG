import { describe, expect, it } from "vitest";
import {
  legacyNamespaceStorageKey,
  namespaceStorageKey,
  normalizeNamespaceList,
  pickNamespace,
  readSavedNamespace,
  saveSavedNamespace,
  workspacePagesAccessible,
  type NamespaceStorage,
} from "./workspaces";

// memoryStorage is the in-memory Web Storage slice used by the selection
// helpers; it keeps the persistence rules testable without window.localStorage.
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

describe("namespaceStorageKey", () => {
  it("keeps the historical shared key for legacy no-session mode", () => {
    expect(legacyNamespaceStorageKey).toBe("rag.namespace");
    expect(namespaceStorageKey()).toBe("rag.namespace");
    expect(namespaceStorageKey("")).toBe("rag.namespace");
    expect(namespaceStorageKey(null)).toBe("rag.namespace");
    expect(namespaceStorageKey("   ")).toBe("rag.namespace");
  });

  it("scopes the key per authenticated user id", () => {
    expect(namespaceStorageKey("u-1")).toBe("rag.namespace.u-1");
    expect(namespaceStorageKey(" u-1 ")).toBe("rag.namespace.u-1");
    expect(namespaceStorageKey("u-1")).not.toBe(namespaceStorageKey("u-2"));
  });
});

describe("readSavedNamespace / saveSavedNamespace", () => {
  it("isolates per-user selections from each other and from legacy mode", () => {
    const storage = memoryStorage();
    saveSavedNamespace(storage, "u-1", "kb-a");
    saveSavedNamespace(storage, "u-2", "kb-b");
    saveSavedNamespace(storage, undefined, "kb-legacy");

    expect(readSavedNamespace(storage, "u-1")).toBe("kb-a");
    expect(readSavedNamespace(storage, "u-2")).toBe("kb-b");
    expect(readSavedNamespace(storage)).toBe("kb-legacy");
    expect(storage.dump()).toEqual({
      "rag.namespace.u-1": "kb-a",
      "rag.namespace.u-2": "kb-b",
      "rag.namespace": "kb-legacy",
    });
  });

  it("validates the namespace pattern before trusting a stored value", () => {
    const storage = memoryStorage({
      "rag.namespace.u-1": "kb with space",
      "rag.namespace.u-2": "-leading-dash",
      "rag.namespace.u-3": "kb-ok",
    });
    expect(readSavedNamespace(storage, "u-1")).toBe("");
    expect(readSavedNamespace(storage, "u-2")).toBe("");
    expect(readSavedNamespace(storage, "u-3")).toBe("kb-ok");
    // Boundary values of the shared namespace pattern.
    saveSavedNamespace(storage, "u-1", "A");
    expect(readSavedNamespace(storage, "u-1")).toBe("A");
    saveSavedNamespace(storage, "u-1", "a".repeat(64));
    expect(readSavedNamespace(storage, "u-1")).toBe("a".repeat(64));
    saveSavedNamespace(storage, "u-1", "a".repeat(65));
    expect(readSavedNamespace(storage, "u-1")).toBe("");
  });

  it("clears the entry instead of storing an invalid namespace", () => {
    const storage = memoryStorage({ "rag.namespace.u-1": "kb-a" });
    expect(saveSavedNamespace(storage, "u-1", "bad namespace")).toBe(true);
    expect(storage.dump()).toEqual({});
    expect(readSavedNamespace(storage, "u-1")).toBe("");
  });

  it("tolerates unavailable or throwing storage", () => {
    const broken: NamespaceStorage = {
      getItem: () => {
        throw new Error("storage denied");
      },
      setItem: () => {
        throw new Error("storage denied");
      },
      removeItem: () => {
        throw new Error("storage denied");
      },
    };
    expect(readSavedNamespace(broken, "u-1")).toBe("");
    expect(saveSavedNamespace(broken, "u-1", "kb-a")).toBe(false);
  });
});

describe("pickNamespace", () => {
  it("prefers the current selection, then the saved one, then the first accessible", () => {
    const available = ["kb-a", "kb-b", "kb-c"];
    expect(pickNamespace(available, "kb-b", "kb-c")).toBe("kb-b");
    expect(pickNamespace(available, "revoked", "kb-c")).toBe("kb-c");
    expect(pickNamespace(available, "", "revoked")).toBe("kb-a");
    expect(pickNamespace(available)).toBe("kb-a");
    expect(pickNamespace(available, undefined, null)).toBe("kb-a");
  });

  it("returns an empty string for an empty or wholly invalid list", () => {
    expect(pickNamespace([], "kb-a")).toBe("");
    expect(pickNamespace(["bad namespace", "-bad"], "kb-a")).toBe("");
  });

  it("can pick from an arbitrary preferred list", () => {
    expect(pickNamespace(["kb-a", "kb-b"], "missing", "kb-b")).toBe("kb-b");
  });
});

describe("normalizeNamespaceList", () => {
  it("drops invalid, non-string and duplicate entries", () => {
    expect(normalizeNamespaceList(["kb-a", "kb-a", "bad namespace", 7, null, "kb-b"]))
      .toEqual(["kb-a", "kb-b"]);
  });

  it("returns an empty list for non-array payloads", () => {
    expect(normalizeNamespaceList(undefined)).toEqual([]);
    expect(normalizeNamespaceList("kb-a")).toEqual([]);
    expect(normalizeNamespaceList({ namespaces: ["kb-a"] })).toEqual([]);
  });
});

describe("workspacePagesAccessible", () => {
  it("requires proof of access with a console session", () => {
    const accessible = {
      sessionMode: true,
      settled: true,
      error: "",
      empty: false,
      syncing: false,
    };
    expect(workspacePagesAccessible(accessible)).toBe(true);
    expect(workspacePagesAccessible({ ...accessible, settled: false })).toBe(false);
    expect(workspacePagesAccessible({ ...accessible, error: "HTTP 403" })).toBe(false);
    expect(workspacePagesAccessible({ ...accessible, empty: true })).toBe(false);
    expect(workspacePagesAccessible({ ...accessible, syncing: true })).toBe(false);
  });

  it("keeps legacy pages on the historical fallback while loading or after an error", () => {
    const loading = {
      sessionMode: false,
      settled: false,
      error: "",
      empty: false,
      syncing: false,
    };
    expect(workspacePagesAccessible(loading)).toBe(true);
    expect(workspacePagesAccessible({ ...loading, settled: true, error: "HTTP 500" })).toBe(true);
  });

  it("stops legacy pages for an empty settled list and while the route is synced", () => {
    const settled = {
      sessionMode: false,
      settled: true,
      error: "",
      empty: false,
      syncing: false,
    };
    expect(workspacePagesAccessible(settled)).toBe(true);
    expect(workspacePagesAccessible({ ...settled, empty: true })).toBe(false);
    expect(workspacePagesAccessible({ ...settled, syncing: true })).toBe(false);
  });
});
