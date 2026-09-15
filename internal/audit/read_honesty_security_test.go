//go:build vuln

package audit

import (
	"strings"
	"testing"
)

// TestSecurityReadLogReportsMalformedRecords asserts that ReadLog does not
// silently swallow a malformed line that is NOT the trailing line of the
// file — as opposed to a partially-written final line, which is the
// legitimate case the current silent-skip behaviour exists to tolerate.
//
// Two independent sinks writing >4KiB records to the same path (the parent
// process and a sandboxed subprocess both auditing to one file) can each
// perform the body-write and the newline-write as separate syscalls; an
// unlucky interleaving glues one sink's trailing newline onto the middle of
// the other's record, producing one malformed physical line with a valid
// record on either side. json.Unmarshal fails on it and ReadLog moves on
// with no signal at all — so a corrupted trail reads as a complete, valid
// one. Reproduced under load: 3748 of 6000 records vanished this way
// (internal/audit is Go's `sync.Mutex`-per-sink, not lock-free-safe across
// two independent processes).
func TestSecurityReadLogReportsMalformedRecords(t *testing.T) {
	// Control: a malformed TRAILING line — the legitimate case, a reader
	// opening the file mid-append — must still be tolerated with no error,
	// or every ordinary concurrent read would start failing.
	trailing := strings.Join([]string{
		`{"run_id":"r1","type":"session.start"}`,
		`{"run_id":"r1","type":"net.decisi`, // cut off mid-record
	}, "\n")
	events, err := ReadLog(strings.NewReader(trailing))
	if err != nil {
		t.Fatalf("control: a malformed trailing line was reported as an error (%v): the fixture is broken, not the security property", err)
	}
	if len(events) != 1 {
		t.Fatalf("control: expected exactly 1 valid event before the trailing partial line, got %d", len(events))
	}

	// A malformed line in the MIDDLE of the file — two complete JSON
	// objects glued with no separator, exactly what the two-syscall split
	// produces — followed by a further valid record.
	glued := strings.Join([]string{
		`{"run_id":"r1","type":"session.start"}`,
		`{"run_id":"r1","type":"net.decision"}{"run_id":"r1","type":"net.decision"}`,
		`{"run_id":"r1","type":"control.mutation"}`,
	}, "\n")

	events, err = ReadLog(strings.NewReader(glued))
	if len(events) != 2 {
		t.Fatalf("expected the two well-formed records around the corruption to survive regardless (got %d): %+v", len(events), events)
	}
	if err == nil {
		t.Errorf("ReadLog silently dropped a malformed NON-trailing line (two records glued together) with no error and no signal at all: " +
			"a caller reading the trail cannot tell a corrupted (records missing) audit log from a complete one")
	}
}
