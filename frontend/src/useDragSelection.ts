import { useEffect, useRef, useState, type RefObject } from "react";
import {
  bandRect,
  dragThresholdPx,
  keysInBand,
  mergeSelection,
  type DragRect,
} from "./dragSelect";

export type UseDragSelectionOptions = {
  containerRef: RefObject<HTMLElement | null>;
  selectedKeys: readonly string[];
  onChange: (keys: string[]) => void;
  enabled?: boolean;
  /** CSS selector of the selectable rows; defaults to `tr[data-row-key]`. */
  rowSelector?: string;
};

const defaultRowSelector = "tr[data-row-key]";

// Elements that own pointer interaction. A mousedown on any of them must not
// start a marquee, otherwise checkbox clicks, links, header sorters, switches
// and pagination buttons would stop working.
const interactiveSelector = [
  "button",
  "a",
  "input",
  "textarea",
  "select",
  "label",
  '[role="button"]',
  '[role="switch"]',
  ".ant-select",
  ".ant-checkbox-wrapper",
  ".ant-switch",
  ".ant-pagination",
  "th",
].join(", ");

function isInteractive(target: Element): boolean {
  return target.closest(interactiveSelector) !== null;
}

function collectRows(root: HTMLElement, rowSelector: string): Array<{ key: string; rect: DragRect }> {
  const rows: Array<{ key: string; rect: DragRect }> = [];
  root.querySelectorAll<HTMLElement>(rowSelector).forEach((row) => {
    const key = row.dataset.rowKey;
    if (!key) return;
    const box = row.getBoundingClientRect();
    rows.push({ key, rect: { left: box.left, top: box.top, right: box.right, bottom: box.bottom } });
  });
  return rows;
}

function clearTextSelection() {
  const selection = typeof window.getSelection === "function" ? window.getSelection() : null;
  if (selection && typeof selection.removeAllRanges === "function") selection.removeAllRanges();
}

function sameKeys(a: readonly string[], b: readonly string[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i += 1) if (a[i] !== b[i]) return false;
  return true;
}

/**
 * Turns a left-mouse drag inside `containerRef` into a marquee selection over
 * the rows matched by `rowSelector` (`data-row-key`). Selection is reported
 * through `onChange`; the returned `band`/`dragging` values are the viewport
 * geometry and visibility of the marquee overlay, which the caller renders as
 * its own `.drag-band` element. The hook only touches global gesture state
 * (body/styles) and never appends DOM of its own.
 *
 * Marquee selection is additive: rows already selected stay selected, and every
 * row the band ever covers during one gesture is added to the selection. Moving
 * or shrinking the band, starting another drag somewhere else, and scrolling the
 * table or window therefore never deselect a row. A row leaves the selection
 * only through an explicit checkbox click; Escape restores the pre-drag
 * selection and cancels the gesture.
 *
 * A drag only starts after `dragThresholdPx`, so plain clicks and checkbox
 * clicks inside the table keep working. Move/up/escape listeners live on
 * `window`, so the drag survives leaving the container. Every listener, the
 * `drag-selecting` body class and its inline cursor/user-select styles are
 * removed when the drag ends or the hook unmounts.
 */
export function useDragSelection(options: UseDragSelectionOptions): {
  dragging: boolean;
  band: DragRect | null;
} {
  const { containerRef, selectedKeys, onChange, enabled = true, rowSelector = defaultRowSelector } =
    options;
  const [dragging, setDragging] = useState(false);
  const [band, setBand] = useState<DragRect | null>(null);

  // Event handlers read the latest props without re-subscribing every render.
  const latest = useRef({ selectedKeys, onChange, rowSelector });
  latest.current = { selectedKeys, onChange, rowSelector };

  useEffect(() => {
    if (!enabled || typeof document === "undefined") return undefined;

    let sessionOpen = false;
    let dragActive = false;
    let startX = 0;
    let startY = 0;
    let snapshot: string[] = [];
    // Grows monotonically for the whole gesture; the pre-drag snapshot is only
    // used to restore the selection when Escape cancels the drag.
    let selection: string[] = [];
    let lastSelection: string[] | null = null;
    let bodyStyles: { userSelect: string; cursor: string } | null = null;

    const emit = (keys: string[]) => {
      if (lastSelection && sameKeys(lastSelection, keys)) return;
      lastSelection = keys;
      latest.current.onChange(keys);
    };

    const applyBodyStyles = () => {
      if (bodyStyles) return;
      const body = document.body;
      bodyStyles = { userSelect: body.style.userSelect, cursor: body.style.cursor };
      body.classList.add("drag-selecting");
      body.style.userSelect = "none";
      body.style.cursor = "crosshair";
    };

    const restoreBodyStyles = () => {
      const body = document.body;
      body.classList.remove("drag-selecting");
      if (!bodyStyles) return;
      body.style.userSelect = bodyStyles.userSelect;
      body.style.cursor = bodyStyles.cursor;
      bodyStyles = null;
    };

    const removeWindowListeners = () => {
      window.removeEventListener("mousemove", onMouseMove, true);
      window.removeEventListener("mouseup", onMouseUp, true);
      window.removeEventListener("keydown", onKeyDown, true);
    };

    const teardown = () => {
      sessionOpen = false;
      dragActive = false;
      removeWindowListeners();
      restoreBodyStyles();
      setDragging(false);
      setBand(null);
    };

    const finish = (restoreSnapshot: boolean) => {
      if (!sessionOpen) return;
      if (restoreSnapshot && dragActive) emit(snapshot);
      teardown();
    };

    const updateAt = (x: number, y: number, source?: MouseEvent) => {
      if (!sessionOpen) return;
      if (!dragActive) {
        if (Math.hypot(x - startX, y - startY) <= dragThresholdPx) return;
        // Only past the threshold do we take over the gesture: before that the
        // browser keeps handling clicks, text selection and checkbox toggles.
        dragActive = true;
        setDragging(true);
        applyBodyStyles();
        clearTextSelection();
      }
      source?.preventDefault();

      const rect = bandRect(startX, startY, x, y);
      setBand(rect);

      const root = containerRef.current;
      if (!root) return;
      const hits = keysInBand(collectRows(root, latest.current.rowSelector), rect);
      if (hits.length === 0) return;
      // Only ever add keys: the band is a "paint" cursor. This keeps rows
      // selected when the band shrinks, moves away, or when scrolling shifts
      // the rows under it.
      selection = mergeSelection(selection, hits);
      emit(selection);
    };

    function onMouseMove(event: MouseEvent) {
      updateAt(event.clientX, event.clientY, event);
    }

    function onMouseUp(event: MouseEvent) {
      if (!sessionOpen) return;
      // Record the release position as the final band state before ending.
      // A release without a qualifying move keeps behaving like a plain click.
      if (dragActive) updateAt(event.clientX, event.clientY);
      finish(false);
    }

    function onKeyDown(event: KeyboardEvent) {
      if (event.key !== "Escape" && event.key !== "Esc") return;
      finish(true);
    }

    function onMouseDown(event: MouseEvent) {
      if (sessionOpen) return;
      if (event.button !== 0) return;
      const root = containerRef.current;
      if (!root) return;
      const target = event.target;
      if (!(target instanceof Element) || !root.contains(target)) return;
      if (isInteractive(target)) return;

      sessionOpen = true;
      dragActive = false;
      startX = event.clientX;
      startY = event.clientY;
      snapshot = [...latest.current.selectedKeys];
      selection = snapshot;
      lastSelection = null;

      // Capture phase: table internals (or a nested handler) must not be able
      // to swallow the move/up events that keep the marquee alive.
      window.addEventListener("mousemove", onMouseMove, true);
      window.addEventListener("mouseup", onMouseUp, true);
      window.addEventListener("keydown", onKeyDown, true);
    }

    document.addEventListener("mousedown", onMouseDown, true);
    return () => {
      document.removeEventListener("mousedown", onMouseDown, true);
      if (sessionOpen) teardown();
      // Defensive: never leave the drag feedback behind, even if this effect
      // is torn down while a drag is in flight.
      restoreBodyStyles();
    };
  }, [containerRef, enabled]);

  return { dragging, band };
}
