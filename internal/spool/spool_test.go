package spool

import (
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
