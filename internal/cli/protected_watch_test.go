package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/prefs"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

// newReportableWatch returns a watch whose goroutine exits as soon as report
// closes stop. report must be called once.
func newReportableWatch() *protectedWatch {
	w := &protectedWatch{stop: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		<-w.stop
		close(w.exited)
	}()
	return w
}

func TestProtectedWatchReport(t *testing.T) {
	w := newReportableWatch()
	w.recordPath("/workdir/.env")

	var buf bytes.Buffer
	w.report(false, func(format string, args ...any) {
		fmt.Fprintf(&buf, format+"\n", args...)
	})
	if !strings.Contains(buf.String(), "/workdir/.env") {
		t.Fatalf("report missing notice: %q", buf.String())
	}
	if strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("report should print exactly the recorded notices: %q", buf.String())
	}
}

func TestProtectedWatchReportCollapsesBurst(t *testing.T) {
	w := newReportableWatch()
	w.recordPath("/workdir/a/.env.local")
	w.recordPath("/workdir/b/.env.local")
	w.recordPath("/workdir/c/.env")

	var buf bytes.Buffer
	w.report(false, func(format string, args ...any) {
		fmt.Fprintf(&buf, format+"\n", args...)
	})
	if got := buf.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "3 protected-pattern files") {
		t.Fatalf("burst must collapse to one summary line, got %q", got)
	}
	if strings.Contains(buf.String(), "/workdir/b/.env.local") {
		t.Fatalf("summary must not list every path, got %q", buf.String())
	}
}

func TestProtectedWatchReportVerboseListsBurst(t *testing.T) {
	w := newReportableWatch()
	w.recordPath("/workdir/a/.env.local")
	w.recordPath("/workdir/b/.env.local")

	var buf bytes.Buffer
	w.report(true, func(format string, args ...any) {
		fmt.Fprintf(&buf, format+"\n", args...)
	})
	if got := buf.String(); strings.Count(got, "\n") != 2 ||
		!strings.Contains(got, "/workdir/a/.env.local") || !strings.Contains(got, "/workdir/b/.env.local") {
		t.Fatalf("verbose must list every path, got %q", got)
	}
}

func TestProtectedWatchReportNilSafe(t *testing.T) {
	var w *protectedWatch
	w.report(false, func(format string, args ...any) {
		t.Fatal("nil watch must not report")
	})
}

func TestProtectedBatchNotice(t *testing.T) {
	title, msg := protectedBatchNotice([]string{"/work/.env.local"})
	if title != "omac: protected file created" || !strings.Contains(msg, "/work/.env.local") {
		t.Fatalf("single notice = %q / %q", title, msg)
	}

	var many []string
	for i := range maxAlertPaths + 2 {
		many = append(many, fmt.Sprintf("/work/%d/.env", i))
	}
	title, msg = protectedBatchNotice(many)
	if title != "omac: protected files created" {
		t.Fatalf("batch title = %q", title)
	}
	if !strings.Contains(msg, "10 files match") {
		t.Fatalf("batch must state the full count, got %q", msg)
	}
	if strings.Count(msg, "/work/") != maxAlertPaths {
		t.Fatalf("batch must list at most %d paths, got %q", maxAlertPaths, msg)
	}
	if !strings.Contains(msg, "and 2 more") {
		t.Fatalf("batch must account for the hidden paths, got %q", msg)
	}
}

// newExceptionWatch returns a watch whose dialog answers are scripted. Each
// step receives the option labels and returns the chosen label.
func newExceptionWatch(t *testing.T, steps ...func([]string) string) (*protectedWatch, *[]string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := prefs.Load()
	if err != nil {
		t.Fatal(err)
	}
	var alerts []string
	i := 0
	w := &protectedWatch{
		ctx:     context.Background(),
		prefs:   store,
		ignored: map[string]bool{},
		alert:   func(title, _ string) { alerts = append(alerts, title) },
		notify:  func(string, string) {},
		choose: func(_ context.Context, _ string, _ string, options []string) (string, error) {
			if i >= len(steps) {
				return options[0], nil
			}
			step := steps[i]
			i++
			return step(options), nil
		},
	}
	return w, &alerts
}

func TestAskExceptionsIgnoresOneThenKeepsRest(t *testing.T) {
	w, _ := newExceptionWatch(t,
		func(options []string) string { return options[1] }, // first file
		func(options []string) string { return options[0] }, // keep warning
	)
	kept := w.askExceptions([]string{"/a/.env.local", "/b/.env.local"})
	if len(kept) != 1 || kept[0] != "/b/.env.local" {
		t.Fatalf("kept = %v, want only /b/.env.local", kept)
	}
	store, err := prefs.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !store.IgnoresProtectedPath("/a/.env.local") || store.IgnoresProtectedPath("/b/.env.local") {
		t.Fatalf("persisted exceptions = %v", store.ProtectedWatchIgnore)
	}
}

func TestAskExceptionsIgnoresAll(t *testing.T) {
	w, _ := newExceptionWatch(t, func(options []string) string {
		for _, option := range options {
			if strings.HasPrefix(option, "Ignore all ") {
				return option
			}
		}
		t.Fatalf("all option missing from %v", options)
		return ""
	})
	kept := w.askExceptions([]string{"/a/.env", "/b/.env", "/c/.env"})
	if len(kept) != 0 {
		t.Fatalf("kept = %v, want none", kept)
	}
	store, err := prefs.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(store.ProtectedWatchIgnore) != 3 {
		t.Fatalf("persisted exceptions = %v, want 3", store.ProtectedWatchIgnore)
	}
}

func TestAskExceptionsKeepAll(t *testing.T) {
	w, _ := newExceptionWatch(t, func(options []string) string { return options[0] })
	paths := []string{"/a/.env", "/b/.env"}
	kept := w.askExceptions(paths)
	if len(kept) != 2 {
		t.Fatalf("kept = %v, want both", kept)
	}
	store, err := prefs.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(store.ProtectedWatchIgnore) != 0 {
		t.Fatalf("persisted exceptions = %v, want none", store.ProtectedWatchIgnore)
	}
}

func TestAskExceptionsFallsBackWhenDialogFails(t *testing.T) {
	w, alerts := newExceptionWatch(t)
	w.choose = func(context.Context, string, string, []string) (string, error) {
		return "", errors.New("no backend")
	}
	paths := []string{"/a/.env"}
	kept := w.askExceptions(paths)
	if len(kept) != 1 || kept[0] != "/a/.env" {
		t.Fatalf("kept = %v, want the path", kept)
	}
	if len(*alerts) != 1 {
		t.Fatalf("fallback alerts = %v, want one", *alerts)
	}
}

func TestProtectedExceptionOptions(t *testing.T) {
	options, choices := protectedExceptionOptions([]string{"/a/.env"}, []string{"/a/.env"}, "", false)
	if len(options) != 2 || options[0] != "Keep warning for 1 file" ||
		options[1] != "Ignore this file permanently: /a/.env" {
		t.Fatalf("options = %v", options)
	}
	if c := choices["Ignore this file permanently: /a/.env"]; c.kind != ignoreExactPath || c.value != "/a/.env" {
		t.Fatalf("exact choice = %+v", c)
	}
	options, choices = protectedExceptionOptions([]string{"/a/.env", "/b/.env"}, []string{"/a/.env", "/b/.env"}, "", false)
	if options[len(options)-1] != "Ignore all 2 files permanently" {
		t.Fatalf("all option should be last, got %v", options)
	}
	if c := choices["Ignore all 2 files permanently"]; c.kind != ignoreAllListed {
		t.Fatalf("all choice = %+v", c)
	}
}

func writeProtectedFile(t *testing.T, root, rel string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProtectedExceptionOptionsProjectLast(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		writeProtectedFile(t, root, ".worktrees/a/.env.local"),
		writeProtectedFile(t, root, ".worktrees/b/.env.local"),
		writeProtectedFile(t, root, ".worktrees/a/.env"),
	}
	options, choices := protectedExceptionOptions(paths, paths, root, true)
	if len(options) < 3 {
		t.Fatalf("options = %v", options)
	}
	if options[len(options)-2] != "Ignore every .env in this project ("+filepath.Base(root)+")" ||
		options[len(options)-1] != "Ignore every .env.local in this project ("+filepath.Base(root)+")" {
		t.Fatalf("project options must be last and sorted, got %v", options)
	}
	for _, option := range options {
		if strings.Contains(option, "all projects") {
			t.Fatalf("global option must not exist: %q", option)
		}
	}
	if c := choices[options[len(options)-1]]; c.kind != ignoreProjectName || c.value != ".env.local" {
		t.Fatalf("project choice = %+v", c)
	}
}

func TestAskExceptionsProjectChoice(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	a := writeProtectedFile(t, root, ".worktrees/a/.env.local")
	b := writeProtectedFile(t, root, ".worktrees/b/.env.local")
	other := writeProtectedFile(t, root, ".worktrees/a/.env")
	w, _ := newExceptionWatch(t, func(options []string) string {
		for _, option := range options {
			if strings.HasPrefix(option, "Ignore every .env.local in this project") {
				return option
			}
		}
		t.Fatalf("project option missing from %v", options)
		return ""
	})
	w.workdir = root
	kept := w.askExceptions([]string{a, b, other})
	if len(kept) != 1 || kept[0] != other {
		t.Fatalf("kept = %v, want only %s", kept, other)
	}
	store, err := prefs.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(store.ProtectedWatchProjectIgnores) != 1 {
		t.Fatalf("project exceptions = %v", store.ProtectedWatchProjectIgnores)
	}
	if !store.IgnoresProtectedProjectPath(b) {
		t.Fatal("a sibling worktree path must match the project exception")
	}
	if store.IgnoresProtectedProjectPath(other) {
		t.Fatal("a different basename must stay reported")
	}
}

func TestProjectExceptionCoversNewWorktreeAndKeepsAudit(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := prefs.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddProtectedWatchProjectIgnore(root, ".env.local"); err != nil {
		t.Fatal(err)
	}
	w := &protectedWatch{prefs: store, ignored: map[string]bool{}, workdir: root}
	auditor := &recordingAuditor{}
	checker := sandboxrun.UnrestrictedProtectedPathSet()
	newWorktreePath := writeProtectedFile(t, root, ".worktrees/not-seen-yet/.env.local")
	fresh := w.observe([]string{newWorktreePath}, checker, auditor)
	if len(fresh) != 0 {
		t.Fatalf("fresh = %v, want none", fresh)
	}
	if len(auditor.events) != 1 {
		t.Fatalf("audit events = %d, want one", len(auditor.events))
	}
	if _, ok := checker.IsProtected(newWorktreePath); !ok {
		t.Fatal("an ignored project path must stay in the facade protected set")
	}
}

type recordingAuditor struct {
	events []audit.Event
}

func (r *recordingAuditor) Emit(ev audit.Event) { r.events = append(r.events, ev) }
func (r *recordingAuditor) Close() error        { return nil }
func (r *recordingAuditor) RunID() string       { return "" }
func (r *recordingAuditor) NextSeq() uint64     { return 0 }

func TestStartProtectedWatchSkipsNonNative(t *testing.T) {
	if w := startProtectedWatch(audit.Nop(), nil, sandboxPlan{Name: "nono"}, nil, "/workdir", sandboxrun.WatchTuning{}); w != nil {
		t.Fatal("non-native plan must not start a watch")
	}
	argv := []string{"/omac", "not-sandbox", "run"}
	plan := sandboxPlan{Name: "builtin", Native: true, Policy: &sandboxprofile.Profile{}}
	if w := startProtectedWatch(audit.Nop(), nil, plan, argv, "/workdir", sandboxrun.WatchTuning{}); w != nil {
		t.Fatal("argv that is not a sandbox run must not start a watch")
	}
}
