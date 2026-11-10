import { describe, expect, it } from "vitest";
import { apiKeyNamespaceOptions, apiKeyOwnerText, isAdminLike } from "./apiKeys";

const namespaces = ["default", "product-manuals", "team-kb"];
const owned = ["team-kb"];

describe("apiKeyNamespaceOptions", () => {
  it("gives administrators and legacy local mode every namespace", () => {
    for (const role of ["admin", undefined] as const) {
      expect(apiKeyNamespaceOptions(namespaces, owned, role).map((option) => option.value)).toEqual(namespaces);
      expect(isAdminLike(role)).toBe(true);
    }
    expect(apiKeyNamespaceOptions(namespaces, undefined, "admin")).toEqual(
      namespaces.map((value) => ({ value, label: value })),
    );
  });

  it("gives vip and ordinary accounts only the namespaces they own", () => {
    for (const role of ["vip", "user"] as const) {
      expect(apiKeyNamespaceOptions(namespaces, owned, role).map((option) => option.value)).toEqual(["team-kb"]);
      expect(isAdminLike(role)).toBe(false);
    }
  });

  it("returns no options for a non-admin caller without owned namespaces", () => {
    expect(apiKeyNamespaceOptions(namespaces, undefined, "user")).toEqual([]);
    expect(apiKeyNamespaceOptions(namespaces, [], "vip")).toEqual([]);
  });

  it("dedupes the namespace list", () => {
    expect(apiKeyNamespaceOptions(["a", "a"], undefined, "admin").map((option) => option.value)).toEqual(["a"]);
    expect(apiKeyNamespaceOptions(namespaces, ["team-kb", "team-kb"], "vip")).toEqual([
      { value: "team-kb", label: "team-kb" },
    ]);
  });
});

describe("apiKeyOwnerText", () => {
  it("shows the owning account or labels legacy unowned keys", () => {
    expect(apiKeyOwnerText("alice")).toBe("alice");
    expect(apiKeyOwnerText(undefined)).toBe("—（未绑定）");
    expect(apiKeyOwnerText("")).toBe("—（未绑定）");
  });
});
