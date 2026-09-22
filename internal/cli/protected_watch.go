package cli

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/netprompt"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxdeny"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

// midsessionProtectedWatchInterval is how often the parent re-walks the
// deny-scan roots looking for files a protected pattern matched only
// after launch. Detection latency equals this interval; a file that
// appears and disappears within one interval is never detected. Each
// tick is a full re-walk; switch to fsnotify if the I/O cost ever
// matters.
const midsessionProtectedWatchInterval = 10 * time.Second

// protectedWatch records what the mid-session protected-file watch
// detects so the notices can be delivered once the harness has
// released the terminal.
type protectedWatch struct {
	mu      sync.Mutex
	notices []string
	stop    chan struct{}
	exited  chan struct{}
}

// startProtectedWatch mirrors the sandboxed child's deny-pattern walk
// in the parent: a protected-pattern file (".env", ...) created after
// launch is readable by the agent until the session restarts, because
// the kernel mask is fixed at launch. Each detection
//
//   - pops an info dialog plus a passive desktop notification
//     (netprompt.Alert/Notify), which survive a harness TUI redraw,
//   - prints a red banner once on stderr, the tty the harness shares
//     with omac: the TUI scrolls up on it rather than overpainting it,
//     so one print is already unmissable.
//
// and tags the path into the facade's protected set so
// GET /sandbox/denied answers honestly, audit-logs it, and records it
// for the session-end callout.
//
// argv is the expanded sandbox argv: its flags are parsed and merged
// onto the policy profile exactly like the sandbox child does, so the
// watcher scans the same roots the child masks.
//
// Returns nil when there is nothing to mirror (no resolved policy, or
// argv not a `omac sandbox run`); callers skip learn mode, where
// nothing is protected. checker may be nil; audit events are dropped
// when a is nil.
func startProtectedWatch(a audit.Auditor, checker *sandboxrun.ProtectedPathSet, plan sandboxPlan, argv []string, workdir string, stderr *os.File) *protectedWatch {
	if plan.Policy == nil || len(argv) < 3 || argv[1] != "sandbox" || argv[2] != "run" {
		return nil
	}
	flags, err := sandboxprofile.ParseFlags(argv[3:])
	if err != nil {
		// The sandbox child parses the same argv; the launch is about
		// to fail the same way and reports the usage error itself.
		return nil
	}
	profile, _ := sandboxprofile.Merge(plan.Policy, flags)
	w := &protectedWatch{stop: make(chan struct{}), exited: make(chan struct{})}
	announce := newNoticeAnnouncer(stderr)
	go func() {
		defer close(w.exited)
		if err := sandboxrun.WatchNewProtected(profile, workdir, midsessionProtectedWatchInterval, func(path string) {
			w.record(fmt.Sprintf("security notice: %s matches a sandbox-protected pattern but was created after this session started; the agent could read it until this session ended", path))
			if checker != nil {
				checker.Add(path, sandboxdeny.RuleMidSession)
			}
			const title = "omac: protected file created"
			msg := fmt.Sprintf("%s matches a sandbox-protected pattern and is readable by the agent until the session is restarted. Restart omac to have the sandbox mask it.", path)
			netprompt.Alert(title, msg)
			netprompt.Notify(title, msg)
			announce.announce(path)
			if a != nil {
				a.Emit(audit.ControlMutation("protected-file-watch", path, "created mid-session; readable until restart"))
			}
		}, w.stop); err != nil {
			w.record(fmt.Sprintf("protected-file watch not started: %v", err))
		}
	}()
	return w
}

// noticeAnnouncer paints a newly detected protected path prominently on the
// terminal, once.
type noticeAnnouncer struct {
	w      *os.File
	styler styler
}

func newNoticeAnnouncer(stderr *os.File) noticeAnnouncer {
	if stderr == nil {
		return noticeAnnouncer{}
	}
	return noticeAnnouncer{
		w:      stderr,
		styler: newStyler(stderr),
	}
}

// announce prints the banner once, synchronously in the detection callback.
func (n noticeAnnouncer) announce(path string) {
	if n.w == nil {
		return
	}
	path = stripControlChars(path)
	fmt.Fprintf(n.w, "\n%s\n", n.styler.paint(
		"⚠ omac: "+path+" matches a protected pattern (e.g. .env/.omac) and was created during this session; the agent can read it until omac restarts.",
		ansiBold, ansiRed))
}

// stopAndNotices stops the watch and any pending repaint, waits for the
// walker to exit, and returns the recorded notices. Call it only after the
// harness exited and the parent owns the terminal again; callers render the
// session-end callout. Nil-safe: a watch that never started reports nothing.
func (w *protectedWatch) stopAndNotices() []string {
	if w == nil {
		return nil
	}
	close(w.stop)
	<-w.exited
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.notices
}

// maxRestartCalloutContent is the visible width each callout line wraps to:
// 76 content + 1+1 padding + 2 borders keeps the box within ~80 columns, so
// it never wraps its own frame into a second line on a narrow terminal.
const maxRestartCalloutContent = 76

// printRestartCallout renders the session-end notices as a full-width
// callout: the next omac start re-runs the deny scan, so the flagged paths
// are masked from that session on, and the callout does not repeat. Long
// lines are word-wrapped to the box width rather than pushing the frame
// beyond/~around the terminal edge.
func printRestartCallout(w *os.File, notices []string) {
	if len(notices) == 0 || w == nil {
		return
	}
	st := newStyler(w)
	var lines []string
	for _, n := range notices {
		lines = append(lines, wrapVisible(stripControlChars(n), maxRestartCalloutContent)...)
	}
	lines = append(lines, wrapVisible(
		"The next omac start re-runs the deny scan, so the flagged paths are masked from that session on. This callout does not repeat.",
		maxRestartCalloutContent)...)
	st.callout(w, ansiYellow, "Restart recommended", lines)
}

func (w *protectedWatch) record(notice string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notices = append(w.notices, notice)
}

// protectedChecker extracts the concrete protected-path set the facade
// answers from, or nil when the facade runs without one. The watch
// needs the concrete type: Add is how mid-session detections reach
// GET /sandbox/denied.
func protectedChecker(f *facade.Facade) *sandboxrun.ProtectedPathSet {
	if ps, ok := f.ProtectedPathChecker.(*sandboxrun.ProtectedPathSet); ok {
		return ps
	}
	return nil
}
