import { eachDayOfInterval, format, isValid, parseISO } from "date-fns";
import type { DailyActivityRow } from "@/lib/queries/activity";
import { windowDays, type Filters, type SearchParams } from "@/lib/filters";

// Pure helpers shared by the overview panels: the day axis, per-day sums,
// safe ratios and the window phrases. No DB access here.

// Every local day in the window as YYYY-MM-DD keys, in order. Falls back to
// the days present in the rows when the range is missing or malformed, so a
// bad `from`/`to` never throws from inside a chart.
export function windowDayKeys(f: Filters, rows: DailyActivityRow[]): string[] {
  if (f.from && f.to) {
    const start = parseISO(f.from);
    const end = parseISO(f.to);
    if (isValid(start) && isValid(end) && start <= end) {
      return eachDayOfInterval({ start, end }).map((d) => format(d, "yyyy-MM-dd"));
    }
  }
  return [...new Set(rows.map((r) => r.day))].sort();
}

// Daily series over `days` (missing days are 0).
export function sumByDay(
  rows: DailyActivityRow[],
  days: string[],
  pick: (r: DailyActivityRow) => number,
): number[] {
  const acc = new Map<string, number>();
  for (const r of rows) acc.set(r.day, (acc.get(r.day) ?? 0) + pick(r));
  return days.map((d) => acc.get(d) ?? 0);
}

// Rows grouped by day, for per-day breakdowns (agent segments, titles).
export function groupByDay(rows: DailyActivityRow[]): Map<string, DailyActivityRow[]> {
  const m = new Map<string, DailyActivityRow[]>();
  for (const r of rows) {
    const list = m.get(r.day);
    if (list) list.push(r);
    else m.set(r.day, [r]);
  }
  return m;
}

// null instead of NaN/Infinity so formatters print "—".
export function ratio(num: number, den: number): number | null {
  if (!Number.isFinite(num) || !Number.isFinite(den) || den <= 0) return null;
  return num / den;
}

// 1-2-5 nice ceiling for an axis max; never 0.
export function niceMax(max: number): number {
  if (!Number.isFinite(max) || max <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(max)));
  const n = max / p;
  const s = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
  return s * p;
}

// "last 31 days", or the date range when the window has no day count.
export function windowLabel(f: Filters): string {
  const n = windowDays(f);
  if (n) return `last ${n} day${n === 1 ? "" : "s"}`;
  return rangeLabel(f);
}

// "5 Oct – 4 Nov"
export function rangeLabel(f: Filters): string {
  const fmt = (s: string | null) => {
    if (!s) return "…";
    const d = parseISO(s);
    return isValid(d) ? format(d, "d MMM") : s;
  };
  return `${fmt(f.from)} – ${fmt(f.to)}`;
}

// "previous 31 days" / "previous window"
export function priorLabel(f: Filters): string {
  const n = windowDays(f);
  return n ? `previous ${n} day${n === 1 ? "" : "s"}` : "previous window";
}

// "12 Oct"
export function fmtDayShort(day: string): string {
  const d = parseISO(day);
  return isValid(d) ? format(d, "d MMM") : day;
}

export function isMonday(day: string): boolean {
  const d = parseISO(day);
  return isValid(d) && d.getDay() === 1;
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return n === 1 ? one : many;
}

export type Measure = "active" | "lines";

export function parseMeasure(v: string | string[] | undefined): Measure {
  const one = Array.isArray(v) ? v[0] : v;
  return one === "lines" ? "lines" : "active";
}

// Current search params with `measure` swapped, for the activity toggle links.
export function hrefWithMeasure(sp: SearchParams, measure: Measure): string {
  const next = new URLSearchParams();
  for (const [k, v] of Object.entries(sp)) {
    if (k === "measure" || v == null) continue;
    if (Array.isArray(v)) for (const x of v) next.append(k, x);
    else next.set(k, v);
  }
  next.set("measure", measure);
  return `/?${next.toString()}`;
}

// SVG path for a bar with rounded corners on one end only: the data end gets
// the radius, the baseline stays square.
export function roundedEndRect(
  x: number,
  y: number,
  w: number,
  h: number,
  r: number,
  end: "top" | "right",
): string {
  if (w <= 0 || h <= 0) return "";
  const rr = Math.max(0, Math.min(r, end === "top" ? Math.min(w / 2, h) : Math.min(h / 2, w)));
  const f = (n: number) => n.toFixed(1);
  if (end === "top") {
    return `M${f(x)} ${f(y + h)} V${f(y + rr)} A${f(rr)} ${f(rr)} 0 0 1 ${f(x + rr)} ${f(y)} H${f(x + w - rr)} A${f(rr)} ${f(rr)} 0 0 1 ${f(x + w)} ${f(y + rr)} V${f(y + h)} Z`;
  }
  return `M${f(x)} ${f(y)} H${f(x + w - rr)} A${f(rr)} ${f(rr)} 0 0 1 ${f(x + w)} ${f(y + rr)} V${f(y + h - rr)} A${f(rr)} ${f(rr)} 0 0 1 ${f(x + w - rr)} ${f(y + h)} H${f(x)} Z`;
}
