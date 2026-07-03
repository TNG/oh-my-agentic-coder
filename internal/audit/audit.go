package audit

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Config controls how New builds an Auditor.
type Config struct {
	// Enabled turns auditing on. When false, New returns Nop().
	Enabled bool
	// Path is the audit log file path. Empty means "use DefaultPath".
	Path string
	// Syslog additionally mirrors events to the system log (Unix only).
	Syslog bool
	// Strict makes the file sink fail-closed: a write failure invokes
	// Fatal instead of degrading to a stderr warning.
	Strict bool
	// Mode identifies the entrypoint (start|serve) stamped on every event.
	Mode Mode
	// Version is stamped on session.start.
	Version string
	// SecretValues seeds the redactor so a secret that leaks onto argv is
	// scrubbed. The always-safe path is passing secret NAMES, not values.
	SecretValues []string
	// Fatal is invoked (once) when a strict-mode write fails after
	// startup. It SHOULD run teardown and exit non-zero. When nil, a
	// strict write failure falls back to os.Exit via the default handler.
	Fatal func(error)
	// Warnf receives one-time warnings (fail-open path, syslog open
	// failure, non-persistent fallback dir). Defaults to stderr.
	Warnf func(format string, args ...any)
}

// auditor is the live Auditor: envelope stamping + redaction + fan-out to
// sinks. The file sink is authoritative; syslog is ancillary.
type auditor struct {
	runID    string
	mode     Mode
	pid      int
	seq      seqCounter
	red      *redactor
	strict   bool
	fatal    func(error)
	warnf    func(string, ...any)
	warnOnce sync.Once

	fileSink   *fileSink
	syslogSink *syslogSink

	fatalOnce sync.Once
}

// New builds an Auditor from cfg. When disabled it returns Nop(). When the
// file sink cannot be opened it returns an error (so strict callers can
// abort before launching the inner command); non-strict callers may choose
// to log and fall back to Nop() themselves.
func New(cfg Config) (Auditor, error) {
	if !cfg.Enabled {
		return Nop(), nil
	}
	warnf := cfg.Warnf
	if warnf == nil {
		warnf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "omac audit: "+format+"\n", args...)
		}
	}

	path := cfg.Path
	if path == "" {
		p, notGuaranteed := DefaultPath()
		path = p
		if notGuaranteed {
			warnf("no home dir resolved; audit log at %s may not persist", path)
		}
	}

	fs, err := openFileSink(path)
	if err != nil {
		return nil, err
	}

	a := &auditor{
		runID:    newRunID(),
		mode:     cfg.Mode,
		pid:      os.Getpid(),
		red:      newRedactor(cfg.SecretValues),
		strict:   cfg.Strict,
		fatal:    cfg.Fatal,
		warnf:    warnf,
		fileSink: fs,
	}

	if cfg.Syslog {
		ss, serr := openSyslogSink()
		if serr != nil {
			warnf("syslog mirror unavailable: %v", serr)
		} else {
			a.syslogSink = ss
		}
	}
	return a, nil
}

// Emit stamps the envelope, redacts, and writes to every sink.
func (a *auditor) Emit(ev Event) {
	ev.Ts = nowRFC3339Nano()
	ev.RunID = a.runID
	ev.Seq = a.seq.next()
	ev.Mode = a.mode
	ev.PID = a.pid
	ev = a.red.apply(ev)

	line, err := marshalLine(ev)
	if err != nil {
		// A marshal failure means a programming error in an event; never
		// fatal, but warn once so it's noticed.
		a.warnOnce.Do(func() { a.warnf("failed to marshal event %q: %v", ev.Type, err) })
		return
	}

	// Authoritative file sink first.
	if err := a.fileSink.write(line); err != nil {
		a.onWriteError(err)
		// In fail-open mode we still attempt syslog below.
	}
	if a.syslogSink != nil {
		_ = a.syslogSink.write(line)
	}
}

// onWriteError applies the selected failure mode to a file-sink error.
func (a *auditor) onWriteError(err error) {
	if err == errBroken {
		return // already handled once
	}
	if a.strict {
		a.fatalOnce.Do(func() {
			if a.fatal != nil {
				a.fatal(fmt.Errorf("audit: strict-mode write failed: %w", err))
				return
			}
			fmt.Fprintf(os.Stderr, "omac audit: strict-mode write failed: %v\n", err)
			os.Exit(1)
		})
		return
	}
	a.warnOnce.Do(func() {
		a.warnf("write failed (%v); further audit writes this run are skipped", err)
	})
}

func (a *auditor) Close() error {
	var firstErr error
	if a.fileSink != nil {
		if err := a.fileSink.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if a.syslogSink != nil {
		_ = a.syslogSink.close()
	}
	return firstErr
}

// --- no-op auditor ---

type nopAuditor struct{}

// Nop returns an Auditor that discards every event. Used when auditing is
// disabled (or, at the caller's discretion, when a non-strict sink failed
// to open), so every emit point can hold a non-nil Auditor.
func Nop() Auditor { return nopAuditor{} }

func (nopAuditor) Emit(Event)   {}
func (nopAuditor) Close() error { return nil }

// newRunID mints a per-invocation random hex identifier. On the vanishingly
// unlikely rand failure it falls back to a time-based value.
func newRunID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "run_" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))[:16]
	}
	return "run_" + hex.EncodeToString(b[:])
}

// ensure io is used (kept for future streaming sinks); avoids churn if a
// build tag ever trims a sink file.
var _ = io.Discard
