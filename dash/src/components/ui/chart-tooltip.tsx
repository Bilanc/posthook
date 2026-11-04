"use client";

import { useEffect, useRef } from "react";

// The one hover layer every chart shares. A mark opts in with
// data-tip="first line\nsecond line"; pointer hover, keyboard focus and touch
// show the text in a floating box beside the pointer, clamped to the viewport.
// Native <title> tooltips are too slow to feel like interaction, so charts use
// this instead and keep aria-labels for assistive tech.
export function ChartTooltip() {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    let current: Element | null = null;

    const place = (x: number, y: number) => {
      const w = el.offsetWidth;
      const h = el.offsetHeight;
      const left = Math.max(8, Math.min(x + 14, window.innerWidth - w - 8));
      const above = y - h - 12;
      const top = above < 8 ? y + 18 : above;
      el.style.left = `${left}px`;
      el.style.top = `${top}px`;
    };
    const show = (t: Element, x: number, y: number) => {
      const text = t.getAttribute("data-tip") ?? "";
      if (!text) return;
      current = t;
      el.textContent = text;
      el.hidden = false;
      place(x, y);
    };
    const hide = () => {
      current = null;
      el.hidden = true;
    };
    const find = (e: Event) =>
      e.target instanceof Element ? e.target.closest("[data-tip]") : null;

    const onMove = (e: MouseEvent) => {
      const t = find(e);
      if (!t) {
        if (current) hide();
        return;
      }
      if (t !== current) show(t, e.clientX, e.clientY);
      else place(e.clientX, e.clientY);
    };
    const onFocus = (e: FocusEvent) => {
      const t = find(e);
      if (!t) return;
      const r = t.getBoundingClientRect();
      show(t, r.left + r.width / 2, r.top);
    };
    const onTouch = (e: TouchEvent) => {
      const t = find(e);
      if (!t) {
        hide();
        return;
      }
      const touch = e.touches[0];
      if (touch) show(t, touch.clientX, touch.clientY);
    };

    document.addEventListener("mousemove", onMove);
    document.addEventListener("mouseleave", hide);
    document.addEventListener("focusin", onFocus);
    document.addEventListener("focusout", hide);
    document.addEventListener("touchstart", onTouch, { passive: true });
    window.addEventListener("scroll", hide, { passive: true });
    return () => {
      document.removeEventListener("mousemove", onMove);
      document.removeEventListener("mouseleave", hide);
      document.removeEventListener("focusin", onFocus);
      document.removeEventListener("focusout", hide);
      document.removeEventListener("touchstart", onTouch);
      window.removeEventListener("scroll", hide);
    };
  }, []);

  return (
    <div
      ref={ref}
      hidden
      role="tooltip"
      className="pointer-events-none fixed z-50 max-w-[320px] whitespace-pre-line rounded-md border border-[var(--color-border-2)] bg-[var(--color-bg)] px-2.5 py-1.5 text-xs leading-snug text-[var(--color-fg)] shadow-lg shadow-black/40"
    />
  );
}
