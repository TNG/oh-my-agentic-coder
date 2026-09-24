package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// runReportStubs wires a watch whose walker exits as soon as report stops.
func runReportStubs() *protectedWatch {
	w := &protectedWatch{stop: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		<-w.stop
		close(w.exited)
	}()
	return w
}

func TestStopAndNoticesReturnsRecords(t *testing.T) {
	w := runReportStubs()
	w.record("security notice: /workdir/.env matches a sandbox-protected pattern")
	notices := w.stopAndNotices()
	if len(notices) != 1 || !strings.Contains(notices[0], "/workdir/.env") {
		t.Fatalf("stopAndNotices returned %v", notices)
	}
}

func TestStopAndNoticesNilSafe(t *testing.T) {
	var w *protectedWatch
	if got := w.stopAndNotices(); got != nil {
		t.Fatalf("nil watch reported %v", got)
	}
}

func TestStartProtectedWatchSkipsNoPolicy(t *testing.T) {
	if w := startProtectedWatch(audit.Nop(), nil, sandboxPlan{}, nil, "/workdir", nil); w != nil {
		t.Fatal("a plan without a resolved policy must not start a watch")
	}
	argv := []string{"/omac", "not-sandbox", "run"}
	plan := sandboxPlan{Policy: &sandboxprofile.Profile{}}
	if w := startProtectedWatch(audit.Nop(), nil, plan, argv, "/workdir", nil); w != nil {
		t.Fatal("argv that is not a sandbox run must not start a watch")
	}
}

// TestNoticeAnnouncerPrintsOnce: one banner per detection, tinted when the
// sink is a terminal, plain when not, never repeated.
func TestNoticeAnnouncerPrintsOnce(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ann := newNoticeAnnouncer(f)
	ann.announce("/workdir/evil.json")
	ann.announce("/workdir/evil.json") // a second detection would be a second record; same path once

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "matches a protected pattern") != 2 {
		t.Errorf("each announcement prints the banner: %q", data)
	}
	if n := strings.Count(string(data), "/workdir/evil.json"); n != 2 {
		t.Errorf("expected the path in each banner, got %d: %q", n, data)
	}
}

// TestPrintRestartCallout: the session-end callout states that the next
// start masks the flagged paths, one line per notice, and stays within the
// box width even for long notices (no broken frame on a narrow terminal).
func TestPrintRestartCallout(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	long := strings.Repeat("longnotice ", 20) + strings.Repeat("a", 60) + filepath.Join("/workdir", "with-very-long-path", "deep", "planted", ".omac")
	printRestartCallout(f, []string{
		"/workdir/.env matches a sandbox-protected pattern; readable until restart",
		long,
	})
	printRestartCallout(f, nil) // no-op

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{
		"Restart recommended",
		"/workdir/.env matches a sandbox-protected pattern",
		"The next omac start re-runs the deny scan",
		"does not repeat",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("callout missing %q:\n%s", want, out)
		}
	}
	// The callout shape renders once.
	if strings.Count(out, "Restart recommended") != 1 || strings.Count(out, "╭") != 1 {
		t.Errorf("callout should render once:\n%s", out)
	}
	// Frame never exceeds ~80 columns, even with a long path.
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if v := visibleLen(l); v > 80 {
			t.Errorf("callout line is %d visible columns (>80), frame would break: %q", v, l)
		}
	}
	// Nothing lost to the wrap: comparing with all decoration stripped, since
	// box borders separate even hard-split words.
	compact := func(s string) string {
		return strings.NewReplacer(" ", "", "\n", "", "│", "", "╭", "", "╮", "", "╰", "", "╯", "", "├", "", "┤", "", "─", "").Replace(s)
	}
	if !strings.Contains(compact(out), compact(long)) {
		t.Errorf("word-wrapping must not lose content:\n%s", out)
	}
}
