package installers

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// These tests run the generated extension under Node, driving it through a
// fake Pi API, with a stand-in posthook binary. They need a Node that runs
// TypeScript directly (22.18+ / 23.6+) and are skipped without one.

func requireNodeTS(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "-p", "Boolean(process.features.typescript)").Output()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Skip("this node cannot run TypeScript files")
	}
	return node
}

// piDriver loads the extension into a fake Pi and runs scenario, a JS
// function body with fire(event, payload), write(path, text) and cwd in
// scope; session_shutdown is fired at the end.
const piDriver = `
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import ext from "./posthook.ts";
const handlers = {};
ext({ on: (e, h) => (handlers[e] ??= []).push(h) });
const cwd = process.env.DRIVER_CWD;
const ctx = { cwd, model: { id: "m" }, sessionManager: { getSessionId: () => "s1", getSessionFile: () => join(cwd, "s1.jsonl") } };
const fire = async (e, ev) => { for (const h of handlers[e] ?? []) await h(ev, ctx); };
const write = (p, text) => writeFileSync(join(cwd, p), text);
const scenario = new Function("fire", "write", "cwd", "return (async () => {" + process.env.DRIVER_SCENARIO + "})()");
await scenario(fire, write, cwd);
await fire("session_shutdown", {});
`

func runPiExtension(t *testing.T, node, bin, scenario string) time.Duration {
	t.Helper()
	dir := t.TempDir()
	src, err := renderPiExtension(bin)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "posthook.ts"), src)
	mustWrite(t, filepath.Join(dir, "driver.mjs"), piDriver)
	cwd := filepath.Join(dir, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "--disable-warning=ExperimentalWarning", filepath.Join(dir, "driver.mjs"))
	cmd.Env = append(os.Environ(), "DRIVER_CWD="+cwd, "DRIVER_SCENARIO="+scenario)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("driver: %v\n%s", err, out)
	}
	return time.Since(start)
}

// fakePosthook is a stand-in binary that saves each event it is sent as its
// own file in dir.
func fakePosthook(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "posthook")
	mustWrite(t, bin, "#!/bin/sh\ncat > \""+dir+"/$$.json\"\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func sentToolUseIDs(t *testing.T, dir string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var ids []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var p map[string]any
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatalf("%s: %v: %s", f, err, data)
		}
		if id, ok := p["tool_use_id"].(string); ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// A failed edit or write is reported when the file changed anyway (Pi checks
// for cancellation after writing) and dropped when it did not; a failed shell
// command is always reported.
func TestPiExtensionReportsFailedFileToolsThatChangedTheFile(t *testing.T) {
	node := requireNodeTS(t)
	events := t.TempDir()
	runPiExtension(t, node, fakePosthook(t, events), `
		write("a.txt", "one\n");
		// Cancelled after its write reached the disk.
		await fire("tool_call", { toolName: "edit", toolCallId: "cancelled-edit", input: { path: "a.txt", edits: [] } });
		write("a.txt", "two\n");
		await fire("tool_result", { toolName: "edit", toolCallId: "cancelled-edit", input: { path: "a.txt", edits: [] }, isError: true });
		// Failed before writing (no match).
		await fire("tool_call", { toolName: "edit", toolCallId: "no-match", input: { path: "a.txt", edits: [] } });
		await fire("tool_result", { toolName: "edit", toolCallId: "no-match", input: { path: "a.txt", edits: [] }, isError: true });
		// A write that failed before creating its file.
		await fire("tool_call", { toolName: "write", toolCallId: "write-failed", input: { path: "new.txt", content: "x\n" } });
		await fire("tool_result", { toolName: "write", toolCallId: "write-failed", input: { path: "new.txt", content: "x\n" }, isError: true });
		// A write cancelled after creating it, with Pi's "@" prefix.
		await fire("tool_call", { toolName: "write", toolCallId: "write-cancelled", input: { path: "@new2.txt", content: "y\n" } });
		write("new2.txt", "y\n");
		await fire("tool_result", { toolName: "write", toolCallId: "write-cancelled", input: { path: "@new2.txt", content: "y\n" }, isError: true });
		// Successful calls and failed shell commands are always reported.
		await fire("tool_call", { toolName: "edit", toolCallId: "ok-edit", input: { path: "a.txt", edits: [] } });
		await fire("tool_result", { toolName: "edit", toolCallId: "ok-edit", input: { path: "a.txt", edits: [] }, isError: false });
		await fire("tool_result", { toolName: "bash", toolCallId: "bash-failed", input: { command: "false" }, isError: true });
	`)
	got := sentToolUseIDs(t, events)
	want := []string{"bash-failed", "cancelled-edit", "ok-edit", "write-cancelled"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("reported tool calls = %v, want %v", got, want)
	}
}

// An ingest that never finishes must not keep Pi running past the bounded
// wait in session_shutdown.
func TestPiExtensionDoesNotHoldPiOpen(t *testing.T) {
	node := requireNodeTS(t)
	bin := filepath.Join(t.TempDir(), "posthook")
	mustWrite(t, bin, "#!/bin/sh\nsleep 30\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	took := runPiExtension(t, node, bin, `
		await fire("tool_result", { toolName: "bash", toolCallId: "t1", input: { command: "true" }, isError: false });
	`)
	if took > 10*time.Second {
		t.Errorf("Pi stayed up %s waiting for a stuck ingest; want about 2s", took)
	}
}

// When every event has gone through, shutdown returns at once rather than
// sitting out the flush timeout.
func TestPiExtensionShutdownDoesNotWaitOutTheTimeout(t *testing.T) {
	node := requireNodeTS(t)
	bin := filepath.Join(t.TempDir(), "posthook")
	mustWrite(t, bin, "#!/bin/sh\nsleep 0.3\ncat > /dev/null\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// Shutdown starts while the ingest is still running, so it waits for it.
	took := runPiExtension(t, node, bin, `
		await fire("tool_result", { toolName: "bash", toolCallId: "t1", input: { command: "true" }, isError: false });
	`)
	if took > 1800*time.Millisecond {
		t.Errorf("driver took %s; shutdown should not wait out the 2s flush timeout", took)
	}
}

// An edit's result carries the whole-line patch Pi computed; the extension
// passes it on with the file as it was before the edit (as on disk, BOM
// included), so posthook can count the edit exactly. Nothing
// else of a tool's result is sent, and a write sends neither.
func TestPiExtensionSendsTheEditPatch(t *testing.T) {
	node := requireNodeTS(t)
	events := t.TempDir()
	runPiExtension(t, node, fakePosthook(t, events), `
		write("a.txt", "\uFEFFa\n");
		await fire("tool_call", { toolName: "edit", toolCallId: "e1", input: { path: "a.txt", edits: [] } });
		write("a.txt", "\uFEFFb\n");
		await fire("tool_result", { toolName: "edit", toolCallId: "e1", input: { path: "a.txt", edits: [] }, isError: false,
			content: [{ type: "text", text: "Successfully replaced 1 block(s)" }], details: { diff: "display", patch: "@@ -1 +1 @@\n-a\n+b\n" } });
		write("big.txt", "x".repeat(5 * 1024 * 1024));
		await fire("tool_call", { toolName: "edit", toolCallId: "e2", input: { path: "big.txt", edits: [] } });
		await fire("tool_result", { toolName: "edit", toolCallId: "e2", input: { path: "big.txt", edits: [] }, isError: false,
			details: { patch: "@@ -1 +1 @@\n-x\n+y\n" } });
		await fire("tool_call", { toolName: "write", toolCallId: "w1", input: { path: "a.txt", content: "x" } });
		await fire("tool_result", { toolName: "write", toolCallId: "w1", input: { path: "a.txt", content: "x" }, isError: false,
			content: [{ type: "text", text: "ok" }], details: { patch: "should not be sent" } });
	`)
	files, _ := filepath.Glob(filepath.Join(events, "*.json"))
	got := map[string]map[string]any{}
	for _, f := range files {
		data, _ := os.ReadFile(f)
		var p map[string]any
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		resp, _ := p["tool_response"].(map[string]any)
		got[p["tool_use_id"].(string)] = resp
	}
	want := map[string]any{"is_error": false, "patch": "@@ -1 +1 @@\n-a\n+b\n", "originalFile": "\uFEFFa\n"}
	if !reflect.DeepEqual(got["e1"], want) {
		t.Errorf("edit tool_response = %#v\nwant %#v", got["e1"], want)
	}
	if _, has := got["e2"]["originalFile"]; has || got["e2"]["patch"] == nil {
		t.Errorf("edit of a file over the size limit = %#v, want the patch and no originalFile", got["e2"])
	}
	if len(got["w1"]) != 1 {
		t.Errorf("write tool_response = %#v, want only is_error", got["w1"])
	}
}

// With POSTHOOK_DISABLE=1 the extension sends nothing, so a Pi run by a script
// or bot is never recorded.
func TestPiExtensionSendsNothingWhenDisabled(t *testing.T) {
	node := requireNodeTS(t)
	events := t.TempDir()
	t.Setenv("POSTHOOK_DISABLE", "1")
	runPiExtension(t, node, fakePosthook(t, events), `
		await fire("before_agent_start", {});
		await fire("tool_call", { toolName: "write", toolCallId: "w1", input: { path: "a.txt", content: "x" } });
		write("a.txt", "x");
		await fire("tool_result", { toolName: "write", toolCallId: "w1", input: { path: "a.txt", content: "x" }, isError: false });
		await fire("agent_settled", {});
	`)
	if files, _ := filepath.Glob(filepath.Join(events, "*.json")); len(files) != 0 {
		t.Errorf("sent %d event(s) with POSTHOOK_DISABLE=1, want none", len(files))
	}
}
