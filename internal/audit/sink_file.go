package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// sink is one destination for serialized events. A sink receives the
// already-marshaled JSON line (without trailing newline) so the envelope
// and redaction are applied exactly once, upstream.
type sink interface {
	// write appends one event line. It returns an error only for the
	// authoritative (file) sink; ancillary sinks (syslog) swallow their
	// own errors and never fail the write path.
	write(line []byte) error
	close() error
}

// fileSink is the authoritative append-only JSON Lines sink. It opens the
// file O_APPEND|O_CREATE|O_WRONLY with mode 0600 and flushes after every
// event to bound loss on crash (volume is human-scale).
type fileSink struct {
	mu     sync.Mutex
	f      *os.File
	w      *bufio.Writer
	broken bool
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
	return &fileSink{f: f, w: bufio.NewWriter(f)}, nil
}

func (s *fileSink) write(line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken {
		return errBroken
	}
	if _, err := s.w.Write(line); err != nil {
		s.broken = true
		return err
	}
	if err := s.w.WriteByte('\n'); err != nil {
		s.broken = true
		return err
	}
	if err := s.w.Flush(); err != nil {
		s.broken = true
		return err
	}
	return nil
}

func (s *fileSink) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	_ = s.w.Flush()
	err := s.f.Close()
	s.f = nil
	return err
}

// errBroken marks a sink that has already failed once (fail-open path).
var errBroken = fmt.Errorf("audit: sink previously failed")

// marshalLine renders an event to a compact single-line JSON byte slice
// (no trailing newline; the sink adds it).
func marshalLine(ev Event) ([]byte, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, err
	}
	return b, nil
}
