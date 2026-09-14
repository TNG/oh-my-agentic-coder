package cli

import (
	"fmt"
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
// the kernel mask is fixed at launch. Each detection pops an info
// dialog plus a passive notification (netprompt.Alert/Notify — stderr
// would corrupt the TUI's frame), tagged into the facade's protected
// set so GET /sandbox/denied answers honestly, audit-logged, and
// recorded for the session-end report.
//
// argv is the expanded sandbox argv: its flags are parsed and merged
// onto the policy profile exactly like the sandbox child does, so the
// watcher scans the same roots the child masks.
//
// Returns nil when there is nothing to mirror (non-native launcher,
// argv not a `omac sandbox run`); callers skip learn mode, where
// nothing is protected. checker may be nil; audit events are dropped
// when a is nil.
func startProtectedWatch(a audit.Auditor, checker *sandboxrun.ProtectedPathSet, plan sandboxPlan, argv []string, workdir string) *protectedWatch {
	if !plan.Native || plan.Policy == nil || len(argv) < 3 || argv[1] != "sandbox" || argv[2] != "run" {
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
	go func() {
		defer close(w.exited)
		if err := sandboxrun.WatchNewProtected(profile, workdir, midsessionProtectedWatchInterval, func(path string) {
			w.record(fmt.Sprintf("security notice: %s matches a sandbox-protected pattern but was created after this session started; the agent could read it until this session ended", path))
			if checker != nil {
				checker.Add(path, sandboxdeny.RuleMidSession)
			}
			const title = "omac: protected file created"
			msg := fmt.Sprintf("%s matches a sandbox-protected pattern and is readable by the agent until the session is restarted", path)
			netprompt.Alert(title, msg)
			netprompt.Notify(title, msg)
			if a != nil {
				a.Emit(audit.ControlMutation("protected-file-watch", path, "created mid-session; readable until restart"))
			}
		}, w.stop); err != nil {
			w.record(fmt.Sprintf("protected-file watch not started: %v", err))
		}
	}()
	return w
}

// report stops the watch and prints one line per recorded notice via
// warn. Call it only after the harness exited and the parent owns the
// terminal again — mid-session stderr writes would be painted over the
// harness TUI's frame. Nil-safe: a watch that never started reports
// nothing.
func (w *protectedWatch) report(warn func(format string, args ...any)) {
	if w == nil {
		return
	}
	close(w.stop)
	<-w.exited
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range w.notices {
		warn("%s", n)
	}
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
