// Package spool is the crash-safe handoff between the synchronous agent hook
// and the asynchronous worker that does the real ingest work.
//
// The hook fires on every agent tool call (Read, Grep, Edit, …). Doing the
// full ingest there — opening SQLite, scanning tables, shelling out to git —
// meant each fire was slow and they piled up under bursts until they saturated
// the CPU. Instead the hook now does the cheapest possible thing: capture the
// payload + cwd and drop a single small file into the spool directory, then
// exit. A single background `posthook worker` drains the spool into the store,
// so all the heavy work happens once, in one process, off the critical path.
//
// Each event is its own file written via temp-file + atomic rename, so:
//   - concurrent hooks never interleave or corrupt each other's writes,
//   - a crash mid-write leaves a stray *.tmp (ignored), never a half record,
//   - the worker either sees a whole record or nothing.
package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bilanc/posthook/internal/atomicfs"
	"github.com/bilanc/posthook/internal/paths"
	"github.com/google/uuid"
)

// Envelope is one spooled agent event: the raw hook payload plus the context
// the worker can't reconstruct later (the agent slug and the cwd at fire time,
// since the worker runs in a different working directory).
// Payload is the raw hook stdin verbatim. It's a []byte (not json.RawMessage)
// so encoding/json base64-encodes it: the envelope stays valid JSON even when
// the agent sends an empty body or non-JSON text, and the bytes round-trip
// exactly.
type Envelope struct {
	Agent      string `json:"agent"`
	Cwd        string `json:"cwd"`
	ReceivedAt string `json:"received_at"`
	Payload    []byte `json:"payload"`
}

// Dir is the spool directory (~/.posthook/spool).
func Dir() string { return filepath.Join(paths.PosthookDir(), "spool") }

// Write atomically drops one envelope into the spool. The filename sorts by
// arrival (ReceivedAt is an RFC3339Nano timestamp) so the worker drains roughly
// in order; a uuid suffix keeps same-instant fires from colliding.
func Write(env Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	name := env.ReceivedAt + "-" + uuid.NewString() + ".json"
	// Keep the name filesystem-safe (RFC3339Nano contains ':').
	name = strings.ReplaceAll(name, ":", "")
	return atomicfs.Write(filepath.Join(Dir(), name), data, 0o644)
}

// Drain processes every complete record currently in the spool, oldest first,
// calling process for each. A record is removed only after process returns nil,
// so a transient failure (e.g. DB locked) leaves it for the next pass. A record
// that can't be parsed is removed — it can never succeed and would otherwise
// wedge the spool forever. Returns the number of records successfully handled.
func Drain(process func(Envelope) error) (int, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		// Skip in-flight temp files from atomicfs.Write; only take records.
		if !strings.HasSuffix(n, ".json") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	handled := 0
	for _, n := range names {
		path := filepath.Join(Dir(), n)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // another drainer took it
			}
			return handled, err
		}
		var env Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			// Unparseable record: can never succeed. Drop it so it doesn't
			// wedge the spool, and keep going.
			_ = os.Remove(path)
			continue
		}
		if err := process(env); err != nil {
			// Leave the record for a later pass; surface the error so the
			// worker can log and back off rather than spin.
			return handled, err
		}
		_ = os.Remove(path)
		handled++
	}
	return handled, nil
}

// Pending reports how many records are waiting in the spool. Used by status
// output and the worker's idle check.
func Pending() (int, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n, nil
}

// CommitJob asks the worker to record commits the git shadow saw were about
// to be pushed and that posthook may never have seen being made: made by a
// git that does not go through the shadow, or by a rebase, a merge or a
// cherry-pick. SHAs are oldest first. NotesRemote is the remote the shadow
// sent attribution notes to after that push, empty when it sent none: the
// worker sends them there again if recording the commits changes them.
type CommitJob struct {
	RepoRoot    string   `json:"repo_root"`
	SHAs        []string `json:"shas"`
	NotesRemote string   `json:"notes_remote,omitempty"`
	ReceivedAt  string   `json:"received_at"`
}

// CommitJobsDir is where commit jobs wait (~/.posthook/spool/commits). A
// directory of their own, not another kind of Envelope: Drain skips
// directories, so a worker from an older binary that is still running never
// picks one up and stores it as an agent event.
func CommitJobsDir() string { return filepath.Join(Dir(), "commits") }

// WriteCommitJob atomically drops one job into the commit spool.
func WriteCommitJob(job CommitJob) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	name := strings.ReplaceAll(job.ReceivedAt+"-"+uuid.NewString()+".json", ":", "")
	return atomicfs.Write(filepath.Join(CommitJobsDir(), name), data, 0o644)
}

// DrainCommitJobs processes every job in the commit spool, oldest first, with
// the same rules as Drain: a job is removed once process returns nil, left
// for the next pass on an error, and dropped if it cannot be parsed.
func DrainCommitJobs(process func(CommitJob) error) (int, error) {
	names, err := jsonFiles(CommitJobsDir())
	if err != nil {
		return 0, err
	}
	handled := 0
	for _, n := range names {
		path := filepath.Join(CommitJobsDir(), n)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return handled, err
		}
		var job CommitJob
		if err := json.Unmarshal(data, &job); err != nil {
			_ = os.Remove(path)
			continue
		}
		if err := process(job); err != nil {
			return handled, err
		}
		_ = os.Remove(path)
		handled++
	}
	return handled, nil
}

// PendingCommitJobs reports how many commit jobs are waiting.
func PendingCommitJobs() (int, error) {
	names, err := jsonFiles(CommitJobsDir())
	return len(names), err
}

// jsonFiles lists the complete records in a spool directory, sorted by name
// (arrival order). A missing directory is an empty spool.
func jsonFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
