package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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
// writer never drift. Blank lines are skipped. A malformed trailing line
// is tolerated silently (a reader may open the file while the final record
// is still being written). A malformed line in the middle of the file is
// reported via the returned error — it means the trail is corrupt — while
// the surrounding valid records are still returned.
func ReadLog(r io.Reader) ([]Event, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	// Collect all raw lines first so we know which one is the last.
	var rawLines [][]byte
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		rawLines = append(rawLines, cp)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	var (
		out        []Event
		corruptErr error
	)
	for i, line := range rawLines {
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			if i == len(rawLines)-1 {
				// Trailing partial line: tolerate silently.
				continue
			}
			if corruptErr == nil {
				corruptErr = fmt.Errorf("audit: corrupt record at line %d (trail may have missing events)", i+1)
			}
			continue
		}
		out = append(out, ev)
	}
	return out, corruptErr
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
