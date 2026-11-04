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

- **Overview (`/`)** — what your agents shipped this period and how the work
  was spread out: AI lines committed (hero), five KPI tiles with deltas against
  the previous window and sparklines, active time per day by agent, a
  weekday × hour rhythm heatmap, an outcome funnel (sessions started → edited →
  shipped; lines generated → committed) and where agent time goes (model vs
  shell vs MCP vs subagents, plus the slowest calls).
- **Agents (`/agents`)** — one table per agent + model with the efficiency
  ratios side by side (keep rate, lines per 1K output tokens, tool calls per
  edit, re-edit rate, shipped sessions) next to the counts they come from, a
  generated → committed dumbbell and the tool mix per agent.
- **Sessions (`/sessions`)** — every session with a title (its first
  prompt), an activity strip, an outcome chip (Shipped / Edited / Explored),
  prompts, active time and lines generated → committed. Grouped by day;
  search covers prompt text, branch and repo; filterable by outcome.
- **Session (`/sessions/<id>`)** — the session as it happened: a timeline
  with idle gaps compressed, a numbered turn rail, the conversation with tool
  calls folded under each prompt, commits inline where they happened, a files
  panel (generated vs committed per file) and output tokens per turn.

Filters (date range, agent, model, repo, outcome) live in URL search params and
apply to every chart on a page.

### How the numbers are defined

- **Active time** is the sum of gaps between consecutive events in a session,
  each gap capped at `POSTHOOK_DASH_IDLE_MINUTES` (default 5). Wall-clock
  session span counts idle hours and double-counts parallel sessions, so it is
  shown only on the session page, next to active time.
- **A turn** is everything between two Stop events: the prompt, the tool calls
  it caused, the edits, the agent's closing message. Prompts are bucketed into
  turns by timestamp.
- **Outcome**: *Shipped* = at least one commit has AI lines attributed to the
  session (a 0-line attribution row, which posthook writes when a recount finds
  nothing, does not count); *Edited* = the session edited a file but nothing
  is committed yet; *Explored* = no file edits.
- **Committed lines** are counted on the commit's date, so a commit made the
  morning after the edits lands in the day and window it was made in.
- **Keep rate** = AI lines committed ÷ AI lines generated. Lines written through
  shell commands (heredocs) are counted from the line ranges posthook located
  in the file, so a file can show more committed than generated lines.
- Tool durations come from Claude Code (`duration_ms`) and Cursor; Codex
  reports none, so its time in tools is shown as unknown rather than zero. In
  "Where agent time goes" each call's wait is capped at the idle threshold,
  the same cap active time applies, so the parts add up to active time.
  Token usage comes from Claude Code and Codex transcripts; Cursor reports none.

## Environment

| Variable                  | Default                       | Purpose                                  |
| ------------------------- | ----------------------------- | ---------------------------------------- |
| `POSTHOOK_DB`             | `~/.posthook/posthook.db`     | Path to the SQLite file posthook writes. |
| `POSTHOOK_DASH_PORT`      | `3847`                        | Port to bind on.                         |
| `POSTHOOK_DASH_HOSTNAME`  | `127.0.0.1`                   | Hostname to bind on.                     |
| `POSTHOOK_DASH_IDLE_MINUTES` | `5`                        | Gap cap for active time (see above).     |

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
- `node:sqlite` (`DatabaseSync`, Node >=24) for SQLite access via `src/lib/db.ts`; server components query it directly.
- Charts are inline SVG rendered on the server; no charting library.
- No API routes, no client-side data fetching.

## Roadmap

- `/commits/<sha>` page with the diff and AI lines highlighted (uses
  `event_line_ranges`), linked from the session page's commit cards and files
  panel.
- Link resumed sessions to their parent once the SessionStart hook reports
  `source = resume`.
- Approximate Codex tool durations from PreToolUse → PostToolUse pairs.
