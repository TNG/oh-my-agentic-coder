package sandboxrun

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// diagSink decides where runtime diagnostics go. Fatal errors always
// go to stderr; everything that can fire *after* the inner TUI has
// taken over the terminal (proxy decisions, prompt notices, path-skip
// notices) goes to a log file when stderr is a terminal, so the
// sandbox never corrupts the harness's TUI drawing.
type diagSink struct {
	mu     sync.Mutex
	w      io.Writer
	file   *os.File // non-nil when logging to a file
	path   string   // log file path ("" when writing to stderr)
	stderr io.Writer
}

// newDiagSink picks the diagnostics destination. When stderr is not a
// terminal (CI, pipes, tests) diagnostics stay on stderr. When it is a
// terminal, they go to ~/.local/state/omac/sandbox.log (created with
// O_APPEND so concurrent sandboxes interleave whole lines).
func newDiagSink(stderr io.Writer) *diagSink {
	d := &diagSink{w: stderr, stderr: stderr}
	f, isTTY := stderr.(*os.File)
	if !isTTY || !term.IsTerminal(int(f.Fd())) {
		return d
	}
	path, err := defaultDiagLogPath()
	if err != nil {
		return d
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return d
	}
	lf, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return d
	}
	d.w = lf
	d.file = lf
	d.path = path
	return d
}

// DiagLogPath returns the path of the sandbox runtime diagnostics log
// (~/.local/state/omac/sandbox.log), where per-connection allow/deny
// decisions are recorded during TTY sessions. `omac diagnose` uses it to
// point users at the raw human-readable log.
func DiagLogPath() (string, error) { return defaultDiagLogPath() }

func defaultDiagLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "omac", "sandbox.log"), nil
}

// Logf writes one diagnostic line (file or stderr, per sink mode),
// prefixed with a timestamp and aligned level/category columns:
//
//	2026-06-11 10:42:01 INFO  net   DENY tracker.example:443 (deny_domain)
//
// Messages from the proxy/prompt arrive with an "omac sandbox: "
// prefix and embedded category words; normalize them into columns.
func (d *diagSink) Logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	level, category, rest := classifyDiag(msg)
	d.mu.Lock()
	defer d.mu.Unlock()
	fmt.Fprintf(d.w, "%s %-5s %-5s %s\n",
		time.Now().Format("2006-01-02 15:04:05"), level, category, rest)
}

// classifyDiag derives (level, category, message) columns from the
// free-form diagnostic strings the subsystems emit.
func classifyDiag(msg string) (level, category, rest string) {
	rest = strings.TrimPrefix(msg, "omac sandbox: ")
	switch {
	case strings.HasPrefix(rest, "net "):
		return "INFO", "net", strings.TrimPrefix(rest, "net ")
	case strings.HasPrefix(rest, "warning: "):
		return "WARN", "core", strings.TrimPrefix(rest, "warning: ")
	case strings.HasPrefix(rest, "WARNING: "):
		return "WARN", "core", strings.TrimPrefix(rest, "WARNING: ")
	case strings.HasPrefix(rest, "notice: "):
		return "INFO", "fs", strings.TrimPrefix(rest, "notice: ")
	case strings.Contains(rest, "prompt"):
		return "INFO", "ask", rest
	default:
		return "INFO", "core", rest
	}
}

// Writer exposes the sink as an io.Writer (for ResolveGrants notices).
func (d *diagSink) Writer() io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			d.Logf("%s", line)
		}
		return len(p), nil
	})
}

// AnnouncePath prints a single pre-launch pointer to the log file on
// stderr (before the TUI starts), so users know where diagnostics go.
func (d *diagSink) AnnouncePath() {
	if d.path != "" {
		fmt.Fprintf(d.stderr, "omac sandbox: diagnostics -> %s\n", d.path)
	}
}

// Close releases the log file (if any).
func (d *diagSink) Close() {
	if d.file != nil {
		_ = d.file.Close()
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
