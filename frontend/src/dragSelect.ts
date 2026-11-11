// Pure geometry and selection-set helpers for mouse drag (marquee) selection.
// They are kept free of DOM and React so the hit-testing and merge rules can be
// unit tested exhaustively without a layout engine.

export type DragRect = { left: number; top: number; right: number; bottom: number };

/** Normalizes two pointer positions into a rectangle, drag direction free. */
export function bandRect(startX: number, startY: number, x: number, y: number): DragRect {
  return {
    left: Math.min(startX, x),
    top: Math.min(startY, y),
    right: Math.max(startX, x),
    bottom: Math.max(startY, y),
  };
}

/**
 * Positive-area intersection: the rectangles must overlap by more than an
 * edge, so a band dragged exactly onto a row border does not select that row.
 * Degenerate rectangles (zero width or height) have no area and therefore
 * never intersect, matching how adjacent table rows must not both be picked
 * up by a band that ends exactly on their shared border.
 */
export function intersects(a: DragRect, b: DragRect): boolean {
  // Degenerate rectangles (and real DOM rows in jsdom without layout) have no
  // area and can never overlap another rectangle by more than a line.
  if (a.left >= a.right || a.top >= a.bottom) return false;
  if (b.left >= b.right || b.top >= b.bottom) return false;
  return a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom;
}

/** Keys of the rows whose rect intersects the band, in the given row order. */
export function keysInBand(
  rows: ReadonlyArray<{ key: string; rect: DragRect }>,
  band: DragRect,
): string[] {
  const keys: string[] = [];
  const seen = new Set<string>();
  for (const row of rows) {
    if (seen.has(row.key)) continue;
    if (intersects(row.rect, band)) {
      seen.add(row.key);
      keys.push(row.key);
    }
  }
  return keys;
}

/** Deduplicates keys while preserving their first-seen order. */
function unique(keys: readonly string[]): string[] {
  const seen = new Set<string>();
  const result: string[] = [];
  for (const key of keys) {
    if (seen.has(key)) continue;
    seen.add(key);
    result.push(key);
  }
  return result;
}

/**
 * Adds `hits` to `base` without ever removing a key: the base order is kept and
 * hit-only keys are appended in hit order. This is the marquee rule — dragging
 * only ever grows the selection, so shrinking the band, dragging over another
 * area, or scrolling the table/window can never deselect a row the user already
 * touched. Deselecting a row is an explicit checkbox click instead.
 *
 * The input arrays are never mutated.
 */
export function mergeSelection(base: readonly string[], hits: readonly string[]): string[] {
  const result = unique(base);
  const seen = new Set(result);
  for (const key of hits) {
    if (seen.has(key)) continue;
    seen.add(key);
    result.push(key);
  }
  return result;
}

/** Movement in pixels before a mousedown turns into a marquee drag. */
export const dragThresholdPx = 4;
