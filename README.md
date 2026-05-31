# posthook

Local-first instrumentation for AI-assisted coding. Installs hooks into Claude Code, Cursor, and Codex CLI, installs a lightweight `git` shadow that intercepts every git command across every repo on your machine, captures both sides into a single SQLite file you can query, and ties every line of code to the AI session that wrote it via `posthook blame`.

Built as the data-collection layer for a team-rollup product. Today it runs entirely on your laptop — no network, no account, no cloud. Attribution data also lives in `refs/notes/posthook` inside each repo, so a teammate who clones the repo can run `posthook blame` and see who wrote what without any account or sync.

This is the Go rewrite of the original TypeScript posthook, now bundled with the web dashboard (the Next.js app under `dash/`). See the [Architecture](#architecture) section for the layout.

## Quickstart

```bash
# 1. Build the CLI (Go 1.23+) AND the dashboard (Node.js 20+).
#    install.sh builds both; the dashboard build is skipped (with a warning) if
#    Node is absent — the CLI still installs and works.
./install.sh

# Or build just the CLI manually:
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/posthook ./cmd/posthook
install -m 0755 dist/posthook ~/.local/bin/posthook

# 2. Make sure ~/.local/bin precedes the real git on PATH (usually /usr/bin):
export PATH="$HOME/.local/bin:$PATH"

# 3. Install hooks + the git shadow
posthook init

# 4. After using Claude Code / Cursor / Codex for a while:
posthook status
posthook metrics
posthook blame path/to/file.ts

# 5. Open the web dashboard (starts the local server if it isn't running):
posthook dash
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

The shadow is a single symlink. When you run `git status`, the shell resolves `git` via PATH to `~/.local/bin/git` (our symlink), which executes the posthook binary. The binary inspects `os.Args[0]` — if it sees `git`, it runs the real git binary as a child process with all the original args, forwards stdio and signals faithfully, and exits with git's exit code. For successful `commit` and `clone` subcommands it also runs our capture logic. For everything else, it's pure passthrough.

Internally posthook saves the location of the real git binary to `~/.posthook/git-path` at install time. Our own ingest path sets `POSTHOOK_BYPASS=1` when shelling out to git, so internal queries (`git log`, `git rev-parse`, `git notes`) don't recurse through the proxy.

## Where data lives

| Path | What |
|---|---|
| `~/.posthook/posthook.db` | SQLite — all your data (`events`, `event_line_ranges`, `sessions`, `commits`, `commit_files`, `repositories`) |
| `~/.posthook/git-path` | Absolute path to the real git binary. Written at install-shadow time, read on every shadow invocation. |
| `~/.posthook/dash/` | Staged Next.js standalone build (`server.js`, traced `node_modules`, `.next/static`). Written by `install.sh`; spawned by `posthook dash`. |
| `~/.posthook/dash.pid` / `~/.posthook/dash.log` | PID + stdout/stderr of the background dashboard server. |
| `~/.local/bin/git` | The shadow symlink → `~/.local/bin/posthook`. Created by `posthook install-shadow`. |
| `~/.posthook/git-template/hooks/post-commit` | Fallback hook copied into every new repo by `git init` |
| `~/.claude/settings.json` | Claude Code hook entries — **merged alongside your existing hooks** |
| `~/.cursor/hooks.json` | Cursor hook entries — merged |
| `~/.codex/config.toml` | Codex hook entries — merged |
| `~/.gitconfig` (`init.templateDir`) | Points git at the template dir above |
| Each tracked repo's `refs/notes/posthook` | Line-attribution metadata (no prompt text). Auto-pushed/fetched on `origin` so blame works after clone. |

All installers are idempotent and preserve any pre-existing user hooks. Re-running `posthook init` after a binary upgrade refreshes the embedded path without clobbering anything.

## Commands

- `posthook init` — install hooks + shadow + template fallback, then auto-start the web dashboard in the background. Idempotent. (Set `POSTHOOK_DASH_AUTOSTART=0` to skip the dashboard; it's also skipped silently if the dashboard isn't built or Node.js is absent.)
- `posthook install-shadow` / `posthook uninstall-shadow` — manage the `git` symlink.
- `posthook track <repo-path>` — fallback per-repo post-commit hook for existing repos.
- `posthook ingest --agent <slug>` — read agent hook payload from stdin (called by hooks).
- `posthook ingest --kind git-commit --repo-root <p> --sha <sha>` — record a git commit (called by hooks).
- `posthook status` — counts, shadow health, hook misfires, recent commits. Run this first.
- `posthook metrics` — AI metrics with breakdowns by agent/model/repo.
- `posthook blame <file>` — per-line attribution with prompt headers.
- `posthook inspect [--agent X] [--type Y] [--session Z] [--since ISO] [--limit N]` — raw event payloads.
- `posthook dash` — open the local web dashboard. Starts the bundled Next.js server (reading `~/.posthook/posthook.db`) if it isn't already running, waits for it to come up, then opens your browser. `--no-open` starts it without opening a browser; `--stop` shuts the background server down. Binds `127.0.0.1:3847` by default.

### Environment variables

- `POSTHOOK_BIN` — override the binary path written into hook configs (useful for dev installs).
- `POSTHOOK_DEBUG=1` — verbose stderr logging from every command.
- `POSTHOOK_BYPASS=1` — when set, the git shadow passes straight through. Used internally to prevent recursion when our ingest path shells out to git; useful manually to temporarily run a command under "plain git" semantics without uninstalling the shadow.
- `POSTHOOK_DASH_PORT` / `POSTHOOK_DASH_HOSTNAME` — override where `posthook dash` binds the dashboard server (default `127.0.0.1:3847`). Read by both the Go `dash` command and the Next.js server, so they always agree.
- `POSTHOOK_DASH_AUTOSTART=0` — skip the automatic dashboard start at the end of `posthook init`.
- `POSTHOOK_DB` — override the SQLite path the dashboard reads (default `~/.posthook/posthook.db`).

## Privacy

Everything stays in `~/.posthook/posthook.db` on your machine. No HTTP calls, no telemetry, no account.

What's stored:
- **Tool-call payloads are stored verbatim** in `events.payload` (TEXT). For `Edit` calls this includes `old_string` and `new_string`. For `Bash` calls it includes the command. If your environment requires redaction, the place to add it is in `internal/ingest/ingest.go` (see `AgentEvent`).
- Absolute file paths, plus a derived repo-relative path.
- Commit metadata (author email, message subject, file paths) from your local git.

**What's in `refs/notes/posthook` and pushed to remotes:** Line ranges, agent slug, session ID, model name, event timestamp. No prompt text. No code snippets.

To remove the auto-configured push refspec:
```
git config --unset-all remote.origin.push refs/notes/posthook:refs/notes/posthook
```

Nothing in the local database is encrypted at rest. Treat it like any local git checkout.

## Architecture

- **Go 1.23+**, compiled to a single static binary via `go build`. No runtime required on user machines.
- **SQLite via `modernc.org/sqlite`** — pure Go driver, no CGO, cross-compiles to every platform. Schema mirrors what we'll eventually want in Postgres so the migration path is `INSERT INTO postgres SELECT * FROM sqlite` per table.
- **Per-agent installer modules** (`internal/installers/`) follow the same idempotent merge-and-deduplicate pattern: read existing config, surgically add/update only posthook's command, write atomically.
- **Line-range extractor** (`internal/lineranges/`) takes the post-edit file content and the tool input, returns the line range(s) the AI wrote. Pure function, testable in isolation.
- **Stop-hook transcript parser** (`internal/transcript/`) reads Claude Code JSONL transcripts for model + session-span backfill, and for prompt-lookup during `posthook blame`.
- **Schema migrations** run automatically on `store.Open()` via `ALTER TABLE` guards, backfill passes for repo IDs and session models, and `datetime()`-wrapped time comparisons for timezone-correct windowing.

## Cloud version

Posthook also has a cloud version (built in `bilanc-cloud`) for teams. When a user authenticates against the cloud, hook configs are updated to point at the cloud's HTTP ingest API instead of the local binary. Data lands in Supabase Postgres against a schema that mirrors the SQLite schema in this repo. The OSS binary in this repo stays local-only.

## Limitations

- **Bash-driven edits aren't line-attributed.** Only `Edit`, `Write`, and `MultiEdit` tool calls produce line ranges. Sub-line attribution is a Phase 3 question.
- **The shadow only intercepts `commit` and `clone`.** Other history-changing commands (rebase, cherry-pick, amend, reset, push, fetch) pass through without our hooks running.
- **Windows isn't supported yet.** The shadow uses Unix symlinks and POSIX signal numbers.
- **MultiEdit with identical replacement strings** attributes both to the first match.

## Repo layout

```
cmd/posthook/main.go             Entry point + argv[0] dispatch (git shadow vs posthook CLI)
internal/
  paths/                         ~/.posthook, agent config paths, notes ref
  logx/                          POSTHOOK_DEBUG-gated stderr logger
  atomicfs/                      Atomic file write (temp + rename)
  gitx/                          findRepoRoot, relPathInRepo, Canonicalize, BypassEnv
  lineranges/                    Pure line-range extractor for Edit/Write/MultiEdit/apply_patch
  transcript/                    Claude Code JSONL parser + prompt extraction
  store/                         SQLite schema, migrations, backfill passes, attribution
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
install.sh                       curl-pipe installer (v0 placeholder)
```

## Internal

Source of truth lives at `/Users/samuelakinwunmi/Desktop/Bilanc/posthook-go`.
