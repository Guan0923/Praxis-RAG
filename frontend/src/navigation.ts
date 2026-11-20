import { useCallback, useEffect, useRef, useState } from "react";
import { readStorage, type Role } from "./api";

export const namespacePattern = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/;
export type ConsolePage =
  | "search"
  | "documents"
  | "create"
  | "api-keys"
  | "membership"
  | "model-profiles"
  | "knowledge-bases"
  | "users"
  | "system-settings"
  | "audit-events"
  | "monitor"
  | "permissions"
  | "invitations"
  | "groups"
  | "invite"
  | "group-invite";
export type ConsoleRoute = {
  namespace: string;
  page: ConsolePage;
  documentName?: string;
  token?: string;
};

// adminOnlyPages require an administrator once Console login is enabled. The
// API Key and model-configuration pages are not listed: every account manages
// only its own keys and its own saved model connections there.
export const adminOnlyPages = new Set<ConsolePage>([
  "knowledge-bases",
  "users",
  "system-settings",
  "audit-events",
  "monitor",
]);

// canViewConsolePage is the single visibility rule for both the sidebar and a
// direct #/<page> link, so a non-admin cannot open an admin-only page by URL.
// With login disabled (legacy local mode) there is no account identity, so
// management pages stay visible exactly as before.
export function canViewConsolePage(
  page: ConsolePage,
  sessionMode: boolean,
  role?: Role,
  planName?: string,
) {
  if (page === "permissions") {
    // Every signed-in owner may review and revoke existing sharing, including
    // after a paid membership expires. The server still decides whether the
    // account may create new invitations (admin/vip or active Plus/Pro).
    return sessionMode && Boolean(role);
  }
  // 用户组：所有账号都可以查看自己创建或加入的组与自己的邀请；服务端
  // 决定当前账号能否创建组或发邀请（vip/admin）。
  if (page === "groups") {
    return sessionMode && Boolean(role);
  }
  // Membership entitlements belong to one Console account. Legacy token-only
  // mode has no account, so the page is hidden until login is enabled.
  if (page === "membership") return sessionMode && Boolean(role);
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
    if (hash === "#/group-invite" || hash.startsWith("#/group-invite?")) {
      const token = hash.includes("token=") ? parseInviteToken(hash) : "";
      return token
        ? { namespace: fallbackNamespace, page: "group-invite", token }
        : { namespace: fallbackNamespace, page: "group-invite" };
    }
    if (
      hash === "#/api-keys" ||
      hash === "#/membership" ||
      hash === "#/model-profiles" ||
      hash === "#/knowledge-bases" ||
      hash === "#/users" ||
      hash === "#/system-settings" ||
      hash === "#/audit-events" ||
      hash === "#/monitor" ||
      hash === "#/permissions" ||
      hash === "#/invitations" ||
      hash === "#/groups"
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

// routeForNamespace mirrors one validated workspace selection into the route.
// When the selection changed because another account signed in
// (identityChanged), a document detail name from the previous account is
// dropped: that document belongs to the previous account, so the new session
// lands on its own document list instead of a stale detail URL.
export function routeForNamespace(
  route: ConsoleRoute,
  namespace: string,
  identityChanged: boolean,
): ConsoleRoute {
  if (identityChanged && route.page === "documents" && route.documentName !== undefined) {
    return { namespace, page: "documents" };
  }
  return { ...route, namespace };
}

export function routeHash(route: ConsoleRoute) {
  if (
    route.page === "api-keys" ||
    route.page === "membership" ||
    route.page === "model-profiles" ||
    route.page === "knowledge-bases" ||
    route.page === "users" ||
    route.page === "system-settings" ||
    route.page === "audit-events" ||
    route.page === "monitor" ||
    route.page === "permissions" ||
    route.page === "invitations" ||
    route.page === "groups"
  )
    return "#/" + route.page;
  if (route.page === "invite")
    return route.token ? `#/invite?token=${encodeURIComponent(route.token)}` : "#/invite";
  if (route.page === "group-invite")
    return route.token ? `#/group-invite?token=${encodeURIComponent(route.token)}` : "#/group-invite";
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
