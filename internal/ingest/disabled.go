package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bilanc/posthook/internal/atomicfs"
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
//     unrecorded run included. The marker holds the transcript's path, when
//     the agent gave it, and lasts as long as that transcript exists;
//   - at the end of a turn, the repository the agent ran in, as a commit job,
//     so commits made there by a git posthook did not see are still recorded
//     (catchup.go). Commits are never part of what is switched off.

// AgentEventsDisabled reports whether POSTHOOK_DISABLE=1 is set. Only the
// exact value 1 counts, as with POSTHOOK_BYPASS.
func AgentEventsDisabled() bool {
	return os.Getenv("POSTHOOK_DISABLE") == "1"
}

// disabledSessionsDir holds one file per session that ran with
// POSTHOOK_DISABLE=1, named by a hash of the agent and session id. It holds
// the path of the session's transcript, when known, and nothing else.
func disabledSessionsDir() string { return filepath.Join(paths.PosthookDir(), "disabled-sessions") }

func disabledSessionMarker(agentSlug, rawSessionID string) string {
	sum := sha256.Sum256([]byte(agentSlug + "\x00" + rawSessionID))
	return filepath.Join(disabledSessionsDir(), hex.EncodeToString(sum[:16]))
}

// sessionDisabled reports whether the session ran with POSTHOOK_DISABLE=1.
// It stays so for as long as its marker exists, which is as long as the
// transcript holding the unrecorded prompts does (PruneDisabledSessions): a
// time limit would let a session resumed after it import them.
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

// dropPayload keeps none of raw: it refreshes the session's marker and, at
// the end of a turn, queues a commit job for the repository.
func dropPayload(agentSlug string, raw []byte) (queued bool) {
	payload := parseHookPayload(agentSlug, raw)
	if payload == nil {
		return false
	}
	if session := pickString(payload, "session_id", "sessionId"); session != "" {
		transcriptPath := pickString(payload, "transcript_path", "transcriptPath")
		if agentSlug == copilotAgentSlug {
			transcriptPath = copilotTranscriptPath(transcriptPath, session)
		}
		markSessionDisabled(agentSlug, session, transcriptPath)
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

// markSessionDisabled creates the session's marker, or refreshes it: every
// event of the session, recorded or not, updates its time. The transcript's
// path is written when the event carries one and kept when it does not.
func markSessionDisabled(agentSlug, rawSessionID, transcriptPath string) {
	marker := disabledSessionMarker(agentSlug, rawSessionID)
	if err := os.MkdirAll(disabledSessionsDir(), 0o755); err != nil {
		return
	}
	if transcriptPath != "" {
		if abs, err := filepath.Abs(transcriptPath); err == nil {
			transcriptPath = abs
		}
		_ = atomicfs.Write(marker, []byte(transcriptPath), 0o644)
	} else if f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
	now := time.Now()
	_ = os.Chtimes(marker, now, now)
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

// PruneDisabledSessions removes the markers that can go: a marker whose
// transcript no longer exists (there is nothing left to import), and a marker
// with no known transcript once its session has not run for idleFor (0 keeps
// those for ever). A marker whose transcript still exists is kept, however
// old.
func PruneDisabledSessions(idleFor time.Duration) int {
	entries, err := os.ReadDir(disabledSessionsDir())
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		path := filepath.Join(disabledSessionsDir(), e.Name())
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, _ := os.ReadFile(path)
		transcriptPath := strings.TrimSpace(string(data))
		var drop bool
		if transcriptPath != "" {
			_, err := os.Stat(transcriptPath)
			drop = errors.Is(err, os.ErrNotExist)
		} else {
			drop = idleFor > 0 && time.Since(info.ModTime()) > idleFor
		}
		if drop && os.Remove(path) == nil {
			removed++
		}
	}
	return removed
}
