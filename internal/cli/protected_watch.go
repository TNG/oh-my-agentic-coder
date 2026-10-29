package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/netprompt"
	"github.com/TNG/oh-my-agentic-coder/internal/pathutil"
	"github.com/TNG/oh-my-agentic-coder/internal/prefs"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxdeny"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

// midsessionProtectedWatchInterval is how often the parent re-walks the
// deny-scan roots looking for files a protected pattern matched only
// after launch. Each tick re-walks incrementally: a denyScanCache
// carries every directory's fingerprint between ticks, so unchanged
// directories cost one lstat and only changed ones are re-read. A
// directory whose last read fell inside the coarse-timestamp window is
// listed once more on the next tick, so worst-case detection latency is
// one tick; an optional full pass (off by default) is a backstop for
// coarser filesystems. A file that appears and disappears within one
// interval is never detected. See sandboxrun.denyScanCache.
const midsessionProtectedWatchInterval = 10 * time.Second

// maxAlertPaths bounds the file choices one dialog lists. Worktree setup
// can create dozens of dotenv files in one tick; the dialog names the
// first few and "Ignore all" covers the rest.
const maxAlertPaths = 8

// protectedDialogTimeout bounds one decision dialog. The watch goroutine
// blocks while it is open; a user who walks away must not stop the scan for
// the rest of the session. report's context cancel closes it earlier.
const protectedDialogTimeout = 5 * time.Minute

// protectedWatch records what the mid-session protected-file watch
// detects so the notices can be delivered once the harness has
// released the terminal.
type protectedWatch struct {
	mu      sync.Mutex
	paths   []string
	notices []string
	stop    chan struct{}
	exited  chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	// choose is netprompt.ChooseContext in production; tests replace it.
	choose func(ctx context.Context, title, message string, options []string) (string, error)
	// alert/notify are netprompt.Alert/Notify in production; tests replace
	// them so a unit test never opens a real desktop dialog.
	alert  func(title, body string)
	notify func(title, body string)
	// prefs holds the exact paths the user dismissed permanently. The
	// parent owns this file; the sandboxed agent cannot write it. It is nil
	// when prefs.yaml could not be read: the watch then keeps exceptions
	// in memory only, so a later save cannot overwrite the unreadable file.
	prefs *prefs.Store
	// ignored mirrors the prefs list plus any exception whose save failed,
	// so the same file never prompts twice in one session. It is owned by
	// the watch goroutine: only observe, ignoreProtectedPath, and
	// ignoreProjectPaths touch it, and all three run on that goroutine.
	ignored map[string]bool
	// workdir is the session workdir; projectRoot is resolved from it
	// lazily on the first dialog. projectRootTried keeps a failed or
	// non-git resolution from running git again for every tick.
	workdir          string
	projectRoot      string
	projectRootTried bool
}

// startProtectedWatch mirrors the sandboxed child's deny-pattern walk
// in the parent: a protected-pattern file (".env", ...) created after
// launch is readable by the agent until the session restarts, because
// the kernel mask is fixed at launch. Each tick's new paths arrive as
// one batch, so a worktree setup that creates several dotenv files at
// once pops one decision dialog instead of one per file. The dialog can
// remember an exact file, or a basename for the whole git project, which
// is stored in the user-global prefs; the file stays protected at launch
// and only the repeated warning is skipped.
//
// Every path is tagged into the facade's protected set so
// GET /sandbox/denied answers honestly, and audit-logged even when the
// user ignores its warning.
//
// argv is the expanded sandbox argv: its flags are parsed and merged
// onto the policy profile exactly like the sandbox child does, so the
// watcher scans the same roots the child masks.
//
// Returns nil when there is nothing to mirror (non-native launcher,
// argv not a `omac sandbox run`); callers skip learn mode, where
// nothing is protected. checker may be nil; audit events are dropped
// when a is nil. tuning comes from the launcher config's
// sandbox.midsession_watch block.
func startProtectedWatch(a audit.Auditor, checker *sandboxrun.ProtectedPathSet, plan sandboxPlan, argv []string, workdir string, tuning sandboxrun.WatchTuning) *protectedWatch {
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
	store, loadNotice := loadWatchPrefs()
	ctx, cancel := context.WithCancel(context.Background())
	w := &protectedWatch{
		stop:    make(chan struct{}),
		exited:  make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
		choose:  netprompt.ChooseContext,
		alert:   netprompt.Alert,
		notify:  netprompt.Notify,
		prefs:   store,
		ignored: map[string]bool{},
		workdir: workdir,
	}
	if loadNotice != "" {
		w.recordNotice(loadNotice)
	}
	go func() {
		defer close(w.exited)
		if err := sandboxrun.WatchNewProtected(profile, workdir, midsessionProtectedWatchInterval, func(paths []string) {
			fresh := w.observe(paths, checker, a)
			if len(fresh) == 0 {
				return
			}
			for _, path := range w.askExceptions(fresh) {
				w.recordPath(path)
			}
		}, w.stop, tuning); err != nil {
			w.recordNotice(fmt.Sprintf("protected-file watch not started: %v", err))
		}
	}()
	return w
}

// loadWatchPrefs reads the user-global prefs for the watch. On error it returns
// a nil store plus one session notice: the watch then keeps exceptions in
// memory only, because saving from an empty store would overwrite a prefs file
// that failed to parse and drop every stored preference.
func loadWatchPrefs() (*prefs.Store, string) {
	store, err := prefs.Load()
	if err != nil {
		return nil, fmt.Sprintf("could not read %s; protected-file exceptions will not be saved this session: %v", prefs.Path(), err)
	}
	return store, ""
}

// observe tags and audit-logs every detection, then returns the paths the
// user has not already dismissed. Auditing stays complete even for ignored
// files: the exception suppresses the notice, not the security record.
func (w *protectedWatch) observe(paths []string, checker *sandboxrun.ProtectedPathSet, a audit.Auditor) []string {
	fresh := make([]string, 0, len(paths))
	for _, path := range paths {
		if checker != nil {
			checker.Add(path, sandboxdeny.RuleMidSession)
		}
		if a != nil {
			a.Emit(audit.ControlMutation("protected-file-watch", path, "created mid-session; readable until restart"))
		}
		canonical := pathutil.Canonical(path)
		if w.ignored[path] || w.ignored[canonical] || (w.prefs != nil &&
			(w.prefs.IgnoresProtectedPath(path) || w.prefs.IgnoresProtectedProjectPath(path))) {
			continue
		}
		fresh = append(fresh, path)
	}
	return fresh
}

// exceptionKind is the scope of a dialog choice.
type exceptionKind int

const (
	keepWarning exceptionKind = iota
	ignoreExactPath
	ignoreAllListed
	ignoreProjectName
)

type exceptionChoice struct {
	kind  exceptionKind
	value string
}

// askExceptions shows the decision dialog and returns the paths whose warning
// the user kept. A nil return means every path was dismissed permanently.
// It loops after a per-file or per-name choice so several entries can be
// dismissed without reopening the session; the safe "Keep warning (report at
// exit)" option is the default and saves no exception.
//
// The watch goroutine calls this inline, so the 10-second re-scan pauses while
// a dialog is open. A protected file created during that window is detected on
// the first tick after the dialog closes; one created and deleted inside the
// window is missed. Re-architecting the scan around the dialog is deliberately
// out of scope for this change.
func (w *protectedWatch) askExceptions(paths []string) []string {
	remaining := append([]string(nil), paths...)
	root, hasRoot := w.projectRootForDialog()
	notified := false
	for len(remaining) > 0 {
		shown := remaining
		if len(shown) > maxAlertPaths {
			shown = shown[:maxAlertPaths]
		}
		options, choices := protectedExceptionOptions(shown, remaining, root, hasRoot)
		if !notified {
			title, _ := protectedBatchNotice(remaining)
			w.sendNotify(title, "A decision dialog is waiting: ignore a protected-file warning permanently, or keep it.")
			notified = true
		}
		title, message := protectedExceptionPrompt(remaining, root, hasRoot)
		ctx, cancel := context.WithTimeout(w.ctx, protectedDialogTimeout)
		choice, err := w.choose(ctx, title, message, options)
		cancel()
		if err != nil {
			if w.ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
				return remaining // session is ending, or the user did not answer
			}
			// No dialog backend (or it failed): keep the informational
			// alert, which is the pre-dialog behavior.
			alertTitle, alertBody := protectedBatchNotice(remaining)
			w.sendAlert(alertTitle, alertBody)
			w.sendNotify(alertTitle, alertBody)
			return remaining
		}
		selected, ok := choices[choice]
		if !ok || selected.kind == keepWarning {
			return remaining
		}
		switch selected.kind {
		case ignoreAllListed:
			w.ignoreProtectedPaths(remaining)
			return nil
		case ignoreExactPath:
			w.ignoreProtectedPath(selected.value)
			remaining = removePath(remaining, selected.value)
		case ignoreProjectName:
			matched := projectIgnoreMatches(remaining, root, selected.value)
			w.ignoreProjectPaths(root, selected.value, matched)
			remaining = removePaths(remaining, matched)
		}
	}
	return nil
}

// projectRootForDialog resolves the git root once per watch. A failed or
// non-git resolution is cached too, so every tick does not spawn git again.
func (w *protectedWatch) projectRootForDialog() (string, bool) {
	if w.projectRootTried {
		return w.projectRoot, w.projectRoot != ""
	}
	w.projectRootTried = true
	if w.workdir == "" {
		return "", false
	}
	root, ok := resolveGitProjectRoot(w.workdir)
	if !ok {
		return "", false
	}
	w.projectRoot = root
	return root, true
}

// projectIgnoreMatches returns the paths covered by one project choice.
func projectIgnoreMatches(paths []string, root, name string) []string {
	var out []string
	for _, path := range paths {
		if prefs.MatchesProjectIgnore(root, name, path) {
			out = append(out, path)
		}
	}
	return out
}

// ignoreProjectPaths marks the affected paths in memory even when the prefs
// write fails, so a read-only config dir cannot turn one click into a dialog
// loop.
func (w *protectedWatch) ignoreProjectPaths(root, name string, matched []string) {
	if w.ignored == nil {
		w.ignored = map[string]bool{}
	}
	for _, path := range matched {
		w.ignored[path] = true
		if canonical := pathutil.Canonical(path); canonical != "" {
			w.ignored[canonical] = true
		}
	}
	if w.prefs == nil {
		return
	}
	if err := w.prefs.AddProtectedWatchProjectIgnore(root, name); err != nil {
		w.recordNotice(fmt.Sprintf("could not save protected-file project exception for %s: %v", stripControlChars(name), err))
	}
}

// ignoreProtectedPath records a permanent exact-path exception and mirrors it
// in memory even when the prefs write fails, so a read-only config dir cannot
// turn one click into a dialog loop.
func (w *protectedWatch) ignoreProtectedPath(path string) {
	w.ignoreProtectedPaths([]string{path})
}

// ignoreProtectedPaths records a batch of permanent exact-path exceptions with
// one prefs write ("Ignore all" can cover dozens of paths). It mirrors every
// path in memory even when the save fails, so a read-only config dir cannot
// turn one click into a dialog loop.
func (w *protectedWatch) ignoreProtectedPaths(paths []string) {
	if w.ignored == nil {
		w.ignored = map[string]bool{}
	}
	for _, path := range paths {
		w.ignored[path] = true
		if canonical := pathutil.Canonical(path); canonical != "" {
			w.ignored[canonical] = true
		}
	}
	if w.prefs == nil {
		return
	}
	if err := w.prefs.AddProtectedWatchIgnores(paths); err != nil {
		w.recordNotice(fmt.Sprintf("could not save %d protected-file exception(s): %v", len(paths), err))
	}
}

// protectedExceptionOptions builds the dialog choices from narrow to broad,
// after the safe default: keep, one exact file each, the whole batch, then one
// basename per project. choices maps the displayed labels back to their scope.
//
// Labels double as map keys. Two paths that differ only in control characters
// (legal in filenames) can therefore collapse to one label, and a click on the
// first row acts on the second file. The collision is accepted for now; it
// fails safe because either path was detected and protected the same way.
func protectedExceptionOptions(shown, remaining []string, root string, hasRoot bool) ([]string, map[string]exceptionChoice) {
	total := len(remaining)
	keep := fmt.Sprintf("Keep warning for %d file", total)
	if total != 1 {
		keep += "s"
	}
	keep += " (report at exit)"
	options := []string{keep}
	choices := map[string]exceptionChoice{keep: {kind: keepWarning}}
	for _, path := range shown {
		label := "Ignore this file permanently: " + stripControlChars(path)
		options = append(options, label)
		choices[label] = exceptionChoice{kind: ignoreExactPath, value: path}
	}
	if total > 1 {
		label := fmt.Sprintf("Ignore all %d files permanently", total)
		if len(shown) < total {
			label += fmt.Sprintf(" (%d listed)", len(shown))
		}
		options = append(options, label)
		choices[label] = exceptionChoice{kind: ignoreAllListed}
	}
	for _, name := range projectIgnoreNames(remaining, root, hasRoot) {
		label := fmt.Sprintf("Ignore every %s in this project (%s)",
			stripControlChars(name), stripControlChars(filepath.Base(root)))
		options = append(options, label)
		choices[label] = exceptionChoice{kind: ignoreProjectName, value: name}
	}
	return options, choices
}

// maxProjectNames bounds the project-scoped options one dialog offers. A
// worktree burst can create many distinct basenames; the exact paths and the
// batch choice still cover the rest.
const maxProjectNames = 4

// projectIgnoreNames returns the unique basenames under root, sorted for a
// stable dialog. Only names that would really match are offered.
func projectIgnoreNames(paths []string, root string, hasRoot bool) []string {
	if !hasRoot || root == "" {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, path := range paths {
		name := filepath.Base(pathutil.Canonical(path))
		if seen[name] || !prefs.MatchesProjectIgnore(root, name, path) {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maxProjectNames {
		names = names[:maxProjectNames]
	}
	return names
}

// protectedExceptionPrompt is the dialog body. It states the one trade-off a
// user needs to decide: ignored files stay protected at launch, but a new one
// stays readable without a notice until restart.
func protectedExceptionPrompt(paths []string, root string, hasRoot bool) (title, message string) {
	title = "omac: protected file created"
	if len(paths) != 1 {
		title = "omac: protected files created"
	}
	message = fmt.Sprintf(
		"%d protected-pattern %s created during this session and %s readable by the agent until omac restarts. Ignoring a file skips this warning in future sessions; the file is still protected at launch. Keeping the warning saves no exception: the file stays in the audit log and is listed when the session ends.",
		len(paths), plural(len(paths), "file was", "files were"), plural(len(paths), "is", "are"))
	if hasRoot {
		message += fmt.Sprintf(
			" Project scope: %s, including every worktree of this repository.",
			stripControlChars(root))
	}
	return title, message
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func removePath(paths []string, remove string) []string {
	return slices.DeleteFunc(paths, func(path string) bool { return path == remove })
}

// removePaths removes every path in remove, preserving the order of the rest.
func removePaths(paths, remove []string) []string {
	drop := make(map[string]bool, len(remove))
	for _, path := range remove {
		drop[path] = true
	}
	return slices.DeleteFunc(paths, func(path string) bool { return drop[path] })
}

// protectedBatchNotice builds the one info text for a tick's detections when
// no decision dialog is available. Paths are control-stripped because they
// reach a terminal and a desktop notification; the list is bounded so a large
// burst cannot produce a dialog taller than the screen.
func protectedBatchNotice(paths []string) (title, msg string) {
	if len(paths) == 1 {
		return "omac: protected file created",
			fmt.Sprintf("%s matches a sandbox-protected pattern and is readable by the agent until the session is restarted. Restart omac to have the sandbox mask it.", stripControlChars(paths[0]))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d files match a sandbox-protected pattern and are readable by the agent until the session is restarted:", len(paths))
	shown := paths
	if len(shown) > maxAlertPaths {
		shown = shown[:maxAlertPaths]
	}
	for _, path := range shown {
		b.WriteString("\n  ")
		b.WriteString(stripControlChars(path))
	}
	if rest := len(paths) - len(shown); rest > 0 {
		fmt.Fprintf(&b, "\n  … and %d more (see the session-end summary)", rest)
	}
	b.WriteString("\nRestart omac to have the sandbox mask them.")
	return "omac: protected files created", b.String()
}

// report stops the watch and prints what it recorded via warn. Call it only
// after the harness exited and the parent owns the terminal again — mid-session
// stderr writes would be painted over the harness TUI's frame. Cancelling the
// context closes a decision dialog that is still open. A single detection keeps
// its detailed line; a burst collapses to one summary line unless verbose asks
// for every kept warning. Nil-safe: a watch that never started reports nothing.
func (w *protectedWatch) report(verbose bool, warn func(format string, args ...any)) {
	if w == nil {
		return
	}
	if w.cancel != nil {
		w.cancel()
	}
	close(w.stop)
	<-w.exited
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, n := range w.notices {
		warn("%s", n)
	}
	switch {
	case len(w.paths) == 0:
		return
	case verbose, len(w.paths) == 1:
		for _, path := range w.paths {
			warn("%s", protectedWatchNotice(path))
		}
	default:
		warn("%s", protectedWatchSummary(w.paths))
	}
}

// protectedWatchNotice is the detailed line for one mid-session detection.
func protectedWatchNotice(path string) string {
	return fmt.Sprintf("security notice: %s matches a sandbox-protected pattern but was created after this session started; the agent could read it until this session ended", stripControlChars(path))
}

// protectedWatchSummary keeps a burst to one line while still naming an
// example path. The individual paths stay in the audit log and in --verbose.
func protectedWatchSummary(paths []string) string {
	return fmt.Sprintf("security notice: %d protected-pattern files were created after this session started and were readable until it ended (first: %s). The next omac start masks them; pass --verbose to list every warning you kept.", len(paths), stripControlChars(paths[0]))
}

func (w *protectedWatch) sendAlert(title, body string) {
	if w.alert != nil {
		w.alert(title, body)
	}
}

func (w *protectedWatch) sendNotify(title, body string) {
	if w.notify != nil {
		w.notify(title, body)
	}
}

func (w *protectedWatch) recordPath(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.paths = append(w.paths, path)
}

func (w *protectedWatch) recordNotice(notice string) {
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
