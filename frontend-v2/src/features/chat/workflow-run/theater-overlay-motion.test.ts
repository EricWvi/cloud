import { afterEach, describe, expect, it, vi } from "vitest";
import { animateOverlayWidth } from "./theater-overlay-motion";

describe("animateOverlayWidth", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("anchors progress to the first animation frame clock rather than performance.now()", () => {
    const frames: Array<(time: number) => void> = [];
    vi.spyOn(window, "matchMedia").mockReturnValue({
      matches: false,
    } as MediaQueryList);
    vi.spyOn(window, "requestAnimationFrame").mockImplementation((cb) => {
      frames.push(cb);
      return frames.length;
    });
    vi.spyOn(window, "cancelAnimationFrame").mockImplementation(() => undefined);
    // Simulate a large offset between performance.now() and rAF frame timestamps.
    vi.spyOn(performance, "now").mockReturnValue(10_000);

    const widths: number[] = [];
    const onCollapsed = vi.fn();
    const onComplete = vi.fn();
    const animationRef = { current: null as number | null };

    animateOverlayWidth({
      animationRef,
      duration: 200,
      fromWidth: 0,
      targetWidth: 320,
      onFrame: (width) => widths.push(width),
      onCollapsed,
      onComplete,
    });

    expect(frames).toHaveLength(1);
    frames[0]!(50);
    frames[1]!(150);
    frames[2]!(250);

    expect(widths[0]).toBe(0);
    expect(widths[1]).toBe(280);
    expect(widths[2]).toBe(320);
    expect(onCollapsed).not.toHaveBeenCalled();
    expect(onComplete).toHaveBeenCalledTimes(1);
    expect(animationRef.current).toBeNull();
  });
});
