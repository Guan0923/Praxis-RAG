import { describe, expect, it } from "vitest";
import {
  grantRuleKey,
  masksOfTriStates,
  triStateOf,
  triStatesFromMasks,
} from "./PermissionsPage";
import {
  effectiveScopeMask,
  mergeMasks,
  PERM_CREATE,
  PERM_DELETE,
  PERM_READ,
  PERM_UPDATE,
} from "./api";

// 权限页三态编辑的纯逻辑：掩码 ↔ 未设置/允许/拒绝。
describe("permission page tri-state helpers", () => {
  it("resolves one operation's tri-state from the two masks", () => {
    // allow=5（阅读+修改）、deny=4（修改）：deny 压过 allow。
    expect(triStateOf(5, 0, PERM_READ)).toBe("allow");
    expect(triStateOf(5, 4, PERM_UPDATE)).toBe("deny");
    expect(triStateOf(5, 0, PERM_DELETE)).toBe("unset");
    expect(triStateOf(0, 0, PERM_CREATE)).toBe("unset");
  });

  it("folds tri-states back into the allow/deny masks", () => {
    expect(masksOfTriStates({
      [PERM_READ]: "allow",
      [PERM_UPDATE]: "allow",
      [PERM_DELETE]: "deny",
      [PERM_CREATE]: "unset",
    })).toEqual({ allow_mask: 5, deny_mask: 8 });
    expect(masksOfTriStates({})).toEqual({ allow_mask: 0, deny_mask: 0 });
  });

  it("round-trips masks through the tri-state editor", () => {
    const states = triStatesFromMasks(13, 2);
    expect(states[PERM_READ]).toBe("allow");
    expect(states[PERM_UPDATE]).toBe("allow");
    expect(states[PERM_DELETE]).toBe("allow");
    expect(states[PERM_CREATE]).toBe("deny");
    expect(masksOfTriStates(states)).toEqual({ allow_mask: 13, deny_mask: 2 });
  });

  it("keys one rule by user and document name", () => {
    expect(grantRuleKey("u1", "")).toBe("u1\u0000");
    expect(grantRuleKey("u1", "a.md")).toBe("u1\u0000a.md");
    expect(grantRuleKey("u1", "a.md")).not.toBe(grantRuleKey("u2", "a.md"));
  });
});

// 单文件有效权限 = (default|whole|file allow) &^ (default|whole|file deny)。
describe("effective scope mask", () => {
  it("lets deny clear the allow bit at any layer", () => {
    expect(effectiveScopeMask({
      default_allow: 1,
      default_deny: 0,
      whole_allow: 4,
      whole_deny: 0,
      file_allow: { "a.md": 8 },
      file_deny: { "a.md": 8 },
      whole_read: true,
      whole_write: false,
      read_documents: [],
      write_documents: [],
    }, "a.md")).toBe(1 | 4);
    expect(mergeMasks(15, 2)).toBe(13);
  });
});
