package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/paths"
	"github.com/bilanc/posthook/internal/spool"
)

// Runs that are not recorded.
//
// Scripts and bots that run an agent unattended set POSTHOOK_DISABLE=1 so
// their runs are not recorded: their prompts are not stored and they don't
// count as an engineer's sessions. The agent's hooks still call `posthook
// ingest`, which keeps none of the event. It remembers two things only:
//
//   - the session's id, in ~/.posthook/disabled-sessions, so the session stays
//     unrecorded if it is later resumed without the variable. Otherwise the
//     first recorded Stop would import the whole transcript, prompts of the
//     unrecorded run included. The marker never expires: posthook cannot know
//     how long the agent keeps the transcript, and any expiry would let a
//     later resume import it. Markers are empty files; deleting the directory
//     forgets them all;
//   - at the end of a turn, the repository the agent ran in, as a commit job,
//     so commits made there by a git posthook did not see are still recorded
//     (catchup.go). Commits are never part of what is switched off.

// AgentEventsDisabled reports whether POSTHOOK_DISABLE=1 is set. Only the
// exact value 1 counts, as with POSTHOOK_BYPASS.
func AgentEventsDisabled() bool {
	return os.Getenv("POSTHOOK_DISABLE") == "1"
}

// disabledSessionsDir holds one empty file per session that ran with
// POSTHOOK_DISABLE=1, named by a hash of the agent and session id.
func disabledSessionsDir() string { return filepath.Join(paths.PosthookDir(), "disabled-sessions") }

func disabledSessionMarker(agentSlug, rawSessionID string) string {
	sum := sha256.Sum256([]byte(agentSlug + "\x00" + rawSessionID))
	return filepath.Join(disabledSessionsDir(), hex.EncodeToString(sum[:16]))
}

// sessionDisabled reports whether the session ever ran with
// POSTHOOK_DISABLE=1.
func sessionDisabled(agentSlug, rawSessionID string) bool {
	_, err := os.Stat(disabledSessionMarker(agentSlug, rawSessionID))
	return err == nil
}

// DropAgentEvent handles one hook event of a run that is not recorded: it
// reads the payload to the end, so the agent never sees a broken pipe, keeps
// none of it, and notes what the comment above describes. It reports whether
// it queued a commit job, in which case the caller wakes the worker. It never
// fails: a hook must not, and a run that is not recorded loses nothing by it.
func DropAgentEvent(agentSlug string) (queued bool) {
	raw, err := readStdin()
	if err != nil || len(raw) == 0 {
		return false
	}
	return dropPayload(agentSlug, raw)
}

// dropPayload keeps none of raw: it marks the session as unrecorded and, at
// the end of a turn, queues a commit job for the repository.
func dropPayload(agentSlug string, raw []byte) (queued bool) {
	payload := parseHookPayload(agentSlug, raw)
	if payload == nil {
		return false
	}
	if session := pickString(payload, "session_id", "sessionId"); session != "" {
		markSessionDisabled(agentSlug, session)
	}
	if normalizeEventType(pickString(payload, "hook_event_name", "event", "type")) != "Stop" {
		return false
	}
	cwd := pickString(payload, "cwd")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	root := gitx.FindRepoRoot(cwd)
	if root == "" {
		return false
	}
	if err := spool.WriteCommitJob(spool.CommitJob{
		RepoRoot:   root,
		ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		logx.Debugf("ingest --agent %s: commit job for an unrecorded turn: %v", agentSlug, err)
		return false
	}
	return true
}

func parseHookPayload(agentSlug string, raw []byte) map[string]any {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	if agentSlug == copilotAgentSlug {
		payload = normalizeCopilotPayload(payload)
	}
	return payload
}

// markSessionDisabled creates the session's marker if it is not there yet.
// It is empty and readable only by its owner.
func markSessionDisabled(agentSlug, rawSessionID string) {
	if err := os.MkdirAll(disabledSessionsDir(), 0o700); err != nil {
		return
	}
	if f, err := os.OpenFile(disabledSessionMarker(agentSlug, rawSessionID), os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
}

// resumedDisabledPayload reports whether raw belongs to a session that ran
// with POSTHOOK_DISABLE=1 and is running again without it. The marker
// directory only exists once something ran disabled, so until then this
// costs one stat and the payload is not parsed.
func resumedDisabledPayload(agentSlug string, raw []byte) bool {
	if _, err := os.Stat(disabledSessionsDir()); err != nil {
		return false
	}
	payload := parseHookPayload(agentSlug, raw)
	if payload == nil {
		return false
	}
	session := pickString(payload, "session_id", "sessionId")
	return session != "" && sessionDisabled(agentSlug, session)
}
