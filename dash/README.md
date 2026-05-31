# posthook-dash

Local web dashboard for [posthook](../posthook) AI usage metrics. Renders KPIs,
breakdowns, and per-session detail straight from `~/.posthook/posthook.db`.

This package is the UI half of posthook. It has no API, no auth, and no cloud
sync — server components read the SQLite file the posthook CLI writes to.

## Install

```bash
npm install -g posthook-dash
posthook-dash             # boots Next.js, serves http://127.0.0.1:3847
```

Or without a global install:

```bash
npx posthook-dash
```

The `posthook` CLI's `posthook dash` command will spawn this binary once the
two ship together. Until then run `posthook-dash` directly.

## What you get

- **Overview (`/`)** — 6 KPI cards (AI code %, AI lines generated, AI lines
  committed, top model, working hours, max parallel agents), a Sankey funnel
  from AI-generated → committed, and breakdown bars by agent / model / repo / engineer.
- **Sessions list (`/sessions`)** — every AI session with duration, lines
  generated, edits, files touched, and commits in window. Filterable and paginated.
- **Session detail (`/sessions/<id>`)** — header KPIs, the user prompts pulled
  from the Claude transcript JSONL, commits that fell in the session's window,
  and the files that were touched.

Filters (date range, agent, model, repo, engineer) live in URL search params and
work consistently across pages.

## Environment

| Variable                  | Default                       | Purpose                                  |
| ------------------------- | ----------------------------- | ---------------------------------------- |
| `POSTHOOK_DB`             | `~/.posthook/posthook.db`     | Path to the SQLite file posthook writes. |
| `POSTHOOK_DASH_PORT`      | `3847`                        | Port to bind on.                         |
| `POSTHOOK_DASH_HOSTNAME`  | `127.0.0.1`                   | Hostname to bind on.                     |

## Develop

```bash
git clone …
cd posthook-dash
npm install
npm run dev      # next dev on :3847
```

Build the prod bundle that ships with the npm package:

```bash
npm run build    # produces .next/standalone, which bin/posthook-dash.mjs runs
```

## Stack

- Next.js 15 App Router + React 19 + TypeScript strict.
- Tailwind v4 for styling.
- `better-sqlite3` for SQLite access; server components query it directly.
- Recharts for KPI bars and breakdowns; ECharts (Sankey only) for the funnel.
- No API routes, no client-side data fetching.

## Roadmap

- `/commits/<sha>` page with diff + AI line highlights (uses `event_line_ranges`
  from posthook's schema).
- Cloud Postgres adapter for SaaS deployment — same UI, swappable data layer.
- Time-series chart of AI activity over the filtered window.
- Drill-down from commit → session → prompt → exact lines.
