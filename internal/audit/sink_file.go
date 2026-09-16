package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// fileSink is the authoritative append-only JSON Lines sink. It opens the
// file O_APPEND|O_CREATE|O_WRONLY with mode 0600. Each record is written
// as a single Write call (body + newline in one buffer) so concurrent
// O_APPEND writers on the same file cannot interleave within a record.
type fileSink struct {
	mu sync.Mutex
	f  *os.File
	w  io.Writer // non-nil in tests that inject a fake writer; nil means use f
}

// openFileSink creates the parent dir (0700), opens the file (0600), and
// returns the sink. A failure here is fatal for strict mode and reported
// to the caller.
func openFileSink(path string) (*fileSink, error) {
	if err := ensureDir(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	return newFileSink(f, nil), nil
}

// newFileSink builds a sink around an arbitrary io.Writer, closing f (if
// non-nil) on close. When w is nil, writes go to f directly (production path).
// Tests pass a non-nil w to observe the exact number of Write calls.
func newFileSink(f *os.File, w io.Writer) *fileSink {
	return &fileSink{f: f, w: w}
}

func (s *fileSink) write(line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Append newline in the same buffer so body+newline reach the kernel as
	// one write(2) call. On O_APPEND files this is atomic up to PIPE_BUF.
	buf := make([]byte, len(line)+1)
	copy(buf, line)
	buf[len(line)] = '\n'
	w := s.w
	if w == nil {
		w = s.f
	}
	_, err := w.Write(buf)
	return err
}

func (s *fileSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// marshalLine renders an event to a compact single-line JSON byte slice
// (no trailing newline; the sink adds it).
func marshalLine(ev Event) ([]byte, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	return b, nil
}
