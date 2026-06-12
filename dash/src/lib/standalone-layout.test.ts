import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { resolve } from "node:path";

// Regression: `next build` with output:"standalone" does NOT copy .next/static
// into .next/standalone — the standalone server serves /_next/static/* only
// from inside its own folder. If the copy is missing, every chunk 404s, React
// never hydrates, and the client-only charts (ECharts Sankey, Recharts bars)
// render blank while server-rendered KPIs/tables still show. The build script
// must perform the copy. Skipped on a fresh checkout with no build output.
const root = resolve(import.meta.dirname, "../..");
const standalone = resolve(root, ".next/standalone");

test(
  "standalone build bundles static assets next to server.js",
  { skip: !existsSync(standalone) },
  () => {
    assert.ok(
      existsSync(resolve(standalone, ".next/static")),
      ".next/standalone/.next/static is missing — the dashboard's JS chunks " +
        "will 404 and charts won't paint. `npm run build` must copy " +
        ".next/static into the standalone bundle.",
    );
  },
);
