//go:build vuln

package audit

import (
	"strings"
	"sync"
	"testing"
)

// countingWriter counts the number of times Write is called and records
// each chunk's length, without touching a real file — the seam newFileSink
// exists for.
type countingWriter struct {
	mu     sync.Mutex
	writes int
	sizes  []int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	c.sizes = append(c.sizes, len(p))
	return len(p), nil
}

// TestSecurityAuditRecordIsWrittenAtomically asserts that one audit record
// reaches its underlying writer as a single Write call — not a body write
// followed by a separate newline write.
//
// fileSink.write always performs w.Write(line) and w.WriteByte('\n') as two
// calls. bufio.Writer only batches them into one underlying syscall while
// both fit in its buffer (4096 bytes by default); past that threshold, the
// body flushes on its own and the newline becomes a second, independent
// write. Two sinks on the same file description can then interleave a
// whole second record between a first sink's body and its trailing
// newline, gluing two records into one malformed physical line.
func TestSecurityAuditRecordIsWrittenAtomically(t *testing.T) {
	// Control: a small record — well under the buffer threshold — is
	// delivered as a single underlying Write, so the mechanism itself
	// (and this test's counting) is sound.
	small := &countingWriter{}
	sink := newFileSink(nil, small)
	if err := sink.write([]byte(`{"probe":"small"}`)); err != nil {
		t.Fatalf("control: write: %v", err)
	}
	if small.writes != 1 {
		t.Fatalf("control: a small record produced %d underlying writes, want 1: the fixture is broken, not the security property", small.writes)
	}

	big := &countingWriter{}
	sink = newFileSink(nil, big)
	hostilePath := "/" + strings.Repeat("a", 8192)
	line, err := marshalLine(FacadeRequest("GET", "m", "ns", hostilePath, 200, 0, 1))
	if err != nil {
		t.Fatalf("marshalLine: %v", err)
	}
	if err := sink.write(line); err != nil {
		t.Fatalf("write: %v", err)
	}

	if big.writes != 1 {
		t.Errorf("a %d-byte record produced %d underlying Write calls (sizes %v), not 1: "+
			"the body and its trailing newline reach the file as separate syscalls, opening a window for another writer to interleave a whole record between them",
			len(line), big.writes, big.sizes)
	}
}
