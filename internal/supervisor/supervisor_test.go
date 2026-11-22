package supervisor

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
	"github.com/TNG/oh-my-agentic-coder/internal/sidecarauth"
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
	s := New(nil, nil, nil)

	// SkillName set (serve mode): SIDECAR_SKILL must be the plain name,
	// never the namespaced tracking Name (which contains a slash that
	// breaks sidecar filesystem-path construction).
	env := envMap(s.buildEnv(SidecarSpec{
		Name:      "__global__/skill-marketplace",
		SkillName: "skill-marketplace",
		Workdir:   "/proj",
	}, sidecarauth.Upstream{Port: 1234, Secret: "s3"}))
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
	env2 := envMap(s.buildEnv(SidecarSpec{Name: "slack"}, sidecarauth.Upstream{Port: 1, Secret: "s3"}))
	if env2["SIDECAR_SKILL"] != "slack" {
		t.Errorf("fallback SIDECAR_SKILL = %q, want slack", env2["SIDECAR_SKILL"])
	}
}

// TestStopSidecarTracking verifies the bookkeeping of StopSidecar without
// spawning real processes: a Running with a nil Cmd.Process terminates as a
// no-op (terminate handles nil), so we can assert set membership directly.
func TestStopSidecarTracking(t *testing.T) {
	s := New(nil, nil, nil)
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
func (c *capturingAuditor) Close() error    { return nil }
func (c *capturingAuditor) RunID() string   { return "test" }
func (c *capturingAuditor) NextSeq() uint64 { return uint64(len(c.events) + 1) }

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
	s := New(nil, aud, nil)

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
	s := New(nil, aud, nil)

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

// TestShutdownAllReapsLongRunningChildWithoutHanging is a regression test
// for a deadlock where watchChild's reaper goroutine and terminate() both
// called Cmd.Wait() concurrently on the same child. os/exec.Cmd.Wait must
// only ever have one caller in flight; a second concurrent caller can
// block forever even after the process has been killed. That bug only
// shows up for a child that is still running when shutdown starts (a
// self-terminated child, as in the tests above, has no live race to lose).
// Uses a long-running child (sleep) plus a hard test-level timeout so a
// regression fails fast instead of hanging the whole test binary.
func TestShutdownAllReapsLongRunningChildWithoutHanging(t *testing.T) {
	s := New(nil, nil, nil)

	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := &Running{Name: "long-lived", Cmd: cmd, startedAt: time.Now()}
	s.mu.Lock()
	s.children = append(s.children, r)
	s.mu.Unlock()

	// Reaper is now racing to Wait() on a child that is still alive —
	// exactly the state that triggered the deadlock.
	s.watchChild(r)

	done := make(chan struct{})
	go func() {
		s.ShutdownAll(2 * time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ShutdownAll did not return within 5s — terminate() likely deadlocked racing watchChild's Cmd.Wait()")
	}
}

// TestAuthorizerBlocksSpawnBeforeExec verifies the spawn-gate backstop (the
// authorizer passed to New): a refused spec must return the authorizer's error AND the
// child command must never execute — the security guarantee that no spawn
// path can reach exec without approval, even if a caller skips its own
// pre-flight check. See internal/skilltrust.
func TestAuthorizerBlocksSpawnBeforeExec(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	spec := SidecarSpec{
		Name:     "denied",
		SkillDir: dir,
		// If this ever runs, it leaves proof.
		Command: []string{"sh", "-c", "touch '" + marker + "'; sleep 30"},
		Health:  config.HealthSpec{},
		LogPath: filepath.Join(dir, "log"),
	}

	denied := errors.New("nope")
	s := New(nil, nil, func(SidecarSpec) error { return denied })

	// AddSidecar must refuse.
	if _, err := s.AddSidecar(t.Context(), spec); err == nil {
		t.Fatal("AddSidecar spawned a denied skill")
	} else if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should carry the authorizer's message, got %v", err)
	}
	// StartAll must refuse too (the other funnel into startOne).
	if _, err := s.StartAll(t.Context(), []SidecarSpec{spec}); err == nil {
		t.Fatal("StartAll spawned a denied skill")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("denied command executed (marker present): stat err=%v", err)
	}

	// A permitting authorizer still allows the spawn (control): the command
	// exits immediately, so health fails — but the marker proves it ran.
	okMarker := filepath.Join(dir, "ran")
	spec2 := spec
	spec2.Name = "allowed"
	spec2.Command = []string{"sh", "-c", "touch '" + okMarker + "'"}
	spec2.Health = config.HealthSpec{InitialDelayMS: 10, IntervalMS: 10, TimeoutMS: 300}
	permitting := New(nil, nil, func(SidecarSpec) error { return nil })
	_, _ = permitting.AddSidecar(t.Context(), spec2) // health will fail; we only assert it ran
	if _, err := os.Stat(okMarker); err != nil {
		t.Errorf("permitted command did not run: %v", err)
	}
}

func TestBuildEnvAlwaysCarriesMintedSecret(t *testing.T) {
	s := New(nil, nil, nil)
	env := envMap(s.buildEnv(SidecarSpec{Name: "x"}, sidecarauth.Upstream{Port: 1, Secret: "minted"}))
	if env[sidecarauth.EnvVar] != "minted" {
		t.Errorf("%s = %q, want minted", sidecarauth.EnvVar, env[sidecarauth.EnvVar])
	}
}

func TestBuildEnvSecretCannotBeOverridden(t *testing.T) {
	t.Setenv(sidecarauth.EnvVar, "host")
	s := New([]string{sidecarauth.EnvVar}, nil, nil)
	env := envMap(s.buildEnv(SidecarSpec{
		Name:           "x",
		EnvPassthrough: []string{sidecarauth.EnvVar},
		Config:         map[string]string{sidecarauth.EnvVar: "cfg"},
		Secrets:        map[string]secrets.Secret{sidecarauth.EnvVar: secrets.NewSecretString("sec")},
	}, sidecarauth.Upstream{Port: 1, Secret: "minted"}))
	if env[sidecarauth.EnvVar] != "minted" {
		t.Errorf("%s = %q, want minted", sidecarauth.EnvVar, env[sidecarauth.EnvVar])
	}
}

func TestStartOneRefusesReservedNames(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	base := SidecarSpec{
		Name:     "bad",
		SkillDir: dir,
		Command:  []string{"sh", "-c", "touch '" + marker + "'"},
		LogPath:  filepath.Join(dir, "log"),
	}
	for name, mutate := range map[string]func(*SidecarSpec){
		"secrets": func(sp *SidecarSpec) {
			sp.Secrets = map[string]secrets.Secret{"omac_sidecar_secret": secrets.NewSecretString("x")}
		},
		"config":          func(sp *SidecarSpec) { sp.Config = map[string]string{sidecarauth.EnvVar: "x"} },
		"env_passthrough": func(sp *SidecarSpec) { sp.EnvPassthrough = []string{sidecarauth.EnvVar} },
	} {
		t.Run(name, func(t *testing.T) {
			spec := base
			mutate(&spec)
			_, err := New(nil, nil, nil).AddSidecar(t.Context(), spec)
			if err == nil || !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("err = %v, want reserved-name refusal", err)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatal("command ran despite reserved name")
			}
		})
	}
}

func upstreamOf(t *testing.T, srv *httptest.Server, secret string) sidecarauth.Upstream {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return sidecarauth.Upstream{Port: port, Secret: secret}
}

var fastHealth = config.HealthSpec{InitialDelayMS: 1, IntervalMS: 10, TimeoutMS: 300}

func TestWaitHealthSendsSecretAndRequiresEnforcement(t *testing.T) {
	enforcing, err := sidecarauth.Require("s3", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(enforcing)
	defer srv.Close()
	if err := waitHealth(t.Context(), upstreamOf(t, srv, "s3"), fastHealth, true); err != nil {
		t.Fatalf("enforcing sidecar: %v", err)
	}
	if err := waitHealth(t.Context(), upstreamOf(t, srv, "wrong"), fastHealth, false); err == nil {
		t.Fatal("probe must send the minted secret and need a 2xx")
	}
}

func TestWaitHealthFailsWhenSidecarIgnoresSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	up := upstreamOf(t, srv, "s3")
	if err := waitHealth(t.Context(), up, fastHealth, false); err != nil {
		t.Fatalf("without RequireSecret an open sidecar passes: %v", err)
	}
	err := waitHealth(t.Context(), up, fastHealth, true)
	if err == nil || !strings.Contains(err.Error(), "does not enforce") {
		t.Fatalf("err = %v, want enforcement failure", err)
	}
}

func TestRunningDoneClosesWhenChildExits(t *testing.T) {
	s := New(nil, &capturingAuditor{}, nil)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := &Running{Name: "done", Cmd: cmd, startedAt: time.Now()}
	s.watchChild(r)

	select {
	case <-r.Done():
		t.Fatal("Done closed while the child is still running")
	case <-time.After(50 * time.Millisecond):
	}

	_ = cmd.Process.Kill()
	select {
	case <-r.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed after the child exited")
	}
}
