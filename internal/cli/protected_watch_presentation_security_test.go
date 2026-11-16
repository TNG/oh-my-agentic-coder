package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxdeny"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

func TestProtectedWatchPresentationAndRawLookup(t *testing.T) {
	isolateHome(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "dialogs.log")
	t.Setenv("WATCH_DIALOG_LOG", log)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Only external GUI programs are replaced; checker, auditor and dialog code are real.
	for _, name := range []string{"osascript", "zenity", "kdialog", "notify-send"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$WATCH_DIALOG_LOG\"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	wd := t.TempDir()
	parent := filepath.Join(wd, "café-東京\x1b[31m\u009b2J")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, ".env")
	// Created before the watch starts, so the background scan counts it as
	// already present and never reports it. The test feeds it to the
	// detection handler itself instead of waiting for a scan tick.
	if err := os.WriteFile(path, []byte("dummy"), 0600); err != nil {
		t.Fatal(err)
	}
	safe := filepath.Join(wd, "café-東京[31m2J", ".env")
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := audit.New(audit.Config{Enabled: true, Path: auditPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	checker := sandboxrun.UnrestrictedProtectedPathSet()
	p := &sandboxprofile.Profile{}
	p.Workdir.Access = sandboxprofile.AccessReadWrite
	w := startProtectedWatch(a, checker, sandboxPlan{Native: true, Policy: p}, []string{"omac", "sandbox", "run", "--", "true"}, wd, sandboxrun.WatchTuning{})
	if w == nil {
		t.Fatal("watch did not start")
	}
	var report bytes.Buffer
	reported := false
	stopWatch := func() {
		if !reported {
			reported = true
			w.report(false, func(f string, args ...any) { fmt.Fprintf(&report, f+"\n", args...) })
		}
	}
	t.Cleanup(stopWatch)
	w.handle([]string{path}, checker, a)
	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if rule, ok := checker.IsProtected(path); !ok || rule != sandboxdeny.RuleMidSession {
		t.Fatalf("raw security lookup lost: %q %v", rule, ok)
	}
	if _, ok := checker.IsProtected(safe); ok {
		t.Fatal("presentation spelling incorrectly became security identity")
	}
	var event audit.Event
	if err := json.Unmarshal(bytes.TrimSpace(raw), &event); err != nil {
		t.Fatal(err)
	}
	if event.Dir != safe || bytes.Contains(raw, []byte("\u009b")) {
		t.Errorf("unsafe audit path: %q", event.Dir)
	}
	// The end-of-session report holds the recorded path.
	stopWatch()
	notices := strings.Split(strings.TrimSpace(report.String()), "\n")
	if len(notices) != 1 || !strings.Contains(notices[0], safe) || strings.ContainsAny(notices[0], "\x1b\u009b") {
		t.Errorf("unsafe/missing notice: %q", notices)
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		dialogs, err := os.ReadFile(log)
		if err != nil || !bytes.Contains(dialogs, []byte("café-東京")) || strings.ContainsAny(string(dialogs), "\x1b\u009b") {
			t.Errorf("unsafe/missing dialogs: %q %v", dialogs, err)
		}
	}
}
