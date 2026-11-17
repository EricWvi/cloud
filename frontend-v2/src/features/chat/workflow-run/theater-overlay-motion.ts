import type { MutableRefObject } from "react";

interface AnimateOverlayWidthOptions {
  animationRef: MutableRefObject<number | null>;
  duration: number;
  fromWidth: number;
  onCollapsed: () => void;
  onComplete?: () => void;
  onFrame: (width: number) => void;
  targetWidth: number;
}

/** Stops a scripted overlay width settle so pointer drag always wins. */
export function cancelOverlayWidthAnimation(
  animationRef: MutableRefObject<number | null>,
): void {
  if (animationRef.current === null) {
    return;
  }
  window.cancelAnimationFrame(animationRef.current);
  animationRef.current = null;
}

/**
 * Animates an overlay panel width with ease-out.
 * Used so the Theater stage stays full-bleed while the inspector opens.
 */
export function animateOverlayWidth({
  animationRef,
  duration,
  fromWidth,
  onCollapsed,
  onComplete,
  onFrame,
  targetWidth,
}: AnimateOverlayWidthOptions): void {
  cancelOverlayWidthAnimation(animationRef);
  const reducedMotion =
    window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;

  const finish = (): void => {
    onFrame(targetWidth);
    if (targetWidth === 0) {
      onCollapsed();
    }
    onComplete?.();
  };

  if (reducedMotion || Math.abs(fromWidth - targetWidth) < 1) {
    finish();
    return;
  }

  // Anchor to the first rAF timestamp so progress uses the same clock as frames even when rAF and
  // performance.now() have different origins (e.g. after module-load offset in jsdom).
  let startedAt: number | null = null;
  const animate = (now: number): void => {
    if (startedAt === null) {
      startedAt = now;
    }
    const progress = duration <= 0 ? 1 : Math.max(0, Math.min(1, (now - startedAt) / duration));
    const easedProgress = 1 - (1 - progress) ** 3;
    onFrame(fromWidth + (targetWidth - fromWidth) * easedProgress);
    if (progress < 1) {
      animationRef.current = window.requestAnimationFrame(animate);
      return;
    }
    animationRef.current = null;
    finish();
  };
  animationRef.current = window.requestAnimationFrame(animate);
}
