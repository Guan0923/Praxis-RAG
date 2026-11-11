import { useCallback, useEffect, useRef, useState } from "react";
import { readStorage, type Role } from "./api";

export const namespacePattern = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/;
export type ConsolePage =
  | "search"
  | "documents"
  | "create"
  | "api-keys"
  | "model-profiles"
  | "knowledge-bases"
  | "users"
  | "monitor"
  | "permissions"
  | "invitations"
  | "invite";
export type ConsoleRoute = {
  namespace: string;
  page: ConsolePage;
  documentName?: string;
  token?: string;
};

// adminOnlyPages require an administrator once Console login is enabled. The
// API Key page is not listed: every account manages only its own keys there.
export const adminOnlyPages = new Set<ConsolePage>([
  "model-profiles",
  "knowledge-bases",
  "users",
  "monitor",
]);

// canViewConsolePage is the single visibility rule for both the sidebar and a
// direct #/<page> link, so a non-admin cannot open an admin-only page by URL.
// With login disabled (legacy local mode) there is no account identity, so
// management pages stay visible exactly as before.
export function canViewConsolePage(page: ConsolePage, sessionMode: boolean, role?: Role) {
  if (page === "permissions") return sessionMode && (role === "admin" || role === "vip");
  if (adminOnlyPages.has(page)) return !sessionMode || role === "admin";
  return true;
}

const inviteTokenKey = "rag.invite-token";

function savedNamespace() {
  const value = readStorage(localStorage, "rag.namespace", "default");
  return namespacePattern.test(value) ? value : "default";
}

// readStoredInviteToken returns the raw invitation token kept while the visitor
// is sent through the login page.
export function readStoredInviteToken() {
  return readStorage(sessionStorage, inviteTokenKey).trim();
}

export function storeInviteToken(token: string) {
  try {
    sessionStorage.setItem(inviteTokenKey, token);
  } catch {
    // Invitation handling still works while the current page stays open.
  }
}

export function clearInviteToken() {
  try {
    sessionStorage.removeItem(inviteTokenKey);
  } catch {
    // Best-effort cleanup only.
  }
}

// parseInviteToken accepts either a raw token or a complete invitation link and
// returns the token part (empty when there is none).
export function parseInviteToken(text: string) {
  const value = text.trim();
  if (!value) return "";
  const marker = value.indexOf("token=");
  const raw = marker >= 0 ? value.slice(marker + "token=".length) : value;
  const rawToken = raw.split(/[&#\s]/, 1)[0] || "";
  let token = rawToken.slice(0, 256);
  try {
    token = decodeURIComponent(token);
  } catch {
    // Keep a malformed escape sequence as-is; the server still validates it.
  }
  return token.slice(0, 256);
}

export function parseRoute(hash: string, fallbackNamespace: string): ConsoleRoute {
  const fallback: ConsoleRoute = { namespace: fallbackNamespace, page: "search" };
  try {
    if (hash === "#/invite" || hash.startsWith("#/invite?")) {
      const token = hash.includes("token=") ? parseInviteToken(hash) : "";
      return token
        ? { namespace: fallbackNamespace, page: "invite", token }
        : { namespace: fallbackNamespace, page: "invite" };
    }
    if (
      hash === "#/api-keys" ||
      hash === "#/model-profiles" ||
      hash === "#/knowledge-bases" ||
      hash === "#/users" ||
      hash === "#/monitor" ||
      hash === "#/permissions" ||
      hash === "#/invitations"
    ) {
      return { namespace: fallbackNamespace, page: hash.slice(2) as ConsolePage };
    }
    const parts = hash.slice(1).split("/");
    if (!hash.startsWith("#/") || parts[1] !== "workspaces") return fallback;
    const namespace = fallbackNamespace;
    const page = parts[2];
    if (!namespacePattern.test(namespace)) return fallback;
    if (page !== "search" && page !== "documents" && page !== "create") return fallback;
    if (parts.length === 3) return { namespace, page };
    if (parts.length !== 4 || page !== "documents") return fallback;
    const documentName = decodeURIComponent(parts[3]);
    if (!documentName || documentName === "." || documentName === ".." || /[/\\\x00-\x1f]/.test(documentName)) return fallback;
    return { namespace, page, documentName };
  } catch {
    return fallback;
  }
}

export function routeHash(route: ConsoleRoute) {
  if (
    route.page === "api-keys" ||
    route.page === "model-profiles" ||
    route.page === "knowledge-bases" ||
    route.page === "users" ||
    route.page === "monitor" ||
    route.page === "permissions" ||
    route.page === "invitations"
  )
    return "#/" + route.page;
  if (route.page === "invite")
    return route.token ? `#/invite?token=${encodeURIComponent(route.token)}` : "#/invite";
  const base = `#/workspaces/${route.page}`;
  return route.page === "documents" && route.documentName !== undefined
    ? `${base}/${encodeURIComponent(route.documentName)}` : base;
}

export function useConsoleRoute(uploading: boolean, onBlocked: () => void) {
  const [route, setRoute] = useState(() => parseRoute(location.hash, savedNamespace()));
  const current = useRef(route);

  const navigate = useCallback((next: ConsoleRoute, replace = false) => {
    if (uploading && next.namespace !== current.current.namespace) {
      history.replaceState(history.state, "", routeHash(current.current));
      onBlocked();
      return false;
    }
    const hash = routeHash(next);
    if (location.hash !== hash) {
      if (replace) history.replaceState(history.state, "", hash);
      else history.pushState(null, "", hash);
    }
    if (routeHash(current.current) !== hash || current.current.namespace !== next.namespace) {
      current.current = next;
      setRoute(next);
    }
    return true;
  }, [uploading, onBlocked]);

  useEffect(() => {
    const restore = () => navigate(parseRoute(location.hash, savedNamespace()), true);
    window.addEventListener("hashchange", restore);
    restore();
    return () => window.removeEventListener("hashchange", restore);
  }, [navigate]);

  return { route, navigate };
}
