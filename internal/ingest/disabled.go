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
//     unrecorded run included;
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
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return false
	}
	if agentSlug == copilotAgentSlug {
		payload = normalizeCopilotPayload(payload)
	}
	if session := pickString(payload, "session_id", "sessionId"); session != "" {
		marker := disabledSessionMarker(agentSlug, session)
		if err := os.MkdirAll(disabledSessionsDir(), 0o755); err == nil {
			// Created, or touched so pruning keeps it while it is in use.
			if f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				f.Close()
				now := time.Now()
				_ = os.Chtimes(marker, now, now)
			}
		}
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

// PruneDisabledSessions removes the markers of sessions that have not run
// for longer than olderThan, so the directory does not grow without end. A
// marker is refreshed by every event of its session.
func PruneDisabledSessions(olderThan time.Duration) int {
	entries, err := os.ReadDir(disabledSessionsDir())
	if err != nil {
		return 0
	}
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(disabledSessionsDir(), e.Name())) == nil {
			removed++
		}
	}
	return removed
}
