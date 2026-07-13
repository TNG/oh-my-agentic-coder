//go:build !windows

package audit

import (
	"log/syslog"
)

// syslogSink mirrors events to the system log. It is best-effort: a
// syslog failure never fails the write path and never triggers strict-mode
// abort (the authoritative file sink already recorded the event).
type syslogSink struct {
	w *syslog.Writer
}

// openSyslogSink connects to the local syslog daemon under AUTHPRIV/NOTICE
// with the "omac" tag. AUTHPRIV is the conventional facility for
// security-relevant events.
func openSyslogSink() (*syslogSink, error) {
	w, err := syslog.New(syslog.LOG_AUTHPRIV|syslog.LOG_NOTICE, "omac")
	if err != nil {
		return nil, err
	}
	return &syslogSink{w: w}, nil
}

func (s *syslogSink) write(line []byte) error {
	// Swallow errors: syslog is ancillary. Returning nil keeps it from
	// tripping strict-mode fatal handling.
	_ = s.w.Notice(string(line))
	return nil
}

func (s *syslogSink) close() error {
	if s.w == nil {
		return nil
	}
	err := s.w.Close()
	s.w = nil
	return err
}
