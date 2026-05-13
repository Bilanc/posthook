// Prices are USD per 1M tokens. Approximate, easy to keep up-to-date in one place.
// The matcher is a slug prefix so we handle versioned model IDs gracefully
// (e.g. "claude-sonnet-4-6-20250929" matches "claude-sonnet-4-6").

export interface Price {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

interface PriceEntry {
  matchPrefix: string;
  price: Price;
}

// Ordered most-specific-first so longer prefixes win.
const PRICES: PriceEntry[] = [
  { matchPrefix: "claude-opus-4-7", price: { input: 15, output: 75, cacheRead: 1.5, cacheWrite: 18.75 } },
  { matchPrefix: "claude-opus-4-6", price: { input: 15, output: 75, cacheRead: 1.5, cacheWrite: 18.75 } },
  { matchPrefix: "claude-opus", price: { input: 15, output: 75, cacheRead: 1.5, cacheWrite: 18.75 } },
  { matchPrefix: "claude-sonnet-4-6", price: { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75 } },
  { matchPrefix: "claude-sonnet", price: { input: 3, output: 15, cacheRead: 0.3, cacheWrite: 3.75 } },
  { matchPrefix: "claude-haiku-4-5", price: { input: 1, output: 5, cacheRead: 0.1, cacheWrite: 1.25 } },
  { matchPrefix: "claude-haiku", price: { input: 0.8, output: 4, cacheRead: 0.08, cacheWrite: 1 } },
  { matchPrefix: "gpt-5", price: { input: 5, output: 15, cacheRead: 0.5, cacheWrite: 6.25 } },
  { matchPrefix: "gpt-4", price: { input: 2.5, output: 10, cacheRead: 0.25, cacheWrite: 3.125 } },
  { matchPrefix: "o1", price: { input: 15, output: 60, cacheRead: 7.5, cacheWrite: 18.75 } },
];

export function priceFor(model: string | null | undefined): Price | null {
  if (!model) return null;
  for (const entry of PRICES) {
    if (model.startsWith(entry.matchPrefix)) return entry.price;
  }
  return null;
}

export interface TokenCounts {
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

export function computeCost(tokens: TokenCounts, model: string | null | undefined): number {
  const p = priceFor(model);
  if (!p) return 0;
  return (
    (tokens.input * p.input +
      tokens.output * p.output +
      tokens.cacheRead * p.cacheRead +
      tokens.cacheWrite * p.cacheWrite) /
    1_000_000
  );
}
