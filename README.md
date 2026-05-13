# posthook

Local-first instrumentation for AI-assisted coding. Installs hooks into Claude Code, Cursor, and Codex CLI plus a git post-commit hook, captures every AI tool call and every commit, and stores it all in a single SQLite file you can query.

Built as the data-collection layer for a team-rollup product. Today it runs entirely on your laptop — no network, no account, no cloud.

## Quickstart

```bash
# 1. Build the binary (requires Bun: https://bun.sh)
bun install
bun run build

# 2. Put it on your PATH
install -m 0755 dist/posthook ~/.local/bin/posthook
# (ensure ~/.local/bin is on your PATH)

# 3. Install hooks
posthook init

# 4. Enroll any existing repos you want tracked
posthook track ~/code/my-project

# 5. After using Claude Code / Cursor / Codex for a while:
posthook metrics
```

## What gets captured

| Source | How | What |
|---|---|---|
| **Claude Code** | hooks in `~/.claude/settings.json` (`PreToolUse`, `PostToolUse`, `SessionStart`, `Stop`) | every tool call (Edit, Write, Bash, Read, ...), full payload, session start/end, tokens, model, cost |
| **Cursor** | hooks in `~/.cursor/hooks.json` (`preToolUse`, `postToolUse`, `beforeSubmitPrompt`, `afterFileEdit`) | every tool call + prompt submission |
| **Codex CLI** | inline hooks in `~/.codex/config.toml` (`PreToolUse`, `PostToolUse`, `Stop`) + `features.hooks = true` | every tool call + session end |
| **Git** | per-repo `post-commit` hook (auto-installed in new repos via `init.templateDir`, manual `posthook track` for existing repos) | commit SHA, author, parent, branch, files changed, +/- lines per file |

Token, cost, and session-duration data come from parsing Claude Code's session transcript at `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl` when the `Stop` hook fires. Cursor and Codex don't expose equivalent transcripts yet — for those agents you get tool-call events but not per-session token totals.

## Where data lives

| Path | What |
|---|---|
| `~/.posthook/posthook.db` | SQLite — all your data (`events`, `sessions`, `commits`, `commit_files`, `repositories`) |
| `~/.posthook/git-template/hooks/post-commit` | The git hook copied into every new repo by `git init` |
| `~/.claude/settings.json` | Claude Code hook entries — **merged alongside your existing hooks** |
| `~/.cursor/hooks.json` | Cursor hook entries — merged |
| `~/.codex/config.toml` | Codex hook entries — merged |
| `~/.gitconfig` (`init.templateDir`) | Points git at the template dir above |

All installers are idempotent and preserve any pre-existing user hooks. Re-running `posthook init` after a binary upgrade refreshes the embedded path without clobbering anything.

## Commands

```
posthook init                          Install hooks for detected agents + global git template
posthook track <repo-path>             Install post-commit hook in an existing repo
posthook ingest --agent <slug>         Read agent hook payload from stdin and store it
posthook ingest --kind git-commit \
                --repo-root <path> \
                --sha <sha>            Record a git commit (called by the post-commit hook)
posthook status                        Counts + recent activity
posthook metrics                       AI metrics with breakdowns by agent / model / repo
```

Environment variables:
- `POSTHOOK_BIN` — override the binary path written into hook configs (useful for dev installs)
- `POSTHOOK_DEBUG=1` — verbose stderr logging

## Metrics

`posthook metrics` shows:

- **AI edit events** — count of `PostToolUse` Edit/Write/MultiEdit calls
- **AI lines generated** — newlines in `new_string + content` summed across edits
- **AI lines committed** — lines from AI edits whose file landed in a commit within the time window between consecutive commits
- **AI Code %** — `lines_committed / total commit lines added`
- **Working hours** — wall-clock span per session (uses transcript timestamps when available, falls back to event timestamps)
- **Max concurrent sessions** — highest overlap of session time-windows
- **Top model** — most-used model across edits / sessions
- **Tokens** (in / out / cache_read / cache_write)
- **Total spend** — USD computed from tokens × `src/pricing.ts`
- **Commits captured** — total + lines added/removed

Each is broken down by **agent**, **model**, and **repo**.

## Privacy

Everything stays in `~/.posthook/posthook.db` on your machine. No HTTP calls, no telemetry, no account.

What's stored:
- **Tool-call payloads are stored verbatim** in `events.payload` (TEXT). For `Edit` calls this includes `old_string` and `new_string` — i.e. snippets of your code. For `Bash` calls it includes the command. If your environment requires redaction, the place to add it is in `src/commands/ingest.ts` (see `ingestAgentEvent`).
- File paths are stored as absolute paths plus a derived repo-relative path.
- Commit metadata (author email, message subject, file paths) is stored from your local git.

Nothing in this database is encrypted at rest. Treat it like any local git checkout.

## Architecture

- **TypeScript on Bun**, compiled to a single ~60MB binary via `bun build --compile`. No Node required on user machines.
- **SQLite via `bun:sqlite`** (built into Bun, zero deps). Schema mirrors what we'll eventually want in Postgres so the migration path is `INSERT INTO postgres SELECT * FROM sqlite` per table.
- **Per-agent installer modules** (`src/installers/`) each follow the same idempotent merge-and-deduplicate pattern: read existing config, surgically add/update only posthook's command, write atomically.
- **Stop-hook transcript parser** (`src/transcript.ts`) reads Claude Code's JSONL transcripts to extract tokens and model, then `src/pricing.ts` computes USD cost.
- **Schema migrations** run automatically on `openDb()` via `ALTER TABLE ADD COLUMN` guards plus a backfill pass that retrofits old rows.

## Limitations

Honest list of things we don't do yet:

- **Cursor and Codex don't surface token counts** — only Claude Code's transcript format is parsed. For those agents you get tool-call events but session totals stay at zero.
- **AI Code % overcounts** when AI-generated lines are rewritten by a human before commit. We sum generated-line counts of AI edits to files in the commit's window; we don't subtract subsequently-discarded lines. A character-level attribution (git-ai's `range_authorship`) would fix this but is out of scope for now.
- **No rebase / cherry-pick / amend tracking.** If you rewrite history, attributions don't follow the new SHAs.
- **Pricing is approximate.** `src/pricing.ts` is a static prefix-match table — keep it current as Anthropic/OpenAI prices shift.
- **No PR-level breakdown.** Adding `gh pr list` lookups is the next obvious step.
- **Single-user, local-only.** Multi-user / team rollups are the SaaS roadmap (see below), not in this binary.

## Roadmap

- [ ] Local web dashboard (`posthook dash`) — small Hono server, charts via Tremor
- [ ] Cloud sync (`posthook sync`) — push to a multi-tenant Postgres for team rollups
- [ ] Per-PR breakdown via `gh` / `glab` / `bb`
- [ ] Trust-hash manifest for Codex hooks (`~/.codex/hooks.json`)
- [ ] More agents: Aider, Gemini CLI, Windsurf
- [ ] Character-level attribution to fix the AI Code % overcount

## Repo layout

```
bin/posthook.ts          Entry point shim
src/
  cli.ts                 Top-level command dispatch
  store.ts               SQLite schema, migrations, backfill
  pricing.ts             Model → $/Mtok table
  transcript.ts          Claude Code transcript parser
  config.ts              Paths under ~/.posthook and ~/.<agent>
  util/
    atomic.ts            writeAtomic
    git.ts               findRepoRoot, relPathInRepo
    log.ts               Tiny stderr logger
  installers/
    base.ts              Shared helpers (JSON read/write, command marker)
    claude_code.ts       ~/.claude/settings.json
    cursor.ts            ~/.cursor/hooks.json
    codex.ts             ~/.codex/config.toml
    git_hook.ts          post-commit hook + global template
  commands/
    init.ts              Orchestrates all installers
    track.ts             Enroll a single existing repo
    ingest.ts            Read events from hooks + transcript parsing
    status.ts            Recent activity dump
    metrics.ts           Aggregations with breakdowns
install.sh               curl-pipe installer (v0 placeholder)
```

## Querying the DB directly

Anything `posthook metrics` shows is derived from SQLite. For ad-hoc queries:

```bash
sqlite3 ~/.posthook/posthook.db
```

Useful starting points:

```sql
-- recent events
SELECT ts, agent_slug, event_type,
       json_extract(payload, '$.tool_name') AS tool,
       rel_file_path
FROM events
ORDER BY ts DESC LIMIT 20;

-- sessions with totals
SELECT id, agent_slug, model_slug, started_at, ended_at,
       tokens_in, tokens_out, ROUND(cost_usd, 4) AS cost
FROM sessions ORDER BY started_at DESC;

-- which files have AI edits but no commit yet
SELECT DISTINCT rel_file_path
FROM events
WHERE event_type = 'PostToolUse'
  AND json_extract(payload, '$.tool_name') IN ('Edit','Write','MultiEdit')
  AND rel_file_path NOT IN (SELECT file_path FROM commit_files);
```

## Internal

Source of truth lives at `/Users/samuelakinwunmi/Desktop/Bilanc/posthook`. Bug reports and PRs welcome via whichever channel we end up settling on.
