package spool

import (
	"errors"
	"testing"
)

// setHome points paths.PosthookDir() at a temp dir for the duration of a test.
func setHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestWriteThenDrainRoundTrips(t *testing.T) {
	setHome(t)

	want := Envelope{Agent: "claude-code", Cwd: "/repo", ReceivedAt: "2026-01-01T00:00:00Z", Payload: []byte(`{"hook_event_name":"PostToolUse"}`)}
	if err := Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n, err := Pending(); err != nil || n != 1 {
		t.Fatalf("Pending = %d, %v; want 1, nil", n, err)
	}

	var got []Envelope
	n, err := Drain(func(e Envelope) error {
		got = append(got, e)
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("Drain = %d, %v; want 1, nil", n, err)
	}
	if len(got) != 1 || got[0].Agent != want.Agent || got[0].Cwd != want.Cwd || string(got[0].Payload) != string(want.Payload) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got[0], want)
	}
	if pending, _ := Pending(); pending != 0 {
		t.Fatalf("spool not emptied after successful drain: %d pending", pending)
	}
}

func TestDrainEmptyPayload(t *testing.T) {
	setHome(t)
	// An empty payload (the misfire case) must still round-trip — []byte
	// base64-encodes, so the envelope stays valid JSON.
	if err := Write(Envelope{Agent: "cursor", ReceivedAt: "t"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	n, err := Drain(func(e Envelope) error {
		if len(e.Payload) != 0 {
			t.Fatalf("expected empty payload, got %q", e.Payload)
		}
		return nil
	})
	if err != nil || n != 1 {
		t.Fatalf("Drain = %d, %v; want 1, nil", n, err)
	}
}

func TestDrainLeavesRecordOnProcessError(t *testing.T) {
	setHome(t)
	if err := Write(Envelope{Agent: "codex", ReceivedAt: "t", Payload: []byte("{}")}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	boom := errIngest
	n, err := Drain(func(e Envelope) error { return boom })
	if n != 0 || err != boom {
		t.Fatalf("Drain = %d, %v; want 0, boom", n, err)
	}
	// Record must survive so a later worker pass can retry it.
	if pending, _ := Pending(); pending != 1 {
		t.Fatalf("record not retained after process error: %d pending", pending)
	}
}

var errIngest = &drainErr{}

type drainErr struct{}

func (*drainErr) Error() string { return "ingest failed" }

func TestCommitJobsRoundTripInTheirOwnSpool(t *testing.T) {
	setHome(t)

	want := CommitJob{RepoRoot: "/repo", SHAs: []string{"aaa", "bbb"}, ReceivedAt: "2026-01-01T00:00:00Z"}
	if err := WriteCommitJob(want); err != nil {
		t.Fatalf("WriteCommitJob: %v", err)
	}
	if n, err := PendingCommitJobs(); err != nil || n != 1 {
		t.Fatalf("PendingCommitJobs = %d, %v; want 1, nil", n, err)
	}

	// The event spool does not see it: a worker from an older binary drains
	// only events, and would store a job it found there as an agent event.
	if n, _ := Pending(); n != 0 {
		t.Fatalf("a commit job shows as %d pending event(s)", n)
	}
	if n, err := Drain(func(Envelope) error { t.Fatal("a commit job was handed out as an event"); return nil }); err != nil || n != 0 {
		t.Fatalf("Drain = %d, %v; want 0, nil", n, err)
	}

	var got []CommitJob
	n, err := DrainCommitJobs(func(j CommitJob) error { got = append(got, j); return nil })
	if err != nil || n != 1 {
		t.Fatalf("DrainCommitJobs = %d, %v; want 1, nil", n, err)
	}
	if len(got) != 1 || got[0].RepoRoot != want.RepoRoot || len(got[0].SHAs) != 2 || got[0].SHAs[1] != "bbb" {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
	if left, _ := PendingCommitJobs(); left != 0 {
		t.Fatalf("%d job(s) left after a successful drain", left)
	}
}

func TestCommitJobStaysOnProcessError(t *testing.T) {
	setHome(t)
	if n, err := DrainCommitJobs(func(CommitJob) error { return nil }); err != nil || n != 0 {
		t.Fatalf("draining an empty commit spool = %d, %v; want 0, nil", n, err)
	}
	if err := WriteCommitJob(CommitJob{RepoRoot: "/repo", SHAs: []string{"aaa"}, ReceivedAt: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := DrainCommitJobs(func(CommitJob) error { return errors.New("store is locked") }); err == nil {
		t.Fatal("expected the process error back")
	}
	if left, _ := PendingCommitJobs(); left != 1 {
		t.Fatalf("job should wait for the next pass, %d left", left)
	}
}
