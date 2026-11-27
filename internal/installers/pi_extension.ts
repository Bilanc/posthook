// posthook ingest --agent pi
//
// Installed by `posthook init` (https://github.com/Bilanc/posthook) and
// rewritten by every later `posthook init`; `posthook uninstall` deletes it.
// Edit the generator in internal/installers/pi_extension.ts, not this copy.
//
// Sends posthook one small JSON event per Pi event, the same shape Claude
// Code's hooks deliver, so posthook can attribute the lines Pi writes to the
// commits that ship them:
//
//   before_agent_start (a user prompt)  -> UserPromptSubmit  (no prompt text; prompts are read from the session file)
//   tool_result                         -> PostToolUse       (the tool's name and arguments, never its output; for an
//                                                             edit, the whole-line patch Pi computed and the file as it
//                                                             was before, so posthook can count it exactly; neither is
//                                                             stored)
//   agent_settled                       -> Stop              (posthook reads model, tokens and prompts from the session file)
//
// Each event runs `posthook ingest --agent pi`, which only writes the event to
// a spool file and returns; nothing here waits on it or can fail a Pi run, and
// the ingest processes never keep Pi from exiting. With POSTHOOK_DISABLE=1 in
// Pi's environment the run is not recorded and only a session id and the end
// of each turn are sent.

import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { readFileSync, statSync } from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join, resolve } from "node:path";

const POSTHOOK_BIN: string = __POSTHOOK_BIN__;
const FILE_TOOLS = new Set(["edit", "write"]);
const FLUSH_TIMEOUT_MS = 2000;
// Files up to this size are fingerprinted by content; larger ones by size and
// modification time.
const MAX_HASH_BYTES = 4 * 1024 * 1024;

// The path a file tool call works on, resolved the way Pi resolves it: a
// leading "@" is dropped, "~" is the home directory, and a relative path is
// relative to the session's cwd.
function toolPath(input: any, cwd: string): string | undefined {
	let p = input?.path;
	if (typeof p !== "string" || p.trim() === "") return undefined;
	if (p.startsWith("@")) p = p.slice(1);
	if (p === "~") p = homedir();
	else if (p.startsWith("~/")) p = join(homedir(), p.slice(2));
	return isAbsolute(p) ? p : resolve(cwd, p);
}

// A file's current state: a fingerprint ("missing", or a content hash), and
// for files up to MAX_HASH_BYTES the content itself.
function readState(path: string): { state: string; content?: Buffer } {
	try {
		const st = statSync(path);
		if (!st.isFile()) return { state: `other:${st.mode}` };
		if (st.size > MAX_HASH_BYTES) return { state: `stat:${st.size}:${st.mtimeMs}` };
		const content = readFileSync(path);
		return { state: createHash("sha256").update(content).digest("hex"), content };
	} catch {
		return { state: "missing" };
	}
}

function fingerprint(path: string): string {
	return readState(path).state;
}

type Context = {
	cwd: string;
	model?: { id?: string };
	sessionManager: {
		getSessionId(): string;
		getSessionFile(): string | undefined;
	};
};

type Api = {
	on(event: string, handler: (event: any, ctx: Context) => unknown): void;
};

export default function posthook(pi: Api) {
	const pending = new Set<Promise<void>>();

	function send(payload: Record<string, unknown>): void {
		let body: string;
		try {
			body = JSON.stringify(payload);
		} catch {
			return;
		}
		const done = new Promise<void>((resolve) => {
			try {
				const child = spawn(POSTHOOK_BIN, ["ingest", "--agent", "pi"], {
					stdio: ["pipe", "ignore", "ignore"],
					windowsHide: true,
				});
				child.on("error", () => resolve());
				child.on("close", () => resolve());
				child.stdin.on("error", () => {});
				child.stdin.end(body);
				// Neither the process nor its stdin pipe may keep Pi running:
				// session_shutdown bounds the wait for the last events instead.
				child.unref();
				(child.stdin as any).unref?.();
			} catch {
				resolve();
			}
		});
		pending.add(done);
		void done.then(() => pending.delete(done));
	}

	function base(event: string, ctx: Context): Record<string, unknown> {
		const out: Record<string, unknown> = { hook_event_name: event, cwd: ctx.cwd };
		try {
			out.session_id = ctx.sessionManager.getSessionId();
			const file = ctx.sessionManager.getSessionFile();
			if (file) out.transcript_path = file;
		} catch {}
		if (ctx.model?.id) out.model = ctx.model.id;
		return out;
	}

	// `pi -p` exits as soon as the run ends: give the last events a moment to
	// reach posthook.
	pi.on("session_shutdown", async () => {
		if (pending.size === 0) return;
		let timer: ReturnType<typeof setTimeout> | undefined;
		await Promise.race([
			Promise.all([...pending]),
			new Promise((resolve) => {
				timer = setTimeout(resolve, FLUSH_TIMEOUT_MS);
			}),
		]);
		clearTimeout(timer);
	});

	// POSTHOOK_DISABLE=1: a Pi run by a script or bot is not recorded. posthook
	// is sent no prompt, tool call or file content: only the session's id and
	// session file path, once, so the session stays unrecorded if it is resumed
	// without the variable, and the end of each turn with its directory, so
	// commits made there are still recorded.
	if (process.env.POSTHOOK_DISABLE === "1") {
		const marked = new Set<string>();
		const minimal = (event: string, ctx: Context): Record<string, unknown> => {
			const out: Record<string, unknown> = { hook_event_name: event, cwd: ctx.cwd };
			try {
				out.session_id = ctx.sessionManager.getSessionId();
				// Where the session's prompts are kept, so posthook can keep
				// the session unrecorded for as long as that file exists.
				const file = ctx.sessionManager.getSessionFile();
				if (file) out.transcript_path = file;
			} catch {}
			return out;
		};
		pi.on("before_agent_start", (_event, ctx) => {
			const payload = minimal("UserPromptSubmit", ctx);
			const id = String(payload.session_id ?? "");
			if (marked.has(id)) return;
			marked.add(id);
			send(payload);
		});
		pi.on("agent_settled", (_event, ctx) => send(minimal("Stop", ctx)));
		return;
	}

	pi.on("before_agent_start", (event, ctx) => {
		if (event?.messageType !== "user") return;
		send(base("UserPromptSubmit", ctx));
	});

	// The state of each file an edit or write is about to change, so a failed
	// call can be checked against the disk: most failures (no match, no
	// permission) write nothing, but a call cancelled after its write reached
	// the disk also ends in an error. For an edit the content is kept too:
	// its patch is made against it.
	const before = new Map<string, { path: string; state: string; original?: string }>();

	pi.on("tool_call", (event, ctx) => {
		if (!FILE_TOOLS.has(event.toolName)) return;
		const path = toolPath(event.input, ctx.cwd);
		if (!path) return;
		const { state, content } = readState(path);
		// As on disk, BOM included: posthook puts the BOM Pi strips before
		// patching back on the patch's first line, since Pi writes it back.
		const original = event.toolName === "edit" && content ? content.toString("utf8") : undefined;
		before.set(event.toolCallId, { path, state, original });
	});

	pi.on("tool_result", (event, ctx) => {
		const prior = before.get(event.toolCallId);
		before.delete(event.toolCallId);
		// A failed edit or write is reported only when the file changed anyway.
		// A failed shell command may still have written files, so it is
		// reported like any other.
		if (event.isError && FILE_TOOLS.has(event.toolName)) {
			if (!prior || fingerprint(prior.path) === prior.state) return;
		}
		const payload = base("PostToolUse", ctx);
		payload.tool_name = event.toolName;
		payload.tool_use_id = event.toolCallId;
		payload.tool_input = event.input;
		const response: Record<string, unknown> = { is_error: Boolean(event.isError) };
		// oldText/newText can cut through lines; the patch Pi built from them
		// covers whole lines. With the file as it was before the edit,
		// posthook applies the patch to it, as it does with Claude Code's
		// originalFile, so the edit's lines keep their author even when a
		// later edit has moved them by the time posthook reads the file.
		// Neither is stored.
		if (event.toolName === "edit" && !event.isError && typeof event.details?.patch === "string") {
			response.patch = event.details.patch;
			if (prior?.original !== undefined) response.originalFile = prior.original;
		}
		payload.tool_response = response;
		if (event.parentToolCallId) payload.parent_tool_use_id = event.parentToolCallId;
		send(payload);
	});

	pi.on("agent_settled", (event, ctx) => {
		const payload = base("Stop", ctx);
		if (event?.aborted) payload.stop_reason = "aborted";
		send(payload);
	});

}
