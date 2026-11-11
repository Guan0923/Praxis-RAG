import { describe, expect, it } from "vitest";
import {
  bandRect,
  dragThresholdPx,
  intersects,
  keysInBand,
  mergeSelection,
  type DragRect,
} from "./dragSelect";

function rect(left: number, top: number, right: number, bottom: number): DragRect {
  return { left, top, right, bottom };
}

describe("bandRect", () => {
  it("normalizes a drag towards the bottom right", () => {
    expect(bandRect(10, 20, 40, 60)).toEqual(rect(10, 20, 40, 60));
  });

  it("normalizes a drag towards the top left", () => {
    expect(bandRect(40, 60, 10, 20)).toEqual(rect(10, 20, 40, 60));
  });

  it("normalizes mixed drag directions", () => {
    expect(bandRect(40, 20, 10, 60)).toEqual(rect(10, 20, 40, 60));
    expect(bandRect(10, 60, 40, 20)).toEqual(rect(10, 20, 40, 60));
  });

  it("supports a zero-height horizontal band", () => {
    expect(bandRect(5, 30, 90, 30)).toEqual(rect(5, 30, 90, 30));
  });

  it("supports a zero-width vertical band", () => {
    expect(bandRect(15, 2, 15, 70)).toEqual(rect(15, 2, 15, 70));
  });

  it("returns a zero-size rect when the pointer did not move", () => {
    expect(bandRect(7, 8, 7, 8)).toEqual(rect(7, 8, 7, 8));
  });

  it("handles negative and fractional coordinates", () => {
    expect(bandRect(-10.5, -20, 5, 6.25)).toEqual(rect(-10.5, -20, 5, 6.25));
    expect(bandRect(5, 6.25, -10.5, -20)).toEqual(rect(-10.5, -20, 5, 6.25));
  });
});

describe("intersects", () => {
  const box = rect(10, 10, 50, 50);

  it("is true for identical rectangles", () => {
    expect(intersects(box, rect(10, 10, 50, 50))).toBe(true);
  });

  it("is true for partially overlapping rectangles", () => {
    expect(intersects(box, rect(40, 40, 80, 80))).toBe(true);
    expect(intersects(rect(40, 40, 80, 80), box)).toBe(true);
  });

  it("is true when one rectangle contains the other", () => {
    expect(intersects(box, rect(20, 20, 30, 30))).toBe(true);
    expect(intersects(rect(0, 0, 80, 80), box)).toBe(true);
  });

  it("is false for horizontally disjoint rectangles", () => {
    expect(intersects(box, rect(51, 10, 90, 50))).toBe(false);
    expect(intersects(box, rect(0, 10, 9, 50))).toBe(false);
  });

  it("is false for vertically disjoint rectangles", () => {
    expect(intersects(box, rect(10, 51, 50, 90))).toBe(false);
    expect(intersects(box, rect(10, 0, 50, 9))).toBe(false);
  });

  it("is false for a diagonal miss", () => {
    expect(intersects(box, rect(51, 51, 90, 90))).toBe(false);
  });

  it("is false when the rectangles only touch an edge", () => {
    expect(intersects(box, rect(50, 10, 90, 50))).toBe(false);
    expect(intersects(box, rect(10, 50, 50, 90))).toBe(false);
    expect(intersects(box, rect(0, 0, 10, 10))).toBe(false);
  });

  it("is false when a row corner merely touches the band", () => {
    expect(intersects(rect(50, 50, 60, 60), box)).toBe(false);
  });

  it("is false for a zero-height band crossing the rectangle", () => {
    expect(intersects(rect(0, 30, 100, 30), box)).toBe(false);
    expect(intersects(rect(0, 30, 100, 30), rect(10, 31, 50, 80))).toBe(false);
  });

  it("is false for a zero-width band crossing the rectangle", () => {
    expect(intersects(rect(30, 0, 30, 100), box)).toBe(false);
  });

  it("is false for a point inside the rectangle", () => {
    expect(intersects(rect(30, 30, 30, 30), box)).toBe(false);
  });

  it("is false for a zero-size band outside the rectangle", () => {
    expect(intersects(rect(0, 0, 0, 0), box)).toBe(false);
  });

  it("is symmetric", () => {
    const others = [rect(0, 0, 9, 9), rect(10, 10, 50, 50), rect(50, 50, 60, 60), rect(-5, 30, 15, 40)];
    for (const other of others) expect(intersects(box, other)).toBe(intersects(other, box));
  });
});

describe("keysInBand", () => {
  const rows = [
    { key: "a", rect: rect(0, 0, 200, 40) },
    { key: "b", rect: rect(0, 40, 200, 80) },
    { key: "c", rect: rect(0, 80, 200, 120) },
  ];

  it("returns an empty array for no rows", () => {
    expect(keysInBand([], rect(0, 0, 500, 500))).toEqual([]);
  });

  it("returns an empty array when nothing intersects", () => {
    expect(keysInBand(rows, rect(400, 400, 500, 500))).toEqual([]);
  });

  it("returns the intersecting keys in row order", () => {
    expect(keysInBand(rows, rect(0, 0, 200, 80))).toEqual(["a", "b"]);
    expect(keysInBand(rows, rect(0, 10, 200, 90))).toEqual(["a", "b", "c"]);
    expect(keysInBand(rows, rect(0, 45, 200, 75))).toEqual(["b"]);
  });

  it("does not select rows that only touch the band edge", () => {
    // Row b starts exactly at y=40; a band ending on that border keeps only a.
    expect(keysInBand(rows, rect(0, -10, 200, 40))).toEqual(["a"]);
    // A zero-height band has no area and selects nothing.
    expect(keysInBand(rows, rect(0, 40, 200, 40))).toEqual([]);
  });

  it("deduplicates repeated keys while preserving order", () => {
    const repeated = [...rows, { key: "b", rect: rect(0, 40, 200, 80) }, rows[0]];
    expect(keysInBand(repeated, rect(0, 0, 200, 120))).toEqual(["a", "b", "c"]);
  });

  it("ignores rows to the side of the band", () => {
    expect(keysInBand([...rows, { key: "d", rect: rect(400, 0, 500, 40) }], rect(0, 0, 200, 30))).toEqual([
      "a",
    ]);
  });

  it("does not mutate or reorder the input rows", () => {
    const frozen = Object.freeze([...rows]);
    expect(keysInBand(frozen, rect(0, 0, 200, 120))).toEqual(["a", "b", "c"]);
    expect(frozen.map((row) => row.key)).toEqual(["a", "b", "c"]);
  });
});

describe("mergeSelection", () => {
  it("adds hits after the base without dropping the existing selection", () => {
    expect(mergeSelection(["a", "c"], ["b", "c", "a", "d"])).toEqual(["a", "c", "b", "d"]);
  });

  it("keeps the base when the band hits nothing", () => {
    expect(mergeSelection(["a", "b"], [])).toEqual(["a", "b"]);
  });

  it("uses the hit order for a fresh selection", () => {
    expect(mergeSelection([], ["b", "a"])).toEqual(["b", "a"]);
  });

  it("is monotonic across moves, so a smaller hit set never removes rows", () => {
    const first = mergeSelection(["seed"], ["a", "b"]);
    const second = mergeSelection(first, ["b"]);
    const third = mergeSelection(second, []);
    expect(first).toEqual(["seed", "a", "b"]);
    expect(second).toEqual(["seed", "a", "b"]);
    expect(third).toEqual(["seed", "a", "b"]);
  });

  it("deduplicates both inputs", () => {
    expect(mergeSelection(["a", "a", "b"], ["b", "b", "c"])).toEqual(["a", "b", "c"]);
  });

  it("returns a new array and does not mutate its inputs", () => {
    const base = Object.freeze(["a"]);
    const hits = Object.freeze(["b", "a"]);
    const merged = mergeSelection(base, hits);
    expect(merged).not.toBe(base);
    merged.push("mutation");
    expect(base).toEqual(["a"]);
    expect(hits).toEqual(["b", "a"]);
  });
});

describe("dragThresholdPx", () => {
  it("is the 4px movement threshold documented for the marquee", () => {
    expect(dragThresholdPx).toBe(4);
  });
});
