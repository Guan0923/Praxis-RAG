import { describe, expect, it } from "vitest";
import {
  canViewConsolePage,
  namespacePattern,
  parseInviteToken,
  parseRoute,
  routeForNamespace,
  routeHash,
} from "./navigation";

describe("canViewConsolePage", () => {
  it("shows the monitor page to administrators only", () => {
    expect(canViewConsolePage("monitor", true, "admin")).toBe(true);
    expect(canViewConsolePage("monitor", true, "vip")).toBe(false);
    expect(canViewConsolePage("monitor", true, "user")).toBe(false);
  });

  it("keeps the system settings page to administrators only", () => {
    expect(canViewConsolePage("system-settings", true, "admin")).toBe(true);
    expect(canViewConsolePage("system-settings", true, "vip")).toBe(false);
    expect(canViewConsolePage("system-settings", true, "user")).toBe(false);
  });

  it("keeps management pages visible in legacy no-session mode", () => {
    expect(canViewConsolePage("monitor", false, undefined)).toBe(true);
    expect(canViewConsolePage("model-profiles", false, undefined)).toBe(true);
  });

  it("keeps permissions to administrators and VIPs", () => {
    expect(canViewConsolePage("permissions", true, "vip")).toBe(true);
    expect(canViewConsolePage("permissions", true, "user")).toBe(false);
    expect(canViewConsolePage("permissions", false, undefined)).toBe(false);
  });

  it("leaves ordinary pages visible to every account", () => {
    expect(canViewConsolePage("search", true, "user")).toBe(true);
    expect(canViewConsolePage("documents", true, "user")).toBe(true);
  });
});

describe("parseInviteToken", () => {
  it("accepts a raw token", () => {
    expect(parseInviteToken("abc123")).toBe("abc123");
    expect(parseInviteToken("  abc123  ")).toBe("abc123");
  });

  it("accepts a complete invitation link", () => {
    expect(parseInviteToken("http://127.0.0.1:8011/console#/invite?token=abc123")).toBe("abc123");
    expect(parseInviteToken("token=abc123&other=1")).toBe("abc123");
    expect(parseInviteToken("#/invite?token=abc123&from=chat")).toBe("abc123");
  });

  it("decodes escapes but keeps malformed input", () => {
    expect(parseInviteToken("token=a%2Fb")).toBe("a/b");
    expect(parseInviteToken("token=%ZZ")).toBe("%ZZ");
  });

  it("returns empty input unchanged and caps the length", () => {
    expect(parseInviteToken("")).toBe("");
    expect(parseInviteToken("   ")).toBe("");
    expect(parseInviteToken("x".repeat(400))).toHaveLength(256);
  });
});

describe("parseRoute", () => {
  it("parses the invite route with and without a token", () => {
    expect(parseRoute("#/invite?token=t1", "default")).toEqual({
      namespace: "default",
      page: "invite",
      token: "t1",
    });
    expect(parseRoute("#/invite", "team")).toEqual({ namespace: "team", page: "invite" });
  });

  it("parses the static Console pages", () => {
    for (const page of ["api-keys", "model-profiles", "knowledge-bases", "users", "system-settings", "monitor", "permissions", "invitations"]) {
      expect(parseRoute("#/" + page, "default")).toEqual({ namespace: "default", page });
    }
  });

  it("parses the system monitor page as a top-level route", () => {
    expect(parseRoute("#/monitor", "team")).toEqual({
      namespace: "team",
      page: "monitor",
    });
    expect(routeHash({ namespace: "team", page: "monitor" })).toBe("#/monitor");
  });

  it("parses the model configuration page as a top-level route", () => {
    expect(parseRoute("#/model-profiles", "team")).toEqual({
      namespace: "team",
      page: "model-profiles",
    });
    expect(routeHash({ namespace: "team", page: "model-profiles" })).toBe("#/model-profiles");
  });

  it("parses workspace pages and one document name", () => {
    expect(parseRoute("#/workspaces/search", "team")).toEqual({
      namespace: "team",
      page: "search",
    });
    expect(parseRoute("#/workspaces/documents/guide%20one.md", "team")).toEqual({
      namespace: "team",
      page: "documents",
      documentName: "guide one.md",
    });
  });

  it("falls back for unknown or unsafe routes", () => {
    const fallback = { namespace: "default", page: "search" };
    expect(parseRoute("#/workspaces/unknown", "default")).toEqual(fallback);
    expect(parseRoute("#/nothing", "default")).toEqual(fallback);
    expect(parseRoute("", "default")).toEqual(fallback);
    expect(parseRoute("#/workspaces/documents/..", "default")).toEqual(fallback);
    expect(parseRoute("#/workspaces/documents/a%5Cb", "default")).toEqual(fallback);
    expect(parseRoute("#/workspaces/documents/a%00b", "default")).toEqual(fallback);
  });
});

describe("routeHash", () => {
  it("round-trips the routes the Console navigates to", () => {
    // The hash never carries the namespace: workspace routes fall back to the
    // namespace remembered by the Console, which is why the fallback argument
    // below is the route's own namespace.
    const routes = [
      { namespace: "team", page: "search" as const },
      { namespace: "team", page: "documents" as const },
      { namespace: "team", page: "documents" as const, documentName: "a b.md" },
      { namespace: "team", page: "permissions" as const },
      { namespace: "team", page: "monitor" as const },
      { namespace: "team", page: "system-settings" as const },
      { namespace: "team", page: "model-profiles" as const },
      { namespace: "team", page: "invitations" as const },
      { namespace: "team", page: "invite" as const, token: "abc" },
      { namespace: "team", page: "invite" as const },
    ];
    for (const route of routes) {
      expect(parseRoute(routeHash(route), route.namespace)).toEqual(route);
    }
  });
});

describe("routeForNamespace", () => {
  it("mirrors the validated namespace while keeping the current page", () => {
    expect(routeForNamespace({ namespace: "old", page: "search" }, "new", false)).toEqual({
      namespace: "new",
      page: "search",
    });
    expect(
      routeForNamespace({ namespace: "old", page: "documents", documentName: "a.md" }, "new", false),
    ).toEqual({ namespace: "new", page: "documents", documentName: "a.md" });
  });

  it("drops a document detail name when the account identity changed", () => {
    expect(
      routeForNamespace({ namespace: "old", page: "documents", documentName: "a.md" }, "new", true),
    ).toEqual({ namespace: "new", page: "documents" });
    expect(routeForNamespace({ namespace: "old", page: "documents" }, "new", true)).toEqual({
      namespace: "new",
      page: "documents",
    });
  });

  it("keeps invite tokens and unrelated pages intact", () => {
    expect(routeForNamespace({ namespace: "old", page: "invite", token: "t" }, "new", true)).toEqual({
      namespace: "new",
      page: "invite",
      token: "t",
    });
    expect(routeForNamespace({ namespace: "old", page: "permissions" }, "new", true)).toEqual({
      namespace: "new",
      page: "permissions",
    });
  });
});

describe("namespacePattern", () => {
  it("accepts the server identifier format", () => {
    for (const value of ["a", "team-kb", "team.kb_1", "A1"]) {
      expect(namespacePattern.test(value)).toBe(true);
    }
    for (const value of ["", "-a", ".a", "a/b", "a b", "a".repeat(65)]) {
      expect(namespacePattern.test(value)).toBe(false);
    }
  });
});
