package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestProtectedWatchReport(t *testing.T) {
	w := &protectedWatch{stop: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		<-w.stop
		close(w.exited)
	}()
	w.record("security notice: /workdir/.env matches a sandbox-protected pattern")

	var buf bytes.Buffer
	w.report(func(format string, args ...any) {
		fmt.Fprintf(&buf, format+"\n", args...)
	})
	if !strings.Contains(buf.String(), "/workdir/.env") {
		t.Fatalf("report missing notice: %q", buf.String())
	}
	if strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("report should print exactly the recorded notices: %q", buf.String())
	}
}

func TestProtectedWatchReportNilSafe(t *testing.T) {
	var w *protectedWatch
	w.report(func(format string, args ...any) {
		t.Fatal("nil watch must not report")
	})
}

func TestStartProtectedWatchSkipsNonNative(t *testing.T) {
	if w := startProtectedWatch(audit.Nop(), nil, sandboxPlan{Name: "nono"}, nil, "/workdir"); w != nil {
		t.Fatal("non-native plan must not start a watch")
	}
	argv := []string{"/omac", "not-sandbox", "run"}
	plan := sandboxPlan{Name: "builtin", Native: true, Policy: &sandboxprofile.Profile{}}
	if w := startProtectedWatch(audit.Nop(), nil, plan, argv, "/workdir"); w != nil {
		t.Fatal("argv that is not a sandbox run must not start a watch")
	}
}
