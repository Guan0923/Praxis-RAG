import { useCallback, useEffect, useRef, useState } from "react";
import { readStorage } from "./api";

export const namespacePattern = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/;
export type ConsolePage = "search" | "documents" | "connection";
export type ConsoleRoute = {
  namespace: string;
  page: ConsolePage;
  documentName?: string;
};

function savedNamespace() {
  const value = readStorage(localStorage, "rag.namespace", "default");
  return namespacePattern.test(value) ? value : "default";
}

export function parseRoute(hash: string, fallbackNamespace: string): ConsoleRoute {
  const fallback: ConsoleRoute = { namespace: fallbackNamespace, page: "search" };
  try {
    const parts = hash.slice(1).split("/");
    if (!hash.startsWith("#/") || parts[1] !== "workspaces") return fallback;
    const namespace = decodeURIComponent(parts[2] || "");
    const page = parts[3];
    if (!namespacePattern.test(namespace)) return fallback;
    if (page !== "search" && page !== "documents" && page !== "connection") return fallback;
    if (parts.length === 4) return { namespace, page };
    if (parts.length !== 5 || page !== "documents") return fallback;
    const documentName = decodeURIComponent(parts[4]);
    if (!documentName || documentName === "." || documentName === ".." || /[/\\\x00-\x1f]/.test(documentName)) return fallback;
    return { namespace, page, documentName };
  } catch {
    return fallback;
  }
}

export function routeHash(route: ConsoleRoute) {
  const base = `#/workspaces/${encodeURIComponent(route.namespace)}/${route.page}`;
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
    if (routeHash(current.current) !== hash) {
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
