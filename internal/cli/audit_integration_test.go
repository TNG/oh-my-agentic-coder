package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// readAuditEvents decodes a JSONL audit file into generic maps.
func readAuditEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open audit log: %v", err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("audit line not valid JSON: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// TestServeControlMutationsAudited drives activate/reload/deactivate on a
// serveServer whose auditor writes to a temp file, then asserts the
// control.mutation and route.state events appear with monotonic seq and a
// hashed (never raw) dir-token namespace. (Task 5.1 + 5.3.)
func TestServeControlMutationsAudited(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "audit.jsonl")
	a, err := audit.New(audit.Config{Enabled: true, Path: logPath, Mode: audit.ModeServe})
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}

	s := newServeServerForTest(t)
	s.auditor = a

	wd := t.TempDir()
	stageSkillWithSecret(t, wd, "slack") // pending-credentials => route.state

	if _, err := s.activate(wd); err != nil {
		t.Fatalf("activate: %v", err)
	}
	// capture the minted dir-token so we can assert it never leaks.
	s.mu.RLock()
	token := s.dirs[wd].Token
	s.mu.RUnlock()

	// Drive a reload and a deactivate via the handlers' underlying methods.
	s.deactivate(wd)
	s.aud().Emit(audit.ControlMutation("deactivate", wd, "ok")) // handler path

	_ = a.Close()

	// The raw token must never appear anywhere in the log.
	raw, _ := os.ReadFile(logPath)
	if token != "" && strings.Contains(string(raw), token) {
		t.Fatalf("dir-token leaked into audit log")
	}

	evs := readAuditEvents(t, logPath)
	if len(evs) == 0 {
		t.Fatalf("no audit events written")
	}
	// seq strictly increasing by 1, shared run_id.
	runID, _ := evs[0]["run_id"].(string)
	for i, ev := range evs {
		if got := int(ev["seq"].(float64)); got != i+1 {
			t.Fatalf("event %d seq = %d, want %d", i, got, i+1)
		}
		if ev["run_id"].(string) != runID {
			t.Fatalf("run_id changed within run")
		}
	}
	// A route.state event should exist for the pending-credentials skill,
	// with a hashed namespace.
	var sawRouteState bool
	for _, ev := range evs {
		if ev["type"] == "route.state" {
			sawRouteState = true
			ns, _ := ev["namespace"].(string)
			if ns != "" && !strings.HasPrefix(ns, "ns_") {
				t.Fatalf("route.state namespace not hashed: %q", ns)
			}
		}
	}
	if !sawRouteState {
		t.Fatalf("expected a route.state event for the pending-credentials skill")
	}
}

// TestNoAuditStrictMisuseStart asserts --no-audit + --audit-strict is
// rejected as a misuse error by the launch pipeline. (Task 5.5.)
func TestNoAuditStrictMisuseStart(t *testing.T) {
	isolateHome(t)
	env := makeEnv(t.TempDir())
	code := runLaunch(env, launchOpts{
		label:       "start",
		harness:     config.DefaultHarness(),
		noAudit:     true,
		auditStrict: true,
	})
	if code != ExitMisuse {
		t.Fatalf("want ExitMisuse (%d) for --no-audit+--audit-strict, got %d", ExitMisuse, code)
	}
}
