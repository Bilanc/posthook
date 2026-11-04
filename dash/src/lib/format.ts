import { format, parseISO } from "date-fns";

const num = new Intl.NumberFormat("en-US");
const compact = new Intl.NumberFormat("en-US", {
  notation: "compact",
  maximumFractionDigits: 1,
});

export function fmtNum(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return "—";
  return num.format(Math.round(n));
}

export function fmtCompact(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return "—";
  return compact.format(n);
}

// 0..1 ratio -> "62%"
export function fmtPct(ratio: number | null | undefined, digits = 0): string {
  if (ratio == null || !Number.isFinite(ratio)) return "—";
  return `${(ratio * 100).toFixed(digits)}%`;
}

// Minutes -> "12m" / "1h 05m" / "14h". Hours are rounded past 10h so a tile
// never reads "38h 17m".
export function fmtMinutes(minutes: number | null | undefined): string {
  if (minutes == null || !Number.isFinite(minutes)) return "—";
  const m = Math.max(0, Math.round(minutes));
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h >= 10) return `${h}h`;
  return `${h}h ${String(m % 60).padStart(2, "0")}m`;
}

export function fmtHours(hours: number | null | undefined): string {
  if (hours == null || !Number.isFinite(hours)) return "—";
  return fmtMinutes(hours * 60);
}

// Milliseconds -> "0.4s" / "2m 48s".
export function fmtMs(ms: number | null | undefined): string {
  if (ms == null || !Number.isFinite(ms)) return "";
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  const s = Math.round(ms / 1000);
  return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
}

export function fmtTime(iso: string | null | undefined): string {
  if (!iso) return "—";
  try {
    return format(parseISO(iso), "HH:mm");
  } catch {
    return iso;
  }
}

export function fmtDateTime(iso: string | null | undefined): string {
  if (!iso) return "—";
  try {
    return format(parseISO(iso), "d MMM, HH:mm");
  } catch {
    return iso;
  }
}

// "2026-11-04" (local day key) -> "Wed 4 Nov"
export function fmtDay(day: string): string {
  try {
    return format(parseISO(day), "EEE d MMM");
  } catch {
    return day;
  }
}

// Collapse whitespace and cut a prompt to one line for titles and previews.
export function oneLine(text: string, max = 110): string {
  const t = text.replace(/\s+/g, " ").trim();
  return t.length > max ? `${t.slice(0, max - 1)}…` : t;
}
