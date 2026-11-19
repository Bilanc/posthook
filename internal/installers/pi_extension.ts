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
//   tool_result                         -> PostToolUse       (the tool's name and arguments, never its output)
//   agent_settled                       -> Stop              (posthook reads model, tokens and prompts from the session file)
//
// Each event runs `posthook ingest --agent pi`, which only writes the event to
// a spool file and returns; nothing here waits on it or can fail a Pi run.

import { spawn } from "node:child_process";

const POSTHOOK_BIN: string = __POSTHOOK_BIN__;
const FILE_TOOLS = new Set(["edit", "write"]);
const FLUSH_TIMEOUT_MS = 2000;

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

	pi.on("before_agent_start", (event, ctx) => {
		if (event?.messageType !== "user") return;
		send(base("UserPromptSubmit", ctx));
	});

	pi.on("tool_result", (event, ctx) => {
		// A failed edit or write changed nothing. A failed shell command may
		// still have written files, so it is reported like any other.
		if (event.isError && FILE_TOOLS.has(event.toolName)) return;
		const payload = base("PostToolUse", ctx);
		payload.tool_name = event.toolName;
		payload.tool_use_id = event.toolCallId;
		payload.tool_input = event.input;
		payload.tool_response = { is_error: Boolean(event.isError) };
		if (event.parentToolCallId) payload.parent_tool_use_id = event.parentToolCallId;
		send(payload);
	});

	pi.on("agent_settled", (event, ctx) => {
		const payload = base("Stop", ctx);
		if (event?.aborted) payload.stop_reason = "aborted";
		send(payload);
	});

	// `pi -p` exits as soon as the run ends: give the last events a moment to
	// reach posthook.
	pi.on("session_shutdown", async () => {
		if (pending.size === 0) return;
		await Promise.race([
			Promise.all([...pending]),
			new Promise((resolve) => setTimeout(resolve, FLUSH_TIMEOUT_MS)),
		]);
	});
}
