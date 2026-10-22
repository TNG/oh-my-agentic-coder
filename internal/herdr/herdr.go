// Package herdr bridges an omac launch to herdr, a terminal session
// manager (https://herdr.dev), from the host side.
//
// omac deliberately strips the environment before launching the inner
// harness, so herdr's own in-harness integrations cannot report from
// inside the sandbox: they never see HERDR_* and cannot reach the herdr
// socket. This package runs in the omac process itself, which sits
// outside the sandbox and does see the herdr environment, and reports
// on the harness's behalf:
//
//   - pane identity (the pane runs "omac", not a bare harness),
//   - a working/idle state derived from host-side signals,
//   - the session id and a resume command, so a herdr server restart
//     restores the pane through `omac continue ... -s <id>` and the
//     session comes back INSIDE the sandbox instead of as a bare
//     harness on the host,
//   - a release when the inner command exits.
//
// The herdr socket is never passed into the sandbox: its pane API can
// drive other panes (send input), so it must stay outside the reach of
// the sandboxed agent. All reports are fail-soft. They are sent from a
// background goroutine with a short timeout and ignored on failure, so
// a missing or hung herdr binary can never affect an omac launch or
// its exit code.
package herdr

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Report states. omac never claims herdr's "blocked" state: it has no
// precise host-side signal for a permission prompt, and a wrong
// "blocked" is worse than herdr's own fallback. Screen-based detection
// covers that case until a precise signal exists.
const (
	StateIdle    = "idle"
	StateWorking = "working"
)

// source identifies omac's integration to herdr. herdr requires a
// stable, unique source that does not start with "herdr:" (that prefix
// is reserved for herdr's own integrations).
const source = "omac"

// Defaults for the bridge loop. The poll interval trades state freshness
// against stat cost; both are tiny (a handful of stat calls per tick).
// quiet is how long the watched session store must be unchanged before
// an active session is reported idle. Harnesses append to their session
// store as a turn streams, but a long-running tool (a build, a test
// suite) can legitimately write nothing for a while; 60s keeps such gaps
// from flapping the state to idle and back.
const (
	defaultPoll    = 2 * time.Second
	defaultQuiet   = 60 * time.Second
	reportTimeout  = 2 * time.Second
	releaseTimeout = 2 * time.Second
)

// Detected is the herdr pane identity of the current omac process.
type Detected struct {
	PaneID  string
	BinPath string
}

// Detect reads the herdr pane environment. It reports ok=false whenever
// omac does not run inside a herdr pane, or the variables look wrong:
// the bridge then stays off, which is always safe. getenv is injected
// so callers and tests can pass a narrowed environment.
func Detect(getenv func(string) string) (Detected, bool) {
	if getenv("HERDR_ENV") != "1" {
		return Detected{}, false
	}
	id, bin := getenv("HERDR_PANE_ID"), getenv("HERDR_BIN_PATH")
	if !validPaneID(id) || !validBinPath(bin) {
		return Detected{}, false
	}
	return Detected{PaneID: id, BinPath: bin}, true
}

// validPaneID accepts the identifiers herdr uses for panes. The value is
// passed as one argv element, never through a shell, so the only real
// hazard is an id that herdr's CLI could mistake for a flag: a leading
// "-" is therefore rejected, as are control characters (which herdr's
// resume-command rules forbid elsewhere). Everything else, including
// colons herdr uses in composite pane ids, is passed through verbatim.
func validPaneID(id string) bool {
	if id == "" || len(id) > 128 || strings.HasPrefix(id, "-") {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// validBinPath requires an absolute path. herdr sets an absolute
// HERDR_BIN_PATH; anything else means the variable was not set by
// herdr, and running it would execute an attacker-chosen binary.
func validBinPath(bin string) bool {
	return bin != "" && filepath.IsAbs(bin) && !strings.ContainsRune(bin, 0)
}

// validToken reports whether s is a non-empty run of letters, digits,
// dot, underscore, or hyphen, at most max bytes long, and does not
// start with punctuation (a leading hyphen would read as a flag).
func validToken(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return s[0] != '-' && s[0] != '.' && s[0] != '_'
}

// ValidSessionID guards ids that may be embedded in a resume command.
// herdr rejects resume argv containing apostrophes or control
// characters, and omac resumes by passing the id as one argv element,
// so the same charset as omac's own session ids applies (see
// internal/session: alphanumeric plus . _ -, never leading
// punctuation). A claude sidechain file ("sidechain-...") names a
// sub-agent session, not one the user can resume, so those are
// rejected here as well.
func ValidSessionID(id string) bool {
	if !validToken(id, 256) || strings.HasPrefix(id, "sidechain-") {
		return false
	}
	return true
}

// runner executes one herdr CLI call. Injected so tests can record argv
// instead of spawning processes.
type runner func(ctx context.Context, argv []string) error

// execRunner runs the herdr binary via argv with the caller's deadline.
// herdr's reports must never touch the terminal the inner harness owns,
// so output is discarded; failures are the caller's to ignore.
func execRunner(ctx context.Context, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	return cmd.Run()
}

// Probe is the host-side activity signal for the bridge: it reports
// whether the watched harness state changed since the previous call,
// and any newly discovered session ids. Implementations are called only
// from the bridge goroutine, so they need no locking of their own.
type Probe interface {
	// Probe advances the snapshot and returns changed=true when the
	// watched state differs from the previous call. The FIRST call
	// establishes the baseline and must report no change (the launch
	// itself counts as activity, so the bridge starts "working").
	// ids lists session ids discovered since the previous call.
	Probe() (changed bool, ids []string)
}

// Options configures a Bridge.
type Options struct {
	PaneID  string
	BinPath string

	// Agent is the label users see in herdr's sidebar.
	Agent string

	// Probe is the activity signal. The bridge is only useful with one;
	// Start is a no-op without it.
	Probe Probe

	// PollInterval and Quiet control the loop cadence and the
	// working→idle threshold. Zero picks the package defaults.
	PollInterval time.Duration
	Quiet        time.Duration

	// SessionID is polled each tick. It returns "" until the session id
	// is known, then a stable id (resume by id, opencode's control-plane
	// report, or a store discovery). Once non-empty the bridge attaches
	// it and the resume command to the next report.
	SessionID func() string

	// ResumeArgv builds the resume command for a session id, or nil
	// when the id cannot be resumed. The returned argv follows herdr's
	// "--" separator; the first word must be a plain command on PATH.
	ResumeArgv func(id string) []string

	// Runner executes herdr CLI calls; nil uses the real binary.
	Runner runner

	// Logf receives best-effort diagnostics (verbose mode). Nil discards.
	Logf func(format string, args ...any)

	// Now overrides the clock in tests. Nil uses time.Now.
	Now func() time.Time
}

// Bridge reports an omac launch to a herdr pane. Start begins the report
// loop; Close stops it and releases the pane. The bridge never blocks
// the launch goroutine for more than the report timeout.
type Bridge struct {
	opts Options
	run  runner
	stop chan struct{}
	done chan struct{}
	// started records that Start began the loop. Close consults it so
	// it stays a no-op for a bridge that never started (e.g. Probe was
	// nil). Start and Close are called from the launch goroutine, never
	// concurrently with each other.
	started bool

	// Loop state, touched only by the loop goroutine (Close waits on
	// done before releasing, which orders the two).
	lastSeq       int64
	state         string
	reportedState string // state last sent to herdr; "" = never reported
	lastChange    time.Time
	sentID        string
}

// New builds a Bridge for the detected pane.
func New(det Detected, opts Options) *Bridge {
	if opts.Agent == "" {
		opts.Agent = "omac"
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPoll
	}
	if opts.Quiet <= 0 {
		opts.Quiet = defaultQuiet
	}
	if opts.Runner == nil {
		opts.Runner = execRunner
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	opts.PaneID = det.PaneID
	opts.BinPath = det.BinPath
	return &Bridge{opts: opts, run: opts.Runner, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start launches the report loop in the background. The first report
// claims the pane. herdr only accepts a resume command from a source
// that holds the pane, so the claim must precede the resume report.
func (b *Bridge) Start() {
	if b.opts.Probe == nil || b.started {
		return
	}
	b.started = true
	// Baseline the probe before the loop: its first Probe call must not
	// count as a change. The launch itself is activity.
	b.opts.Probe.Probe()
	b.state = StateWorking
	b.lastChange = b.opts.Now()
	go b.loop()
}

// Close stops the loop and releases the pane. herdr clears the agent
// name, state, and resume command on release, so the pane does not
// linger as "omac working" after the inner harness exits. If the
// release call does not finish within the timeout, Close returns
// anyway: herdr's own safety net clears the pane once it sees the shell
// prompt again. Idempotent, and a no-op for a bridge that never started.
func (b *Bridge) Close() {
	if !b.started {
		return
	}
	select {
	case <-b.stop:
		// Already closed.
		return
	default:
		close(b.stop)
	}
	<-b.done
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if err := b.run(ctx, b.releaseArgv()); err != nil {
		b.opts.Logf("herdr release failed (ignored): %v", err)
	}
}

func (b *Bridge) loop() {
	defer close(b.done)
	tick := time.NewTicker(b.opts.PollInterval)
	defer tick.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-tick.C:
			b.tick()
		}
	}
}

// tick runs one probe-and-report cycle. Reports come from this single
// goroutine only, which implements herdr's "send only the latest state"
// guidance structurally: while a slow report is in flight (bounded by
// the report timeout), later state changes coalesce into the next one.
func (b *Bridge) tick() {
	changed, discovered := b.discover()
	id := ""
	for _, cand := range discovered {
		if ValidSessionID(cand) {
			id = cand
			break
		}
	}
	if id == "" && b.opts.SessionID != nil {
		id = b.opts.SessionID()
	}
	if id != "" && !ValidSessionID(id) {
		id = ""
	}

	now := b.opts.Now()
	if changed {
		// Fresh writes in the session store mean the harness is
		// streaming a turn: working. Recording the timestamp even when
		// the state already was "working" keeps a busy session from
		// aging out mid-turn.
		b.state = StateWorking
		b.lastChange = now
	} else if b.state == StateWorking && now.Sub(b.lastChange) > b.opts.Quiet {
		// The store has been quiet long enough: the harness is back at
		// its prompt.
		b.state = StateIdle
	}

	if b.reportedState != "" && b.sentID == id && !b.stateChangedSinceReport() {
		return
	}
	b.sendReport(b.state, id)
}

// discover runs the probe and filters discovered candidates to valid
// session ids, oldest-first preserved.
func (b *Bridge) discover() (changed bool, ids []string) {
	if b.opts.Probe == nil {
		return false, nil
	}
	changed, ids = b.opts.Probe.Probe()
	if len(ids) == 0 {
		return changed, nil
	}
	keep := ids[:0]
	for _, id := range ids {
		if ValidSessionID(id) {
			keep = append(keep, id)
		}
	}
	return changed, keep
}

// sendReport issues one report-agent call, recording what was sent.
func (b *Bridge) sendReport(state, id string) {
	argv := []string{
		b.opts.BinPath, "pane", "report-agent", b.opts.PaneID,
		"--source", source,
		"--agent", b.opts.Agent,
		"--state", state,
		"--seq", strconv.FormatInt(b.nextSeq(), 10),
	}
	if id != "" {
		argv = append(argv, "--agent-session-id", id)
		if resume := b.opts.ResumeArgv(id); resume != nil {
			argv = append(argv, "--")
			argv = append(argv, resume...)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	if err := b.run(ctx, argv); err != nil {
		// Fail-soft by design: herdr is a convenience layer, never a
		// launch dependency.
		b.opts.Logf("herdr report failed (ignored): %v", err)
	}
	b.sentID = id
	b.reportedState = state
}

func (b *Bridge) releaseArgv() []string {
	return []string{
		b.opts.BinPath, "pane", "release-agent", b.opts.PaneID,
		"--source", source,
		"--agent", b.opts.Agent,
		"--seq", strconv.FormatInt(b.nextSeq(), 10),
	}
}

// stateChangedSinceReport reports whether the current state differs from
// the state last sent to herdr.
func (b *Bridge) stateChangedSinceReport() bool {
	return b.reportedState != b.state
}

// nextSeq produces the monotonically increasing report number herdr
// requires. A nanosecond timestamp naturally increases across omac
// restarts; the guard covers a rewound clock, which would otherwise
// make herdr drop every subsequent report as stale.
func (b *Bridge) nextSeq() int64 {
	seq := b.opts.Now().UnixNano()
	if seq <= b.lastSeq {
		seq = b.lastSeq + 1
	}
	b.lastSeq = seq
	return seq
}
