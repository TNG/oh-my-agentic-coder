package supervisor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
)

// envMap turns buildEnv's []string ("K=V") into a map for assertions.
func envMap(kv []string) map[string]string {
	m := make(map[string]string, len(kv))
	for _, e := range kv {
		if i := strings.IndexByte(e, '='); i >= 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return m
}

func TestBuildEnvSidecarSkillIsPlainName(t *testing.T) {
	s := New(nil, nil)

	// SkillName set (serve mode): SIDECAR_SKILL must be the plain name,
	// never the namespaced tracking Name (which contains a slash that
	// breaks sidecar filesystem-path construction).
	env := envMap(s.buildEnv(SidecarSpec{
		Name:      "__global__/skill-marketplace",
		SkillName: "skill-marketplace",
		Workdir:   "/proj",
	}, 1234))
	if got := env["SIDECAR_SKILL"]; got != "skill-marketplace" {
		t.Errorf("SIDECAR_SKILL = %q, want skill-marketplace", got)
	}
	if strings.Contains(env["SIDECAR_SKILL"], "/") {
		t.Errorf("SIDECAR_SKILL must not contain '/': %q", env["SIDECAR_SKILL"])
	}
	if env["OMAC_WORKDIR"] != "/proj" {
		t.Errorf("OMAC_WORKDIR = %q, want /proj", env["OMAC_WORKDIR"])
	}

	// SkillName empty (start mode): falls back to Name.
	env2 := envMap(s.buildEnv(SidecarSpec{Name: "slack"}, 1))
	if env2["SIDECAR_SKILL"] != "slack" {
		t.Errorf("fallback SIDECAR_SKILL = %q, want slack", env2["SIDECAR_SKILL"])
	}
}

// TestStopSidecarTracking verifies the bookkeeping of StopSidecar without
// spawning real processes: a Running with a nil Cmd.Process terminates as a
// no-op (terminate handles nil), so we can assert set membership directly.
func TestStopSidecarTracking(t *testing.T) {
	s := New(nil, nil)
	s.children = []*Running{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}

	if ok := s.StopSidecar("b", time.Second); !ok {
		t.Fatal("StopSidecar(b) returned false, want true")
	}
	if len(s.children) != 2 {
		t.Fatalf("after stop, len = %d, want 2", len(s.children))
	}
	for _, r := range s.children {
		if r.Name == "b" {
			t.Fatal("b still tracked after StopSidecar")
		}
	}

	// Stopping an unknown name is a no-op.
	if ok := s.StopSidecar("zzz", time.Second); ok {
		t.Fatal("StopSidecar(zzz) returned true, want false")
	}
	if len(s.children) != 2 {
		t.Fatalf("len changed on no-op stop: %d", len(s.children))
	}

	// Remaining order preserved (a, c).
	if s.children[0].Name != "a" || s.children[1].Name != "c" {
		t.Fatalf("order not preserved: %s, %s", s.children[0].Name, s.children[1].Name)
	}
}

func TestEnsureExecutable(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "scripts", "sidecar.py")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/usr/bin/env python3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Not executable yet.
	if fi, _ := os.Stat(script); fi.Mode()&0o100 != 0 {
		t.Fatal("precondition: script should not be executable")
	}
	ensureExecutable(dir, "./scripts/sidecar.py")
	fi, _ := os.Stat(script)
	if fi.Mode()&0o100 == 0 {
		t.Error("ensureExecutable did not set the execute bit")
	}

	// Bare interpreter name: no-op (must not panic / touch anything).
	ensureExecutable(dir, "python3")

	// Path escaping the skill dir: ignored.
	outside := filepath.Join(t.TempDir(), "evil.sh")
	os.WriteFile(outside, []byte("x"), 0o644)
	ensureExecutable(dir, outside)
	if fi, _ := os.Stat(outside); fi.Mode()&0o100 != 0 {
		t.Error("ensureExecutable touched a file outside the skill dir")
	}
}

// capturingAuditor collects emitted events for inspection in tests.
type capturingAuditor struct {
	mu     sync.Mutex
	events []audit.Event
}

func (c *capturingAuditor) Emit(ev audit.Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}
func (c *capturingAuditor) Close() error      { return nil }
func (c *capturingAuditor) RunID() string     { return "test" }
func (c *capturingAuditor) NextSeq() uint64   { return uint64(len(c.events) + 1) }

func (c *capturingAuditor) countType(typ string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, ev := range c.events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// TestSelfTeratingSidecarEmitsProcessExit asserts that a sidecar which
// exits on its own (without StopSidecar/ShutdownAll being called) still
// produces a process.exit audit event. The spec says: "a process.exit
// event when that process terminates" — termination is the trigger, not
// the supervisor's shutdown.
func TestSelfTerminatingSidecarEmitsProcessExit(t *testing.T) {
	aud := &capturingAuditor{}
	s := New(nil, aud)

	// Spawn a process that exits immediately.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := &Running{
		Name:           "short-lived",
		Cmd:            cmd,
		startedAt:      time.Now(),
		auditSkill:     "short-lived",
		auditNamespace: "",
	}
	s.mu.Lock()
	s.children = append(s.children, r)
	s.mu.Unlock()

	// Start the reaper; it should emit process.exit when the child exits.
	s.watchChild(r)

	// Wait for the process to exit and the audit event to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if aud.countType(audit.TypeProcessExit) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := aud.countType(audit.TypeProcessExit); got != 1 {
		t.Fatalf("want 1 process.exit event for self-terminated sidecar, got %d", got)
	}
}

// TestStopSidecarDoesNotDoubleEmitProcessExit asserts that when a sidecar
// is stopped via StopSidecar after it already self-terminated and was
// audited by the reaper, a second process.exit event is NOT emitted.
func TestStopSidecarDoesNotDoubleEmitProcessExit(t *testing.T) {
	aud := &capturingAuditor{}
	s := New(nil, aud)

	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := &Running{
		Name:      "double",
		Cmd:       cmd,
		startedAt: time.Now(),
	}
	s.mu.Lock()
	s.children = append(s.children, r)
	s.mu.Unlock()

	s.watchChild(r)

	// Wait for self-termination + reaper emit.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if aud.countType(audit.TypeProcessExit) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Now StopSidecar: should not emit a second process.exit.
	s.StopSidecar("double", time.Second)
	if got := aud.countType(audit.TypeProcessExit); got != 1 {
		t.Fatalf("want 1 process.exit (no double-emit), got %d", got)
	}
}
