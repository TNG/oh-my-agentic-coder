//go:build windows

package audit

import "fmt"

// syslogSink is unavailable on Windows; openSyslogSink always errors so the
// constructor logs a warning and continues with the file sink only.
type syslogSink struct{}

func openSyslogSink() (*syslogSink, error) {
	return nil, fmt.Errorf("audit: syslog not supported on windows")
}

func (s *syslogSink) write(line []byte) error { return nil }
func (s *syslogSink) close() error            { return nil }
