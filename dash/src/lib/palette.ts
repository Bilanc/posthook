// Chart colours. Categorical slots were validated for colour-vision-deficiency
// separation on the dash's dark surface (#121214); keep the order, never cycle
// past it. Colour follows the entity: an agent keeps its slot whatever the
// filter shows.

export const SERIES = ["#6366f1", "#d95926", "#199e70", "#c98500", "#d55181", "#3987e5"] as const;

const KNOWN_AGENTS: Record<string, string> = {
  "claude-code": SERIES[0],
  codex: SERIES[1],
  cursor: SERIES[2],
  copilot: SERIES[3],
  gemini: SERIES[4],
};

// Unknown agents get a stable slot from a hash of their slug, so the same
// agent is the same colour on every page and every reload.
export function agentColor(slug: string): string {
  if (KNOWN_AGENTS[slug]) return KNOWN_AGENTS[slug];
  let h = 0;
  for (const ch of slug) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return SERIES[3 + (h % 3)];
}

export const FAMILY_COLOR = {
  shell: SERIES[0],
  read: SERIES[5],
  edit: SERIES[2],
  agent: SERIES[1],
  mcp: SERIES[3],
  web: SERIES[4],
  other: "#5f5f66",
} as const;

// Ordinal ramp for funnel stages (one hue, light -> dark).
export const ORDINAL = ["#3987e5", "#256abf", "#184f95"] as const;

// Sequential ramp for the heatmap, near-surface -> brightest.
export const SEQUENTIAL = ["#15233a", "#184f95", "#1c5cab", "#256abf", "#2a78d6", "#3987e5", "#6da7ec"] as const;

export const ACCENT = "#6366f1";
export const MUTED_MARK = "#5f5f66";
export const COMMIT_MARK = SERIES[2];
