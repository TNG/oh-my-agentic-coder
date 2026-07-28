package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
)

// maxLineBytes bounds a single audit line so a corrupt file cannot make
// the reader allocate without limit. Individual events are small; 4 MiB
// is far above any legitimate line.
const maxLineBytes = 4 << 20

// ReadFile decodes the audit trail at path into events. A non-existent
// file is reported via the returned error (os.IsNotExist detectable), so
// callers can distinguish "no trail yet" from a decode problem.
func ReadFile(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadLog(f)
}

// ReadLog decodes a JSON Lines audit trail. It is the read counterpart of
// the file sink and shares this package's Event schema, so reader and
// writer never drift. Blank lines are skipped. A malformed line is
// skipped rather than aborting the read: the trail is appended to
// concurrently, so its final line may be partially written when a reader
// opens it, and one bad line must not hide an otherwise valid trail.
func ReadLog(r io.Reader) ([]Event, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	var out []Event
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, sc.Err()
}

// LastRunID returns the run_id of the most recent run in events (the
// run_id of the last event that carries one), or "" when none do. Events
// are appended in run order, so the trailing run_id identifies the latest
// run.
func LastRunID(events []Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].RunID != "" {
			return events[i].RunID
		}
	}
	return ""
}
