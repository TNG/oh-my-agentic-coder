package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadLogSkipsBlankAndMalformedLines(t *testing.T) {
	in := strings.Join([]string{
		`{"ts":"2026-07-18T10:00:00Z","run_id":"r1","type":"session.start"}`,
		``,
		`{ not json`,
		`{"ts":"2026-07-18T10:00:01Z","run_id":"r1","type":"net.decision","host":"a.example","allow":false}`,
		`   `,
	}, "\n")

	events, err := ReadLog(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 valid events (blank+malformed skipped), got %d: %+v", len(events), events)
	}
	if events[1].Type != TypeNetDecision || events[1].Host != "a.example" {
		t.Fatalf("net.decision decoded wrong: %+v", events[1])
	}
	if events[1].Allow == nil || *events[1].Allow {
		t.Fatalf("allow pointer should decode to false, got %v", events[1].Allow)
	}
}

func TestReadFileMissingIsDetectable(t *testing.T) {
	_, err := ReadFile(filepath.Join(t.TempDir(), "nope.jsonl"))
	if !os.IsNotExist(err) {
		t.Fatalf("want IsNotExist, got %v", err)
	}
}

func TestLastRunID(t *testing.T) {
	events := []Event{
		{RunID: "r1", Type: TypeSessionStart},
		{RunID: "r1", Type: TypeNetDecision},
		{RunID: "r2", Type: TypeSessionStart},
		{Type: "something-without-run"},
	}
	if got := LastRunID(events); got != "r2" {
		t.Fatalf("LastRunID = %q, want r2", got)
	}
	if got := LastRunID(nil); got != "" {
		t.Fatalf("LastRunID(nil) = %q, want empty", got)
	}
}

// Round-trip guard: what the file sink writes, ReadLog must decode. This is
// the test that catches reader/writer schema drift.
func TestReadLogRoundTripsWrittenTrail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	a, err := New(Config{Enabled: true, Path: path, Mode: ModeStart})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.Emit(NetDecision("a.example", 443, false, "", "blocklist", false))
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var netDecisions int
	for _, ev := range events {
		if ev.Type == TypeNetDecision {
			netDecisions++
			if ev.Host != "a.example" || ev.Source != "blocklist" || ev.Allow == nil || *ev.Allow {
				t.Fatalf("round-tripped net.decision wrong: %+v", ev)
			}
		}
	}
	if netDecisions != 1 {
		t.Fatalf("want exactly 1 net.decision round-tripped, got %d", netDecisions)
	}
}
