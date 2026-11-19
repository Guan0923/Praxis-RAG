import { useRef } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { DragRect } from "./dragSelect";
import { useDragSelection } from "./useDragSelection";

type Row = { key: string; rect: DragRect };

const tableRows: Row[] = [
  { key: "a", rect: { left: 0, top: 0, right: 200, bottom: 40 } },
  { key: "b", rect: { left: 0, top: 40, right: 200, bottom: 80 } },
  { key: "c", rect: { left: 0, top: 80, right: 200, bottom: 120 } },
];

function toDomRect(rect: DragRect): DOMRect {
  return {
    left: rect.left,
    top: rect.top,
    right: rect.right,
    bottom: rect.bottom,
    width: rect.right - rect.left,
    height: rect.bottom - rect.top,
    x: rect.left,
    y: rect.top,
    toJSON: () => ({}),
  } as DOMRect;
}

// jsdom has no layout: every row gets a deterministic viewport rect so the
// band hit-test can be asserted without a real table render.
function stubRowRect(element: HTMLElement | null, rect: DragRect) {
  if (!element) return;
  element.getBoundingClientRect = () => toDomRect(rect);
}

function TableHarness({
  selected = [],
  onChange,
  enabled = true,
  rowSelector,
  rows = tableRows,
  renderAsDivs = false,
}: {
  selected?: string[];
  onChange: (keys: string[]) => void;
  enabled?: boolean;
  rowSelector?: string;
  rows?: Row[];
  renderAsDivs?: boolean;
}) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const { dragging, band } = useDragSelection({
    containerRef,
    selectedKeys: selected,
    onChange,
    enabled,
    rowSelector,
  });
  return (
    <div>
      <div ref={containerRef} data-testid="container">
        {renderAsDivs ? (
          rows.map((row) => (
            <div key={row.key} data-row-key={row.key} ref={(element) => stubRowRect(element, row.rect)}>
              {row.key}
            </div>
          ))
        ) : (
          <table>
            <thead>
              <tr>
                <th>列</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr key={row.key} data-row-key={row.key} ref={(element) => stubRowRect(element, row.rect)}>
                  <td>{row.key}</td>
                  <td>
                    <span className="ant-checkbox-wrapper">
                      <input type="checkbox" aria-label={"选择 " + row.key} readOnly />
                    </span>
                  </td>
                  <td>
                    <button type="button" aria-label={"操作 " + row.key}>
                      操作
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      <div
        data-testid="outside-row"
        data-row-key="outside"
        ref={(element) => stubRowRect(element, { left: 0, top: 0, right: 400, bottom: 400 })}
      >
        outside
      </div>
      <output data-testid="dragging">{dragging ? "true" : "false"}</output>
      <output data-testid="band">
        {band ? [band.left, band.top, band.right, band.bottom].join(",") : ""}
      </output>
    </div>
  );
}

function rowElement(key: string): HTMLElement {
  const element = document.querySelector<HTMLElement>(`[data-row-key="${key}"]`);
  if (!element) throw new Error(`row ${key} was not rendered`);
  return element;
}

function firstCell(key: string): HTMLElement {
  const cell = rowElement(key).querySelector<HTMLElement>("td");
  if (!cell) throw new Error(`row ${key} has no cell`);
  return cell;
}

function draggingState(): string {
  return screen.getByTestId("dragging").textContent ?? "";
}

function bandState(): string {
  return screen.getByTestId("band").textContent ?? "";
}

afterEach(() => {
  cleanup();
  document.body.className = "";
  document.body.removeAttribute("style");
});

describe("useDragSelection", () => {
  it("does not start a marquee before the 4px movement threshold", () => {
    const onChange = vi.fn();
    render(<TableHarness selected={["c"]} onChange={onChange} />);

    const downResult = fireEvent.mouseDown(firstCell("a"), { button: 0, clientX: 10, clientY: 10 });
    expect(downResult).toBe(true); // mousedown is never prevented

    // hypot(2, 3) ≈ 3.6 <= 4: still below the threshold.
    const moveResult = fireEvent.mouseMove(window, { clientX: 12, clientY: 13 });
    expect(moveResult).toBe(true); // no preventDefault before the threshold
    expect(onChange).not.toHaveBeenCalled();
    expect(draggingState()).toBe("false");
    expect(bandState()).toBe("");
    expect(document.body.classList.contains("drag-selecting")).toBe(false);
    expect(document.querySelector(".drag-band")).toBeNull();

    // Releasing without crossing the threshold must behave like a plain click.
    fireEvent.mouseUp(window, { clientX: 12, clientY: 13 });
    expect(onChange).not.toHaveBeenCalled();
    expect(document.body.classList.contains("drag-selecting")).toBe(false);
  });

  it("selects every row crossed by the band and keeps the previous selection", () => {
    const onChange = vi.fn();
    render(<TableHarness selected={["c"]} onChange={onChange} />);

    fireEvent.mouseDown(firstCell("a"), { button: 0, clientX: 10, clientY: 10 });
    const moveResult = fireEvent.mouseMove(window, { clientX: 210, clientY: 130 });
    expect(moveResult).toBe(false); // preventDefault once the drag is live

    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenLastCalledWith(["c", "a", "b"]);
    expect(draggingState()).toBe("true");
    expect(bandState()).toBe("10,10,210,130");

    expect(document.body.classList.contains("drag-selecting")).toBe(true);
    expect(document.body.style.userSelect).toBe("none");
    // The hook returns the geometry; the page renders the overlay itself, so
    // the hook must not append a competing `.drag-band` node to the body.
    expect(document.querySelector(".drag-band")).toBeNull();

    // A window mouseup ends the drag and cleans up the overlay/body state.
    fireEvent.mouseUp(window, { clientX: 210, clientY: 130 });
    expect(draggingState()).toBe("false");
    expect(bandState()).toBe("");
    expect(document.body.classList.contains("drag-selecting")).toBe(false);
    expect(document.body.style.userSelect).toBe("");
    expect(onChange).toHaveBeenCalledTimes(1); // same hit set on release: no duplicate emit
  });

  it("keeps the rows the band crossed when it shrinks away again", () => {
    const onChange = vi.fn();
    render(<TableHarness onChange={onChange} />);

    // Start below the rows, drag up across rows b/c, then drag back down so the
    // band no longer overlaps any row. The rows must stay selected.
    fireEvent.mouseDown(screen.getByTestId("container"), { button: 0, clientX: 10, clientY: 300 });
    fireEvent.mouseMove(window, { clientX: 190, clientY: 60 });
    expect(onChange).toHaveBeenLastCalledWith(["b", "c"]);
    expect(onChange).toHaveBeenCalledTimes(1);

    fireEvent.mouseMove(window, { clientX: 190, clientY: 320 });
    expect(onChange).toHaveBeenLastCalledWith(["b", "c"]);
    expect(onChange).toHaveBeenCalledTimes(1);

    fireEvent.mouseUp(window, { clientX: 190, clientY: 320 });
    expect(onChange).toHaveBeenCalledTimes(1);
  });

  it("keeps rows selected when scrolling moves them out from under the band", () => {
    const onChange = vi.fn();
    render(<TableHarness onChange={onChange} />);

    fireEvent.mouseDown(firstCell("a"), { button: 0, clientX: 10, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 190, clientY: 45 });
    expect(onChange).toHaveBeenLastCalledWith(["a", "b"]);

    // Simulate a scroll: every row moves down while the pointer stays put, so
    // the band no longer overlaps rows a/b. Nothing may be deselected.
    stubRowRect(rowElement("a"), { left: 0, top: 200, right: 200, bottom: 240 });
    stubRowRect(rowElement("b"), { left: 0, top: 240, right: 200, bottom: 280 });
    stubRowRect(rowElement("c"), { left: 0, top: 280, right: 200, bottom: 320 });
    fireEvent.mouseMove(window, { clientX: 190, clientY: 60 });
    expect(onChange).toHaveBeenLastCalledWith(["a", "b"]);
    expect(onChange).toHaveBeenCalledTimes(1);

    fireEvent.mouseUp(window, { clientX: 190, clientY: 60 });
    expect(onChange).toHaveBeenCalledTimes(1);
  });

  it("keeps the selection additive when Shift/Ctrl/Meta are held", () => {
    const onChange = vi.fn();
    render(<TableHarness selected={["a", "c"]} onChange={onChange} />);

    // The band covers already-selected row a and row b. No modifier turns the
    // marquee into a replace or toggle gesture: everything stays selected.
    fireEvent.mouseDown(rowElement("a"), {
      button: 0,
      shiftKey: true,
      ctrlKey: true,
      metaKey: true,
      clientX: 10,
      clientY: 10,
    });
    fireEvent.mouseMove(window, { clientX: 190, clientY: 45 });
    expect(onChange).toHaveBeenLastCalledWith(["a", "c", "b"]);
    fireEvent.mouseUp(window, { clientX: 190, clientY: 45 });
  });

  it("selects rows crossed by a nearly-horizontal band", () => {
    const onChange = vi.fn();
    render(<TableHarness onChange={onChange} />);

    // One pixel of vertical movement keeps the band's area positive, so it
    // still selects the row it crosses (touching-only bands never select).
    fireEvent.mouseDown(rowElement("b"), { button: 0, clientX: 10, clientY: 60 });
    fireEvent.mouseMove(window, { clientX: 150, clientY: 61 });
    expect(onChange).toHaveBeenLastCalledWith(["b"]);
    fireEvent.mouseUp(window, { clientX: 150, clientY: 61 });
  });

  it("Escape cancels the drag and restores the snapshot", () => {
    const onChange = vi.fn();
    render(<TableHarness selected={["c"]} onChange={onChange} />);

    fireEvent.mouseDown(rowElement("a"), { button: 0, clientX: 10, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 190, clientY: 39 });
    expect(onChange).toHaveBeenLastCalledWith(["c", "a"]);

    fireEvent.keyDown(window, { key: "Escape" });
    expect(onChange).toHaveBeenLastCalledWith(["c"]);
    expect(draggingState()).toBe("false");
    expect(bandState()).toBe("");
    expect(document.querySelector(".drag-band")).toBeNull();
    expect(document.body.classList.contains("drag-selecting")).toBe(false);

    // The drag really ended: later moves no longer update the selection.
    fireEvent.mouseMove(window, { clientX: 210, clientY: 130 });
    expect(onChange).toHaveBeenLastCalledWith(["c"]);
    expect(onChange).toHaveBeenCalledTimes(2);
  });

  it("ignores mousedowns on interactive elements", () => {
    const onChange = vi.fn();
    render(<TableHarness onChange={onChange} />);

    const checkboxWrapper = rowElement("a").querySelector<HTMLElement>(".ant-checkbox-wrapper");
    expect(checkboxWrapper).toBeTruthy();
    const header = document.querySelector<HTMLElement>("th");
    expect(header).toBeTruthy();

    for (const target of [
      screen.getByRole("button", { name: "操作 a" }),
      rowElement("a").querySelector("input")!,
      checkboxWrapper!,
      header!,
    ]) {
      fireEvent.mouseDown(target, { button: 0, clientX: 10, clientY: 10 });
      fireEvent.mouseMove(window, { clientX: 210, clientY: 130 });
      expect(onChange).not.toHaveBeenCalled();
      expect(draggingState()).toBe("false");
      expect(document.body.classList.contains("drag-selecting")).toBe(false);
      fireEvent.mouseUp(window, { clientX: 210, clientY: 130 });
    }
  });

  it("ignores non-left mouse buttons", () => {
    const onChange = vi.fn();
    render(<TableHarness onChange={onChange} />);

    fireEvent.mouseDown(rowElement("a"), { button: 2, clientX: 10, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 210, clientY: 130 });
    expect(onChange).not.toHaveBeenCalled();
    fireEvent.mouseUp(window, { clientX: 210, clientY: 130 });
  });

  it("ignores mousedowns outside the container and never selects outside rows", () => {
    const onChange = vi.fn();
    render(<TableHarness onChange={onChange} />);

    // The outside row is a matching `[data-row-key]` element with a huge rect,
    // but it is not inside the container, so it cannot start a drag.
    fireEvent.mouseDown(screen.getByTestId("outside-row"), { button: 0, clientX: 10, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 210, clientY: 130 });
    expect(onChange).not.toHaveBeenCalled();

    // A drag started inside the container also never picks the outside row up.
    fireEvent.mouseDown(rowElement("a"), { button: 0, clientX: 10, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 210, clientY: 130 });
    expect(onChange).toHaveBeenLastCalledWith(["a", "b", "c"]);
    fireEvent.mouseUp(window, { clientX: 210, clientY: 130 });
  });

  it("does nothing while enabled is false", () => {
    const onChange = vi.fn();
    const { rerender } = render(<TableHarness onChange={onChange} enabled={false} />);

    fireEvent.mouseDown(rowElement("a"), { button: 0, clientX: 10, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 210, clientY: 130 });
    expect(onChange).not.toHaveBeenCalled();
    expect(document.body.classList.contains("drag-selecting")).toBe(false);
    fireEvent.mouseUp(window, { clientX: 210, clientY: 130 });

    rerender(<TableHarness onChange={onChange} enabled />);
    fireEvent.mouseDown(rowElement("a"), { button: 0, clientX: 10, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 210, clientY: 130 });
    expect(onChange).toHaveBeenLastCalledWith(["a", "b", "c"]);
    fireEvent.mouseUp(window, { clientX: 210, clientY: 130 });
  });

  it("supports a custom row selector", () => {
    const onChange = vi.fn();
    render(<TableHarness onChange={onChange} rows={tableRows} renderAsDivs rowSelector="[data-row-key]" />);

    fireEvent.mouseDown(rowElement("a"), { button: 0, clientX: 10, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 190, clientY: 39 });
    expect(onChange).toHaveBeenLastCalledWith(["a"]);
    fireEvent.mouseUp(window, { clientX: 190, clientY: 39 });
  });

  it("cleans up listeners and body class when unmounted mid-drag", () => {
    const onChange = vi.fn();
    const { unmount } = render(<TableHarness onChange={onChange} />);

    fireEvent.mouseDown(rowElement("a"), { button: 0, clientX: 10, clientY: 10 });
    fireEvent.mouseMove(window, { clientX: 210, clientY: 130 });
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(document.body.classList.contains("drag-selecting")).toBe(true);

    unmount();
    expect(document.body.classList.contains("drag-selecting")).toBe(false);
    expect(document.body.style.userSelect).toBe("");
    expect(document.querySelector(".drag-band")).toBeNull();

    // Removed listeners must not call onChange after unmount.
    fireEvent.mouseMove(window, { clientX: 300, clientY: 300 });
    fireEvent.mouseUp(window, { clientX: 300, clientY: 300 });
    expect(onChange).toHaveBeenCalledTimes(1);
  });
});
