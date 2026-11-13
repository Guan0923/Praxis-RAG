import { useCallback, useEffect, useRef, useState } from "react";
import { api, errorText, type Connection } from "./api";
import { namespacePattern } from "./navigation";

// --- Pure persistence / selection helpers ----------------------------------

// NamespaceStorage is the small Web Storage slice these helpers need. Tests
// pass an in-memory fake and the Console passes window.localStorage, so the
// selection rules stay testable without a DOM.
export type NamespaceStorage = Pick<Storage, "getItem" | "setItem" | "removeItem">;

export const namespaceStoragePrefix = "rag.namespace";
// legacyNamespaceStorageKey is the shared key of legacy API-key mode: without a
// console session there is no account identity, so the historical key stays.
export const legacyNamespaceStorageKey = namespaceStoragePrefix;

// namespaceStorageKey returns the per-account key (userId = AuthUser.id). An
// empty/absent user id falls back to the legacy shared key.
export function namespaceStorageKey(userId?: string | null) {
  const id = userId?.trim();
  return id ? `${namespaceStoragePrefix}.${id}` : legacyNamespaceStorageKey;
}

function resolveStorage(storage?: NamespaceStorage) {
  if (storage) return storage;
  try {
    return typeof window === "undefined" ? undefined : window.localStorage;
  } catch {
    return undefined;
  }
}

// readSavedNamespace returns the remembered selection for one identity, or ""
// when the value is absent or does not pass the namespace pattern. Callers
// still have to validate it against the freshly loaded accessible list.
export function readSavedNamespace(
  storage?: NamespaceStorage,
  userId?: string | null,
): string {
  const target = resolveStorage(storage);
  if (!target) return "";
  try {
    const value = target.getItem(namespaceStorageKey(userId)) ?? "";
    return namespacePattern.test(value) ? value : "";
  } catch {
    return "";
  }
}

// saveSavedNamespace persists one identity's selection; an empty namespace (or
// one rejected by the pattern) clears the entry instead of storing garbage.
export function saveSavedNamespace(
  storage: NamespaceStorage | undefined,
  userId: string | null | undefined,
  namespace: string,
): boolean {
  const target = resolveStorage(storage);
  if (!target) return false;
  try {
    const key = namespaceStorageKey(userId);
    if (namespacePattern.test(namespace)) target.setItem(key, namespace);
    else target.removeItem(key);
    return true;
  } catch {
    return false;
  }
}

// normalizeNamespaceList drops non-string/invalid/duplicate entries from a
// server list so a malformed payload can never become a selectable workspace.
export function normalizeNamespaceList(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  const result: string[] = [];
  for (const item of value) {
    if (typeof item !== "string" || !namespacePattern.test(item) || result.includes(item)) continue;
    result.push(item);
  }
  return result;
}

// pickNamespace chooses the first preferred namespace that is still
// accessible, otherwise the first accessible one. Preference order is
// deliberate: current selection -> remembered selection -> first accessible.
export function pickNamespace(
  available: readonly string[],
  ...preferred: Array<string | null | undefined>
): string {
  const accessible = normalizeNamespaceList(available);
  for (const candidate of preferred) {
    if (candidate && accessible.includes(candidate)) return candidate;
  }
  return accessible[0] ?? "";
}

// workspacePagesAccessible decides whether namespace-dependent page content may
// mount. A console session requires proof of access (settled list, no error,
// non-empty and the selection already mirrored into the route). Legacy
// no-session mode keeps its historical behaviour while the list is loading or
// after a list error, but still stops the pages once a settled list is empty.
export function workspacePagesAccessible({
  sessionMode,
  settled,
  error,
  empty,
  syncing,
}: {
  sessionMode: boolean;
  settled: boolean;
  error: string;
  empty: boolean;
  syncing: boolean;
}): boolean {
  if (sessionMode) return settled && !error && !empty && !syncing;
  if (!settled || error) return true;
  return !empty && !syncing;
}

// --- Loader seam -----------------------------------------------------------

export type NamespaceList = {
  namespaces: string[];
  owned_namespaces?: string[];
};

export type LoadNamespaces = (
  connection: Connection | undefined,
  options: { signal: AbortSignal },
) => Promise<NamespaceList>;

// loadNamespacesFromApi is the production loader: one cancellable
// GET /v1/namespaces without HTTP caching. The request is namespace-agnostic:
// it keeps the credential token (legacy API-key mode) but deliberately leaves
// X-RAG-Namespace empty, because the endpoint reports the credential's own
// scope and a stale/revoked remembered namespace must not block recovery.
export function loadNamespacesFromApi(
  connection: Connection | undefined,
  { signal }: { signal: AbortSignal },
): Promise<NamespaceList> {
  return api<NamespaceList>(
    "/v1/namespaces",
    connection ? { namespace: "", token: connection.token } : undefined,
    { signal, cache: "no-store" },
  );
}

// --- Hook ------------------------------------------------------------------

type Snapshot = {
  // identity is the session identity this snapshot belongs to. Keeping it in
  // state makes an account switch take effect immediately, before the reset
  // effect has had a chance to run.
  identity: string;
  settled: boolean;
  loading: boolean;
  error: string;
  namespaces: string[];
  ownedNamespaces: string[];
  namespace: string;
};

const emptySnapshot = (identity: string, loading: boolean): Snapshot => ({
  identity,
  settled: false,
  loading,
  error: "",
  namespaces: [],
  ownedNamespaces: [],
  namespace: "",
});

const emptyNamespaces: string[] = [];

function workspaceIdentity(sessionMode: boolean, userId?: string | null) {
  return sessionMode ? `user:${userId ?? ""}` : "legacy";
}

function isAbortError(error: unknown) {
  return (error as { name?: unknown } | null | undefined)?.name === "AbortError";
}

export type UseWorkspacesOptions = {
  // enabled becomes true once the session identity is known; in legacy
  // no-session mode it becomes true as soon as the auth probe settles.
  enabled: boolean;
  sessionMode: boolean;
  userId?: string | null;
  connection?: Connection;
  storage?: NamespaceStorage;
  loadNamespaces?: LoadNamespaces;
};

export type UseWorkspacesResult = {
  // identity is the account/session identity this state belongs to ("" while
  // the identity is not known yet). Console uses it to tell an account switch
  // apart from an initial namespace correction.
  identity: string;
  settled: boolean;
  loading: boolean;
  error: string;
  namespaces: string[];
  ownedNamespaces: string[];
  namespace: string;
  empty: boolean;
  reload: () => Promise<void>;
  select: (namespace: string) => void;
};

// useWorkspaces owns the whole workspace-selection lifecycle: load the
// accessible namespaces for the current identity, validate/restore/auto-correct
// the selection, persist it per user and abort stale requests when the account
// changes. `loadNamespaces` is the injectable network seam used by tests.
export function useWorkspaces({
  enabled,
  sessionMode,
  userId,
  connection,
  storage,
  loadNamespaces = loadNamespacesFromApi,
}: UseWorkspacesOptions): UseWorkspacesResult {
  const identity = enabled ? workspaceIdentity(sessionMode, userId) : "";
  const [snapshot, setSnapshot] = useState<Snapshot>(() => emptySnapshot("", false));

  // Latest values for async callbacks; changing the connection/namespace must
  // not restart the list request.
  const identityRef = useRef(identity);
  identityRef.current = identity;
  const sessionModeRef = useRef(sessionMode);
  sessionModeRef.current = sessionMode;
  const userIdRef = useRef(userId);
  userIdRef.current = userId;
  const connectionRef = useRef(connection);
  connectionRef.current = connection;
  const storageRef = useRef(storage);
  storageRef.current = storage;
  const loaderRef = useRef(loadNamespaces);
  loaderRef.current = loadNamespaces;
  // epochRef invalidates every in-flight response when the identity changes or
  // another load starts; requestRef aborts the underlying fetch.
  const epochRef = useRef(0);
  const requestRef = useRef<AbortController | null>(null);
  // The selection the hook currently believes in. It survives reloads, but is
  // intentionally cleared when the account changes.
  const namespaceRef = useRef("");

  const runLoad = useCallback(async (targetIdentity: string) => {
    if (!targetIdentity) return;
    const epoch = ++epochRef.current;
    requestRef.current?.abort();
    const controller = new AbortController();
    requestRef.current = controller;
    setSnapshot((current) => current.identity === targetIdentity
      ? { ...current, loading: true, error: "" }
      : emptySnapshot(targetIdentity, true));
    try {
      const data = await loaderRef.current(connectionRef.current, { signal: controller.signal });
      // A response from an old identity (or an aborted request) is ignored.
      if (epoch !== epochRef.current || controller.signal.aborted) return;
      const namespaces = normalizeNamespaceList(data?.namespaces);
      // Legacy/no-session responses have no owned_namespaces field: there the
      // caller may bind every namespace, exactly like the namespaces list.
      const ownedNamespaces = data?.owned_namespaces === undefined
        ? namespaces
        : normalizeNamespaceList(data.owned_namespaces);
      const storageUserId = sessionModeRef.current ? userIdRef.current : undefined;
      const saved = readSavedNamespace(storageRef.current, storageUserId);
      const next = pickNamespace(namespaces, namespaceRef.current, saved);
      namespaceRef.current = next;
      saveSavedNamespace(storageRef.current, storageUserId, next);
      setSnapshot({
        identity: targetIdentity,
        settled: true,
        loading: false,
        error: "",
        namespaces,
        ownedNamespaces,
        namespace: next,
      });
    } catch (error) {
      if (epoch !== epochRef.current || controller.signal.aborted || isAbortError(error)) return;
      namespaceRef.current = "";
      setSnapshot({
        identity: targetIdentity,
        settled: true,
        loading: false,
        error: errorText(error),
        namespaces: [],
        ownedNamespaces: [],
        namespace: "",
      });
    }
  }, []);

  // Identity changes (login, logout, account switch) abort the old request,
  // drop the old list/selection and load the new identity's list.
  useEffect(() => {
    epochRef.current += 1;
    requestRef.current?.abort();
    requestRef.current = null;
    namespaceRef.current = "";
    if (!identity) {
      setSnapshot(emptySnapshot("", false));
      return;
    }
    setSnapshot(emptySnapshot(identity, true));
    void runLoad(identity);
    return () => {
      requestRef.current?.abort();
    };
  }, [identity, runLoad]);

  const reload = useCallback(async () => {
    const targetIdentity = identityRef.current;
    if (!targetIdentity) return;
    await runLoad(targetIdentity);
  }, [runLoad]);

  const select = useCallback((next: string) => {
    const targetIdentity = identityRef.current;
    if (!targetIdentity || !namespacePattern.test(next)) return;
    namespaceRef.current = next;
    const storageUserId = sessionModeRef.current ? userIdRef.current : undefined;
    saveSavedNamespace(storageRef.current, storageUserId, next);
    setSnapshot((current) => current.identity === targetIdentity
      ? { ...current, namespace: next }
      : current);
  }, []);

  const belongs = snapshot.identity === identity;
  const settled = belongs && snapshot.settled;
  const loading = belongs ? snapshot.loading : Boolean(identity);
  const error = belongs ? snapshot.error : "";
  const namespaces = belongs ? snapshot.namespaces : emptyNamespaces;
  const ownedNamespaces = belongs ? snapshot.ownedNamespaces : emptyNamespaces;
  const namespace = belongs ? snapshot.namespace : "";
  // `empty` deliberately ignores `loading`: while a refresh is in flight the
  // last known list is still the authoritative accessibility proof, so an empty
  // list must keep the namespace-dependent pages gated instead of briefly
  // re-opening them with the stale route namespace.
  const empty = settled && !error && namespaces.length === 0;

  return {
    identity: belongs ? identity : "",
    settled,
    loading,
    error,
    namespaces,
    ownedNamespaces,
    namespace,
    empty,
    reload,
    select,
  };
}
