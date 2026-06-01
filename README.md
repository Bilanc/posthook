<div align="center">

# posthook

**Local-first instrumentation for AI-assisted coding.**

Posthook tells you which lines of your codebase were written by an AI agent, by which model, from which session — and ties all of it back to the git commits that shipped it.

</div>

---

Posthook installs hooks into the AI coding tools you already use (Claude Code, Cursor, Codex CLI) and a lightweight `git` shadow that sits in front of every git command on your machine. It captures both sides — what the AI did, and what you committed — into a single SQLite file you can query, browse in a web dashboard, or attribute line-by-line with `posthook blame`.

Everything runs on your laptop by default. No account, no network calls, no telemetry. The data is yours, in a file you can open with any SQLite client.

```
AI agents ──────hooks──┐
                       ├──▶  ~/.posthook/posthook.db  ──▶  blame · metrics · dashboard
git commands ──shadow──┘
```

## Why posthook

As more of your code is written by AI, the basic questions get hard to answer:

- **How much of this codebase is AI-written, and by which models?**
- **Who reviewed the AI's work before it shipped — and which commits are pure AI vs. human-edited?**
- **When something breaks, which AI session produced the line?**

Posthook answers these without changing your workflow. You keep using your editor and your agents exactly as before; posthook records the trail in the background and links it to your git history. Attribution travels with the repo (in git notes), so a teammate who clones it can run `posthook blame` and see the same answers — no setup, no sync.

## Quickstart

```bash
# 1. Install the latest release (prebuilt CLI + dashboard) into ~/.local/bin.
#    No Go or Node needed to install — it downloads a static binary.
curl -fsSL https://raw.githubusercontent.com/Bilanc/posthook/main/install.sh | sh

# 2. Make sure ~/.local/bin precedes the real git on your PATH (usually /usr/bin):
export PATH="$HOME/.local/bin:$PATH"

# 3. Install hooks + the git shadow (idempotent; safe to re-run):
posthook init

# 4. After using Claude Code / Cursor / Codex for a while:
posthook status                 # is everything wired up and capturing?
posthook metrics                # AI vs. human, by agent/model/repo
posthook blame path/to/file.ts  # per-line attribution
posthook dash                   # open the web dashboard (needs Node >=24)
```

`posthook init` installs (a) hook entries in each detected agent's global config and (b) a `git` symlink alongside the posthook binary that intercepts every git command. There's no per-repo setup — clones, new repos, and existing repos all just work.

**Locked-down machine?** If you can't put `~/.local/bin` ahead of the real git on `PATH`, `init` also writes a global git template (`init.templateDir`) as a fallback. New repos created with `git init` are still tracked; for existing repos in that mode, run `posthook track <path>`.

### Requirements

- **macOS or Linux.** (Windows isn't supported yet — see [Limitations](#limitations).) The installer downloads a prebuilt, statically-linked binary — no toolchain required.
- **Node.js 24+** (optional) to run `posthook dash`. The dashboard reads your SQLite file through Node's built-in `node:sqlite`. Without Node, every CLI command still works — you just won't have the web dashboard.
- Building from source (contributors only) needs **Go 1.23+** — see [Building from source](#building-from-source).

## What gets captured

| Source | How | What |
|---|---|---|
| **Claude Code** | hooks in `~/.claude/settings.json` (`PreToolUse`, `PostToolUse`, `SessionStart`, `Stop`) | every tool call (Edit, Write, Bash, Read, …), full payload, session start/end, model |
| **Cursor** | hooks in `~/.cursor/hooks.json` (`preToolUse`, `postToolUse`, `beforeSubmitPrompt`, `afterFileEdit`) | every tool call + prompt submission |
| **Codex CLI** | inline hooks in `~/.codex/config.toml` (`PreToolUse`, `PostToolUse`, `Stop`) + `features.hooks = true` | every tool call + session end |
| **Git (shadow)** | `~/.local/bin/git` symlink → posthook binary, intercepts every git command on every repo | every successful `git commit` and `git clone`. All other git commands pass through with zero overhead. Line-attribution metadata is written to `refs/notes/posthook`. |
| **Git (fallback)** | per-repo `post-commit` hook (auto-installed in new repos via `init.templateDir`, manual `posthook track` for existing repos) | same as the shadow. Coexists safely — commit ingest is idempotent via `UNIQUE(repo_id, sha)`. |

For Claude Code, the `Stop` hook parses the session transcript at `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl` to backfill the model name and session timespan. For all three agents, when a `PostToolUse` event arrives for an `Edit`/`Write`/`MultiEdit`, posthook reads the post-edit file and records the exact line range the AI wrote.

### How attribution works

Posthook links AI work to commits at the **file level**: when a commit lands, each touched file is attributed to the AI session(s) that most recently edited it before that commit. From there it rolls up:

- **`event_line_ranges`** — the raw line ranges each AI edit produced.
- **`commit_sessions`** — for each commit, which sessions contributed, with event counts, files touched, and lines attributed.
- **`commit_session_files`** — the same, broken down per file.

This is what powers the "AI lines committed" and "AI code %" figures in `posthook metrics`, the per-line headers in `posthook blame`, and the breakdowns in the dashboard.

### How the git shadow works

The shadow is a single symlink. When you run `git status`, the shell resolves `git` via `PATH` to `~/.local/bin/git` (our symlink), which executes the posthook binary. The binary inspects `os.Args[0]` — seeing `git`, it runs the real git binary as a child process with the original args, forwards stdio and signals faithfully, and exits with git's exit code. For successful `commit` and `clone` subcommands it also runs the capture logic. Everything else is pure passthrough.

Posthook records the path to the real git binary in `~/.posthook/git-path` at install time. Its own ingest path sets `POSTHOOK_BYPASS=1` when shelling out to git, so internal queries (`git log`, `git rev-parse`, `git notes`) never recurse through the proxy.

## Commands

- `posthook init` — install hooks + shadow + template fallback, then auto-start the web dashboard in the background. Idempotent. (Set `POSTHOOK_DASH_AUTOSTART=0` to skip the dashboard; it's also skipped silently if the dashboard bundle or Node >=24 isn't present.)
- `posthook status` — counts, shadow health, hook misfires, recent commits. **Run this first** to confirm everything is wired up.
- `posthook metrics` — AI metrics with breakdowns by agent, model, and repo: edit events, lines generated/replaced/committed, AI code %, working hours, distinct and max-concurrent sessions.
- `posthook blame <file>` — per-line attribution with prompt headers.
- `posthook inspect [--agent X] [--type Y] [--session Z] [--since ISO] [--limit N]` — raw event payloads.
- `posthook dash` — open the local web dashboard. Starts the bundled server (reading `~/.posthook/posthook.db`) if it isn't running, waits for it, then opens your browser. `--no-open` starts it headless; `--stop` shuts it down. Binds `127.0.0.1:3847` by default.
- `posthook sync` — flush local rows to a cloud endpoint (see [Team / cloud](#team--cloud)). `--loop` flushes continuously, `--status` shows pending counts and last-flush state, `--set-endpoint/--set-token/--set-enabled` write `~/.posthook/config.json`.
- `posthook service install|uninstall|status` — manage a background daemon (launchd on macOS, systemd `--user` on Linux) that runs `posthook sync --loop` so a connected machine keeps flushing across reboots. The team installer sets this up automatically.
- `posthook track <repo-path>` — install the fallback per-repo `post-commit` hook for an existing repo.
- `posthook install-shadow` / `posthook uninstall-shadow` — manage the `git` symlink directly.
- `posthook ingest …` — read an agent or git payload from stdin and record it. Called by the hooks; you won't run it by hand.

## The dashboard

`posthook dash` serves a local web UI over your SQLite file. The overview gives you AI-edit totals, lines generated, and working-hours estimates, with breakdowns **by agent, by model, by repo, and by engineer**, plus a drill-down into individual sessions and the commits they produced. It's read-only and bound to localhost.

## Where data lives

| Path | What |
|---|---|
| `~/.posthook/posthook.db` | SQLite — all your data (`repositories`, `sessions`, `events`, `event_line_ranges`, `commits`, `commit_files`, `commit_sessions`, `commit_session_files`) |
| `~/.posthook/config.json` | Cloud-sync settings (endpoint, install token, enabled flag, flush interval). Absent until you configure sync. |
| `~/.posthook/git-path` | Absolute path to the real git binary. Written at install time, read on every shadow invocation. |
| `~/.posthook/dash/` | Staged dashboard build, spawned by `posthook dash`. |
| `~/.posthook/dash.pid` / `dash.log` | PID + logs of the background dashboard server. |
| `~/.local/bin/git` | The shadow symlink → `~/.local/bin/posthook`. |
| `~/.posthook/git-template/hooks/post-commit` | Fallback hook copied into every new repo by `git init`. |
| `~/.claude/settings.json` · `~/.cursor/hooks.json` · `~/.codex/config.toml` | Agent hook entries — **merged alongside your existing hooks**, never clobbered. |
| `~/.gitconfig` (`init.templateDir`) | Points git at the template dir above. |
| Each tracked repo's `refs/notes/posthook` | Line-attribution metadata (no prompt text). Auto-pushed/fetched on `origin` so blame works after a clone. |

All installers are idempotent and preserve pre-existing user hooks. Re-running `posthook init` after a binary upgrade refreshes the embedded path without disturbing anything else.

### Environment variables

- `POSTHOOK_BIN` — override the binary path written into hook configs (useful for dev installs).
- `POSTHOOK_DEBUG=1` — verbose stderr logging from every command.
- `POSTHOOK_BYPASS=1` — make the git shadow pass straight through. Used internally to prevent recursion; useful manually to run a one-off command under plain-git semantics without uninstalling the shadow.
- `POSTHOOK_DB` — override the SQLite path the dashboard reads (default `~/.posthook/posthook.db`).
- `POSTHOOK_DASH_PORT` / `POSTHOOK_DASH_HOSTNAME` — override where `posthook dash` binds (default `127.0.0.1:3847`). Read by both the Go command and the dashboard server so they always agree.
- `POSTHOOK_DASH_AUTOSTART=0` — skip the automatic dashboard start at the end of `posthook init`.
- `POSTHOOK_CLOUD_ENDPOINT` / `POSTHOOK_CLOUD_TOKEN` / `POSTHOOK_CLOUD_ENABLED` / `POSTHOOK_CLOUD_FLUSH_SECS` — override cloud-sync settings without editing `config.json` (handy for pointing one binary at a staging endpoint).
- `POSTHOOK_API_KEY` / `POSTHOOK_VERSION` / `POSTHOOK_INSTALL_DIR` — read by `install.sh`: a team install key enables cloud sync + the background daemon, `POSTHOOK_VERSION` pins a release (default: latest), and `POSTHOOK_INSTALL_DIR` chooses the install directory (default `~/.local/bin`).

## Privacy

By default, everything stays in `~/.posthook/posthook.db` on your machine. No HTTP calls, no telemetry, no account.

**What's stored locally:**
- **Tool-call payloads, verbatim**, in `events.payload`. For `Edit` calls this includes `old_string` and `new_string`; for `Bash` calls, the command. If your environment requires redaction, the place to add it is `internal/ingest/ingest.go` (see `AgentEvent`).
- Absolute file paths, plus a derived repo-relative path.
- Commit metadata (author email, message subject, file paths) from your local git.

**What's written to `refs/notes/posthook` and pushed to remotes:** line ranges, agent slug, session ID, model name, event timestamp. **No prompt text. No code snippets.** To remove the auto-configured push refspec:

```bash
git config --unset-all remote.origin.push refs/notes/posthook:refs/notes/posthook
```

Nothing in the local database is encrypted at rest — treat it like any local git checkout. Data only leaves your machine if you explicitly enable cloud sync.

## Team / cloud

Posthook has a hosted team version (by Bilanc) that rolls the same local data up across your whole team — org-wide AI-adoption metrics, per-engineer and per-repo breakdowns, and shared attribution — without anyone changing their workflow.

Onboarding is one shared link. A manager mints an install link for the team, then each engineer runs it once:

```bash
curl -fsSL "https://api.bilanc.co/posthook/install.sh?apiKey=<team-key>" | sh
```

That installs posthook exactly as in the [Quickstart](#quickstart), and additionally: writes the team key into `~/.posthook/config.json`, runs `posthook init`, and installs the background sync daemon (`posthook service`). From then on the same OSS binary in this repo flushes rows upstream via `posthook sync` — reading rows changed since the last flush and POSTing them to your team's ingest endpoint with a Bearer token (inspect state with `posthook sync --status`). The local SQLite store stays authoritative; sync is a faithful, append-only replica with no redaction or schema rewrites. Until you install with a key, posthook never sends anything anywhere.

Access is invite-based — **[talk to the team](https://bilanc.co)** to get set up.

## Architecture

- **Single static Go binary**, built with `go build` — no runtime required on user machines.
- **SQLite via `modernc.org/sqlite`** — a pure-Go driver, no CGO, cross-compiles to every platform. The schema mirrors the eventual server-side shape, so the cloud migration path stays simple.
- **Per-agent installer modules** (`internal/installers/`) all follow the same idempotent merge-and-deduplicate pattern: read the existing config, surgically add or update only posthook's entry, write atomically.
- **Line-range extractor** (`internal/lineranges/`) — a pure function from post-edit file content + tool input to the line range(s) the AI wrote. Testable in isolation.
- **Transcript parser** (`internal/transcript/`) reads Claude Code JSONL for model + session-span backfill and prompt lookup during `posthook blame`.
- **Cloud sync** (`internal/sync/`, `internal/config/`) — a per-row `synced_at` cursor and a single batched POST per flush. Append-only and at-least-once; safe to retry.
- **Schema migrations** run automatically on `store.Open()` via `ALTER TABLE` guards, backfill passes for repo IDs and session models, and `datetime()`-wrapped time comparisons for timezone-correct windowing.

### Repo layout

```
cmd/posthook/main.go             Entry point + argv[0] dispatch (git shadow vs posthook CLI)
internal/
  paths/                         ~/.posthook, agent config paths, notes ref
  config/                        ~/.posthook/config.json (cloud-sync settings) + env overrides
  logx/                          POSTHOOK_DEBUG-gated stderr logger
  atomicfs/                      Atomic file write (temp + rename)
  gitx/                          findRepoRoot, relPathInRepo, Canonicalize, BypassEnv
  lineranges/                    Pure line-range extractor for Edit/Write/MultiEdit/apply_patch
  transcript/                    Claude Code JSONL parser + prompt extraction
  store/                         SQLite schema, migrations, backfill passes, attribution
  sync/                          Cloud flush: per-row synced_at cursor, batched POST, sync_state
  service/                       launchd / systemd unit manager for the background sync daemon
  proxy/                         Git shadow: spawn real git, forward stdio/signals,
                                 intercept commit + clone for capture
  installers/                    Per-agent hook installers (idempotent merge/dedup)
    base.go                      Shared helpers
    claudecode.go                ~/.claude/settings.json
    cursor.go                    ~/.cursor/hooks.json
    codex.go                     ~/.codex/config.toml
    githook.go                   per-repo post-commit hook + global template + notes transport
  ingest/                        Event + commit capture core
  commands/                      Cobra command implementations
dash/                            Web dashboard (Next.js), downloaded into ~/.posthook/dash by install.sh
install.sh                       Network installer: downloads the release binary + dashboard bundle
.goreleaser.yaml                 Release build (CGO_ENABLED=0 cross-compile to darwin/linux × amd64/arm64)
```

## Limitations

- **Bash-driven edits aren't line-attributed.** Only `Edit`, `Write`, and `MultiEdit` tool calls produce line ranges.
- **The shadow only intercepts `commit` and `clone`.** Other history-changing commands (rebase, cherry-pick, amend, reset, push, fetch) pass through without capture.
- **`MultiEdit` with identical replacement strings** attributes both edits to the first match.
- **Windows isn't supported yet.** The shadow relies on Unix symlinks and POSIX signal numbers.

## Building from source

You only need a toolchain to hack on posthook — installing uses a prebuilt binary.

```bash
git clone https://github.com/Bilanc/posthook && cd posthook

# CLI -> ~/.local/bin/posthook
go build -o ~/.local/bin/posthook ./cmd/posthook
go test ./...

# Dashboard (optional; needs Node >=24):
cd dash && npm ci && npm run build
```

`posthook dash` runs the dashboard from `~/.posthook/dash`, which `install.sh` fills from the release. To run a from-source dashboard, stage the standalone build there: copy `dash/.next/standalone/.`, `dash/.next/static` (into `.next/static`), and `dash/public` into `~/.posthook/dash`.

## Contributing

Issues and pull requests are welcome. The CLI has no runtime dependencies beyond a Go toolchain, so `go build ./...` and `go test ./...` are all you need to get going. Each `internal/` package is small and single-purpose — the line-range extractor, installers, and transcript parser are all pure and unit-tested, which is the easiest place to start.
