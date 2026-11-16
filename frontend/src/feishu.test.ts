import { describe, expect, it } from "vitest";
import { feishuImportReason, feishuJobLabel } from "./feishu";
import type { FeishuImportItem } from "./feishu";

function item(overrides: Partial<FeishuImportItem> = {}): FeishuImportItem {
  return {
    document_id: "doxcn1",
    title: "周报",
    name: "周报.md",
    status: "accepted",
    ...overrides,
  };
}

describe("feishu import presentation", () => {
  it("prefers the safe server error text and otherwise maps the reason code", () => {
    expect(feishuImportReason(item({ reason: "exists", error: "同名文档已存在" }))).toBe(
      "同名文档已存在",
    );
    expect(feishuImportReason(item({ reason: "permission" }))).toBe(
      "没有飞书文档权限",
    );
    expect(feishuImportReason(item({ reason: "empty" }))).toBe("没有正文");
  });

  it("never presents an accepted item as searchable before the job succeeds", () => {
    expect(feishuJobLabel(undefined, "accepted")).toBe("等待索引");
    expect(feishuJobLabel({ status: "queued", stage: "queued", error: null }, "accepted")).toBe(
      "排队中",
    );
    expect(
      feishuJobLabel({ status: "succeeded", stage: "completed", error: null }, "accepted"),
    ).toBe("已索引");
    expect(feishuJobLabel({ status: "failed", stage: "failed", error: "boom" }, "accepted")).toBe(
      "失败",
    );
    expect(feishuJobLabel(undefined, "skipped")).toBe("未入库");
    expect(feishuJobLabel(undefined, "failed")).toBe("未入库");
  });
});
