# posthook

Local-first instrumentation for AI-assisted coding. Installs hooks into Claude Code, Cursor, and Codex CLI, installs a lightweight `git` shadow that intercepts every git command across every repo on your machine, captures both sides into a single SQLite file you can query, and ties every line of code to the AI session that wrote it via `posthook blame`.

Built as the data-collection layer for a team-rollup product. Today it runs entirely on your laptop — no network, no account, no cloud. Attribution data also lives in `refs/notes/posthook` inside each repo, so a teammate who clones the repo can run `posthook blame` and see who wrote what without any account or sync.

## Quickstart

```bash
# 1. Build the binary (requires Bun: https://bun.sh)
bun install
bun run build

# 2. Put it on your PATH
install -m 0755 dist/posthook ~/.local/bin/posthook
# Make sure ~/.local/bin precedes the real git on PATH (usually /usr/bin):
#   export PATH="$HOME/.local/bin:$PATH"

# 3. Install hooks + the git shadow
posthook init

# 4. After using Claude Code / Cursor / Codex for a while:
posthook status
posthook metrics
posthook blame path/to/file.ts
```

The `init` step installs (a) hook entries in each detected AI agent's global config and (b) a `git` symlink alongside the posthook binary that intercepts every git command. No per-repo setup needed — clones, new repos, and existing repos all just work.

If you can't have `~/.local/bin` precede real git on PATH (e.g. locked-down corporate machine), `init` also writes a global git template + `init.templateDir` as a fallback. New repos created with `git init` will still be tracked. For existing repos in that mode, run `posthook track <path>`.

## What gets captured

| Source | How | What |
|---|---|---|
| **Claude Code** | hooks in `~/.claude/settings.json` (`PreToolUse`, `PostToolUse`, `SessionStart`, `Stop`) | every tool call (Edit, Write, Bash, Read, …), full payload, session start/end, model |
| **Cursor** | hooks in `~/.cursor/hooks.json` (`preToolUse`, `postToolUse`, `beforeSubmitPrompt`, `afterFileEdit`) | every tool call + prompt submission |
| **Codex CLI** | inline hooks in `~/.codex/config.toml` (`PreToolUse`, `PostToolUse`, `Stop`) + `features.hooks = true` | every tool call + session end |
| **Git (shadow)** | `~/.local/bin/git` symlink → posthook binary, intercepts every git command on every repo | every successful `git commit` and `git clone` is captured. Other git commands pass through with zero overhead. Line-attribution metadata is written to `refs/notes/posthook`. |
| **Git (fallback)** | per-repo `post-commit` hook (auto-installed in new repos via `init.templateDir`, manual `posthook track` for existing repos) | same as above. Coexists safely with the shadow — commit ingest is idempotent via `UNIQUE(repo_id, sha)`. |

For Claude Code, `Stop` triggers a transcript parse at `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl` to backfill model and session timestamps. For all three agents, when a `PostToolUse` event arrives for `Edit`/`Write`/`MultiEdit`, posthook reads the post-edit file and records the line range that the AI wrote.

### How the git shadow works

The shadow is a single symlink. When you run `git status`, the shell resolves `git` via PATH to `~/.local/bin/git` (our symlink), which executes the posthook binary. The binary inspects `process.argv0` — if it sees `git`, it runs the real git binary as a child process with all the original args, forwards stdio and signals faithfully, and exits with git's exit code. For successful `commit` and `clone` subcommands it also runs our capture logic. For everything else, it's pure passthrough.

Internally posthook saves the location of the real git binary to `~/.posthook/git-path` at install time. Our own ingest path sets `POSTHOOK_BYPASS=1` when shelling out to git, so internal queries (`git log`, `git rev-parse`, `git notes`) don't recurse through the proxy.

## Where data lives

| Path | What |
|---|---|
| `~/.posthook/posthook.db` | SQLite — all your data (`events`, `event_line_ranges`, `sessions`, `commits`, `commit_files`, `repositories`) |
| `~/.posthook/git-path` | Absolute path to the real git binary. Written at install-shadow time, read on every shadow invocation. |
| `~/.local/bin/git` | The shadow symlink → `~/.local/bin/posthook`. Created by `posthook install-shadow`. |
| `~/.posthook/git-template/hooks/post-commit` | Fallback hook copied into every new repo by `git init` |
| `~/.claude/settings.json` | Claude Code hook entries — **merged alongside your existing hooks** |
| `~/.cursor/hooks.json` | Cursor hook entries — merged |
| `~/.codex/config.toml` | Codex hook entries — merged |
| `~/.gitconfig` (`init.templateDir`) | Points git at the template dir above |
| Each tracked repo's `refs/notes/posthook` | Line-attribution metadata (no prompt text). Auto-pushed/fetched on `origin` so blame works after clone. |

All installers are idempotent and preserve any pre-existing user hooks. Re-running `posthook init` after a binary upgrade refreshes the embedded path without clobbering anything.

## Commands

### `posthook init`

Install hooks for every detected AI agent, install the git shadow, and set up the global git template as a fallback. Run this once per machine after building. Re-run safely after upgrading the binary.

```
posthook init
```

Output:
```
Using binary path: /Users/sam/.local/bin/posthook

AI agent hooks:
  Claude Code: hooks merged into ~/.claude/settings.json
  Cursor: hooks merged into ~/.cursor/hooks.json
  Codex CLI: hooks added to ~/.codex/config.toml

Git shadow:
  Shadow installed: /Users/sam/.local/bin/git → /Users/sam/.local/bin/posthook
  Real git saved:    /usr/bin/git
  PATH check:        `which git` → /Users/sam/.local/bin/git ✓

Git template (fallback for new repos):
  Git template: hook installed at ~/.posthook/git-template/hooks/post-commit; init.templateDir set

Done. Track existing repos with: posthook track <path>
Verify with:                     posthook status
```

### `posthook install-shadow`

Install just the `git` shadow symlink. Called automatically by `posthook init` — exposed separately for re-running after PATH changes or fixing a misconfigured install. Idempotent.

```
posthook install-shadow
```

Critical: the shadow only intercepts git if it wins PATH lookup. The command verifies via `which git` and warns loudly if PATH ordering puts a different git first. Fix with:
```bash
export PATH="$HOME/.local/bin:$PATH"
```
in your shell rc, open a new shell, and re-run `posthook install-shadow` to confirm.

### `posthook uninstall-shadow`

Remove the shadow symlink, restore the user's plain git. Safe — refuses to delete the symlink if it's not pointing at the posthook binary, so you can't accidentally damage another wrapper that happened to install at the same path.

```
posthook uninstall-shadow
```

The saved `~/.posthook/git-path` is preserved (in case you re-install later) but you can delete it manually.

### `posthook track <repo-path>`

Install the per-repo `post-commit` hook in a single existing repo. Only needed as a fallback when the shadow isn't usable (locked-down PATH, etc.) or for safety belt-and-braces if you want a hook captured even when the shadow is removed. Coexists safely with the shadow — commits are deduplicated.

```
posthook track ~/code/my-existing-project
```

### `posthook status`

First thing to run to confirm everything's wired up. Shows row counts, git shadow health, hook misfires, events by agent, and recent commits. Run this anytime to spot-check the pipeline — most setup mistakes show up loudly here.

```
posthook — local store at ~/.posthook/posthook.db
  events:       492
  sessions:     5
  commits:      5
  repositories: 5

Git shadow:
  active   /Users/sam/.local/bin/git
  real git /usr/bin/git

Events by agent:
  claude-code      492

Recent commits:
  2026-05-13 04:09  e9c06f1  posthook             +187/-0 (1 files)
  ...
```

What `posthook status` will catch automatically:

- **Shadow installed but not winning PATH** — a different `git` runs first, so the shadow captures nothing. Status prints the exact `export PATH=...` line to add to your shell rc.
- **Shadow symlink missing** — someone deleted `~/.local/bin/git`. Status tells you to run `posthook install-shadow`.
- **Shadow symlink corrupted** (points somewhere other than the posthook binary). Status tells you to `uninstall-shadow` then re-install.
- **Hook misfires** — any agent hook that fired with no stdin payload is recorded as a `hook_misfire` event. Status shows the per-agent count under "Hook health" so you know your agent hook configs are sound.

Example of a problematic status (shadow installed, PATH mis-ordered):

```
Git shadow: ⚠ NOT INTERCEPTING
  Symlink is in place at /Users/sam/.local/bin/git
  but `which git` returns /opt/homebrew/bin/git — PATH order means our shadow is bypassed.
  Fix: add to your shell rc and open a new shell:
    export PATH="/Users/sam/.local/bin:$PATH"
  Then verify with: which git
  While unfixed, git commits captured only via per-repo hooks (templateDir + posthook track).
```

### `posthook metrics`

Aggregated AI metrics with breakdowns by agent, model, and repo. Use this when you want a snapshot of how much you're using AI and where.

```
posthook metrics

Overall
  AI edit events            43
  AI lines generated        2082  (raw new_string + content)
  AI lines replaced         330
  AI lines committed*       148
  AI code %*                7.7%
  Working hours             6.32
  Distinct sessions         3
  Top model                 claude-opus-4-7 (43 edits)
  Commits captured          5  (+1917 / -0 lines)

By agent  / By model  / By repo  — same columns, grouped
```

Pricing and per-session token totals were intentionally removed in v0.4 (see *Design notes* below).

### `posthook blame <file>`

The headline feature. Per-line attribution: shows which AI session wrote each line, with the user prompt that triggered it.

```
posthook blame src/foo.ts

  1  human · sam                  function hello() {
  2  human · sam                    return "world";
  3  human · sam                  }
     ┌─ add a compute function that doubles its input
  4  AI claude-opus-4-7 14:32 ab8d12c0
  5  AI claude-opus-4-7 14:32 ab8d12c0  function compute(x: number): number {
  6  AI claude-opus-4-7 14:32 ab8d12c0    return x * 2;
  7  AI claude-opus-4-7 14:32 ab8d12c0  }

  4/7 lines AI-authored (57.1%)
```

How it works: posthook runs `git blame --porcelain` on the file, joins each line against `event_line_ranges` for the same repo + relative path, scoped to the commit window each event landed in. The prompt header is pulled from the Claude Code transcript JSONL for the originating session.

Falls back to `refs/notes/posthook` for any commit whose AI ranges aren't in local SQLite — so blame still works for a teammate who clones the repo without ever running posthook themselves. The note carries attribution but not prompt content; the prompt header is only available on the machine that originally captured the session.

### `posthook inspect [--agent X] [--type Y] [--session Z] [--since ISO] [--limit N]`

Print recent event payloads verbatim, pretty-printed as JSON. The main tool for verifying what providers are actually sending you and debugging accuracy issues. Default limit is 10.

```
posthook inspect --agent claude-code --type PostToolUse --limit 1
```

Output (one block per event, separated by `---`):
```json
{
  "ts": "2026-05-13T23:07:33.626Z",
  "agent": "claude-code",
  "event_type": "PostToolUse",
  "session_id": "f106e3c0-2649-415f-ae94-9492f6ca629e",
  "cwd": "/Users/sam/code/foo",
  "file_path": "/Users/sam/code/foo/src/bar.ts",
  "rel_file_path": "src/bar.ts",
  "payload": {
    "tool_name": "Edit",
    "tool_input": {
      "file_path": "/Users/sam/code/foo/src/bar.ts",
      "old_string": "...",
      "new_string": "..."
    },
    "tool_response": { ... }
  }
}
---
```

Filters compose. To dig into hook misfires: `posthook inspect --type hook_misfire`. To audit one session's full timeline: `posthook inspect --session <id> --limit 200`.

### `posthook dash`

Stub today — prints a "coming soon" pointer. The web dashboard ships as a separate npm package, `posthook-dash`, which reads the same SQLite file directly. Once `posthook-dash` is published, this command will spawn it (cloud-authed users will be routed to `app.bilanc.co` instead). In the meantime, install `posthook-dash` from its own directory and run `posthook-dash` directly. See [`posthook-dash/README.md`](../posthook-dash/README.md).

### `posthook ingest …`

Called by hooks, not by you directly. Listed here for completeness.

```
posthook ingest --agent <slug>                                    # reads JSON from stdin
posthook ingest --kind git-commit --repo-root <path> --sha <sha>  # called by post-commit hook
```

### Environment variables

- `POSTHOOK_BIN` — override the binary path written into hook configs (useful for dev installs).
- `POSTHOOK_DEBUG=1` — verbose stderr logging from every command. Helpful when investigating why a payload didn't capture as expected.
- `POSTHOOK_BYPASS=1` — when set, the git shadow passes straight through to real git without running capture logic. Used internally to prevent recursion when our ingest path shells out to git; useful manually to temporarily run a command under "plain git" semantics without uninstalling the shadow.

## Verifying your data as you go

A practical sequence for confirming things are working, and what to do when they're not.

### Right after `posthook init`

```bash
# 1. Confirm hook configs were merged.
posthook init   # idempotent — should say "already up to date" on second run

# 2. Run status — this is your one-shot health check. Captures shadow health,
#    PATH ordering, misfires, and DB initialization in one command.
posthook status
# expect: "Git shadow: active …" section. If you see "⚠ NOT INTERCEPTING",
# follow the fix command it prints exactly.

# 3. Confirm the shadow is a pure passthrough.
git --version
# expect: identical to running real git directly

# 4. Eyeball the actual hook config to make sure your existing hooks weren't clobbered.
cat ~/.claude/settings.json | jq .hooks
```

### After using AI for a while

```bash
# 1. Are events flowing?
posthook status
# expect: events count > 0, events-by-agent showing claude-code/cursor/codex

# 2. What's actually in the payloads?
posthook inspect --limit 3
# scan for unexpected fields, missing tool_input, etc.

# 3. Is per-line attribution being captured?
sqlite3 ~/.posthook/posthook.db \
  "SELECT COUNT(*) AS ranges FROM event_line_ranges;"
# expect: > 0 once you've had AI edits since installing this version

# 4. Did any hook fire without a payload?
posthook inspect --type hook_misfire --limit 5
# if any: check the corresponding agent's hook config and the binary path
```

### After committing

```bash
# 1. Did the commit get captured?
posthook status   # check "Recent commits"

# 2. Did line attribution land in the Git Note?
git notes --ref=refs/notes/posthook list
# expect: one entry per commit that touched AI-written code

git notes --ref=refs/notes/posthook show HEAD | jq .
# expect: { v: 1, commit: "...", files: { "path/to/file": [{ lines, agent, session, model, ts }] } }

# 3. Run the killer feature.
posthook blame path/to/file.ts
# expect: AI lines tagged with model + session, human lines tagged with git author
```

### When something looks off

```bash
# Run with debug logging to see what each step is doing.
POSTHOOK_DEBUG=1 posthook status
POSTHOOK_DEBUG=1 posthook blame foo.ts

# Verify the shadow is actually intercepting git.
POSTHOOK_DEBUG=1 git --version 2>&1 | head -1
# expect: "[posthook] ..." debug lines, then real git version

# Temporarily bypass the shadow (e.g. to compare behavior with plain git).
POSTHOOK_BYPASS=1 git commit -m "skipping posthook capture for this one"

# See an event's raw payload to confirm what the provider actually sent.
posthook inspect --session <session-id> --limit 100

# Drop into SQL for ad-hoc queries — see "Querying the DB directly" below.
sqlite3 ~/.posthook/posthook.db
```

If the shadow seems broken (e.g. `git --version` errors), uninstall and reinstall:
```bash
posthook uninstall-shadow
posthook install-shadow
```
If that still fails, see `~/.posthook/git-path` for what posthook thinks real git is, and verify that path exists and is executable.

## Querying the DB directly

Useful starting points:

```sql
-- recent events
SELECT ts, agent_slug, event_type,
       json_extract(payload, '$.tool_name') AS tool,
       rel_file_path
FROM events
WHERE event_type != 'hook_misfire'
ORDER BY ts DESC LIMIT 20;

-- all AI line ranges for a file
SELECT elr.start_line, elr.end_line, e.ts, e.session_id, s.model_slug
FROM event_line_ranges elr
JOIN events e ON e.id = elr.event_id
LEFT JOIN sessions s ON s.id = e.session_id
WHERE elr.rel_file_path = 'src/foo.ts'
ORDER BY e.ts;

-- which files have AI edits but no commit yet
SELECT DISTINCT elr.rel_file_path
FROM event_line_ranges elr
JOIN events e ON e.id = elr.event_id
WHERE elr.rel_file_path NOT IN (SELECT file_path FROM commit_files);

-- top sessions by line-range count
SELECT e.session_id, s.model_slug,
       COUNT(*) AS ranges,
       SUM(elr.new_text_lines) AS lines_written
FROM event_line_ranges elr
JOIN events e ON e.id = elr.event_id
LEFT JOIN sessions s ON s.id = e.session_id
GROUP BY e.session_id
ORDER BY lines_written DESC LIMIT 10;
```

## Privacy

Everything stays in `~/.posthook/posthook.db` on your machine. No HTTP calls, no telemetry, no account.

What's stored:
- **Tool-call payloads are stored verbatim** in `events.payload` (TEXT). For `Edit` calls this includes `old_string` and `new_string` — i.e. snippets of your code. For `Bash` calls it includes the command. If your environment requires redaction, the place to add it is in `src/commands/ingest.ts` (see `ingestAgentEvent`).
- Absolute file paths, plus a derived repo-relative path.
- Commit metadata (author email, message subject, file paths) from your local git.

**What's in `refs/notes/posthook` and pushed to remotes:**
- Line ranges, agent slug, session ID, model name, event timestamp. No prompt text. No code snippets.

If your repo has anyone with read access who shouldn't see attribution metadata, remove the auto-configured push refspec from `.git/config`:
```
git config --unset-all remote.origin.push refs/notes/posthook:refs/notes/posthook
```

Nothing in the local database is encrypted at rest. Treat it like any local git checkout.

## Design notes

**Why no token counts or pricing.** Earlier versions tracked tokens and computed USD cost from a static price table. Tokens were only available for Claude Code (transcript parse) and zero for Cursor/Codex, so cross-agent comparisons were misleading. Pricing drifted silently as providers changed rates. Both were dropped in v0.4. Token data still lives in the on-disk transcript JSONL — anything that needs them can re-parse from there.

**Why line attribution lives in two places.** SQLite is the source of truth: rich, joinable, fast. Git Notes (`refs/notes/posthook`) carry a compact derivative — line ranges plus session/agent/model metadata — so blame still works for someone who clones the repo without local capture data. Prompt content is deliberately kept out of Notes; that's the future cloud product's value-add and avoids leaking conversation context through git remotes.

**Why attribution is line-level, not character-level.** Character-level attribution (git-ai's approach) is more accurate but ~5k lines of Rust to implement well. Line ranges captured via string-matching against the post-edit file content cover the practical blame use case. Sub-line attribution is a Phase 3 question.

## Architecture

- **TypeScript on Bun**, compiled to a single ~60MB binary via `bun build --compile`. No Node required on user machines.
- **SQLite via `bun:sqlite`** (built into Bun, zero deps). Schema mirrors what we'll eventually want in Postgres so the migration path is `INSERT INTO postgres SELECT * FROM sqlite` per table.
- **Per-agent installer modules** (`src/installers/`) follow the same idempotent merge-and-deduplicate pattern: read existing config, surgically add/update only posthook's command, write atomically.
- **Line-range extractor** (`src/lineRanges.ts`) takes the post-edit file content and the tool input, returns the line range(s) the AI wrote. Pure function, testable in isolation.
- **Stop-hook transcript parser** (`src/transcript.ts`) reads Claude Code JSONL transcripts for model + session-span backfill, and for prompt-lookup during `posthook blame`.
- **Schema migrations** run automatically on `openDb()` via `ALTER TABLE` guards, backfill passes for repo IDs and session models, and `datetime()`-wrapped time comparisons for timezone-correct windowing.

## Limitations

Honest list of things we don't do yet:

- **Bash-driven edits aren't line-attributed.** Only `Edit`, `Write`, and `MultiEdit` tool calls produce line ranges. If an AI runs `sed` via `Bash`, those edits show up as `human` in blame. Fixing this needs pre/post file snapshots, deferred to Phase 3.
- **The shadow only intercepts `commit` and `clone`.** Other history-changing commands (rebase, cherry-pick, amend, reset, push, fetch) pass through without our hooks running. Line ranges still reference the original commit window, and Git Notes stay attached to the original SHAs.
- **Windows isn't supported yet.** The shadow uses Unix symlinks and POSIX signal numbers. Windows would need a `.cmd` wrapper and a different argv[0] strategy. Coming in a later release.
- **MultiEdit with identical replacement strings** attributes both to the first match. Edge case — uncommon and easy to live with.
- **Single-user, local-only.** Multi-user / team rollups are the SaaS roadmap, not in this binary. The schema is multi-tenant-ready (`org_id` column exists).
- **Engineer identity is captured from git.** At session creation, posthook reads `git config user.email` and `user.name` from the active repo and stores them on the `sessions` row so the dashboard can break down metrics by engineer. Local-only today; will sync to the SaaS once cloud sync ships.

## Roadmap

- [ ] Wire `posthook dash` to spawn `posthook-dash` — the Next.js dashboard already exists in `../posthook-dash` and reads `~/.posthook/posthook.db` directly; the CLI command is a stub today
- [ ] Cloud dashboard — same `posthook-dash` codebase with a Postgres adapter, hosted at app.bilanc.co
- [ ] Cloud sync (`posthook sync`) — push to a multi-tenant Postgres for team rollups; prompt content lives here as the SaaS value-add
- [ ] Per-PR breakdown via `gh` / `glab` / `bb`
- [ ] Trust-hash manifest for Codex hooks (`~/.codex/hooks.json`)
- [ ] More agents: Aider, Gemini CLI, Windsurf
- [ ] Bash-driven edit attribution via pre/post file snapshots
- [ ] Rebase / cherry-pick note migration via shadow pre/post hooks on those subcommands
- [ ] Windows support for the git shadow

## Repo layout

```
bin/posthook.ts          Entry point + argv[0] dispatch (git shadow vs posthook CLI)
src/
  cli.ts                 Top-level posthook command dispatch
  store.ts               SQLite schema, migrations, backfill
  lineRanges.ts          Pure line-range extractor for Edit/Write/MultiEdit
  transcript.ts          Claude Code transcript parser + prompt extraction
  config.ts              Paths under ~/.posthook and ~/.<agent>, notes ref
  util/
    atomic.ts            writeAtomic
    git.ts               findRepoRoot, relPathInRepo, canonicalize, gitBypassEnv
    log.ts               Tiny stderr logger
  proxy/
    index.ts             Git shadow entry: spawn real git, forward stdio/signals,
                         intercept commit + clone for capture
    realGit.ts           Detect + save the real git binary path, skipping shadows
  installers/
    base.ts              Shared helpers (JSON read/write, command marker)
    claude_code.ts       ~/.claude/settings.json
    cursor.ts            ~/.cursor/hooks.json
    codex.ts             ~/.codex/config.toml
    git_hook.ts          per-repo post-commit hook + global template + notes transport
  commands/
    init.ts              Orchestrates all installers + shadow
    installShadow.ts     install-shadow + uninstall-shadow
    track.ts             Enroll a single existing repo (fallback path)
    ingest.ts            Read events from hooks, capture line ranges, write notes
    status.ts            Counts + hook health + recent commits
    metrics.ts           Aggregations with breakdowns
    inspect.ts           Pretty-print raw payloads
    blame.ts             Per-line attribution with prompt headers
    dash.ts              Stub for `posthook dash` — pointer to posthook-dash
install.sh               curl-pipe installer (v0 placeholder)
```

## Internal

Source of truth lives at `/Users/samuelakinwunmi/Desktop/Bilanc/posthook`. Bug reports and PRs welcome via whichever channel we end up settling on.
