import { describe, expect, it } from "vitest";
import { namespacePattern, parseInviteToken, parseRoute, routeHash } from "./navigation";

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
    for (const page of ["api-keys", "knowledge-bases", "users", "permissions", "invitations"]) {
      expect(parseRoute("#/" + page, "default")).toEqual({ namespace: "default", page });
    }
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
      { namespace: "team", page: "invitations" as const },
      { namespace: "team", page: "invite" as const, token: "abc" },
      { namespace: "team", page: "invite" as const },
    ];
    for (const route of routes) {
      expect(parseRoute(routeHash(route), route.namespace)).toEqual(route);
    }
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
