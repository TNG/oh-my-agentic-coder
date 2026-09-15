//go:build vuln

package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// TestSecurityServeActivateRejectsUnauthenticatedCaller extends
// TestSecurityReloadRejectsUnauthenticatedCaller's property (start-mode
// /__omac__/reload) to serve-mode's /__omac__/activate — the route that
// brings a new directory under serve's management. Reachable from every
// process on the machine, since serve's control plane is unauthenticated
// loopback by design.
func TestSecurityServeActivateRejectsUnauthenticatedCaller(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	wd := t.TempDir()

	srv := httptest.NewServer(s.controlMux())
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/__omac__/activate", "application/json",
		strings.NewReader(`{"dir":"`+wd+`"}`))
	if err != nil {
		t.Fatalf("POST /__omac__/activate: %v", err)
	}
	defer resp.Body.Close()

	s.mu.RLock()
	_, activated := s.dirs[wd]
	s.mu.RUnlock()

	if activated {
		t.Errorf("an unauthenticated POST /__omac__/activate (status %d) activated directory %s: any process on the machine can bring a new directory under this serve session's management", resp.StatusCode, wd)
	}
}

// TestSecurityStartActivateRejectsUnauthenticatedCaller extends
// TestSecurityReloadRejectsUnauthenticatedCaller's property to start-mode's
// /__omac__/activate, which the plugin bridge posts to and which — like
// /__omac__/reload — calls r.reload() unconditionally.
func TestSecurityStartActivateRejectsUnauthenticatedCaller(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	isolateHome(t)
	t.Setenv("TMPDIR", t.TempDir())
	workdir := t.TempDir()
	r, _ := newLiveReloader(t, workdir)
	stageForgedSkill(t, workdir, "pwn", "pwn")

	controlURL, closeControl, ok := startControlPlane(r)
	if !ok {
		t.Fatal("startControlPlane could not bind: the fixture is broken, not the security property")
	}
	t.Cleanup(closeControl)

	resp, err := http.Post(controlURL+"/__omac__/activate", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /__omac__/activate: %v", err)
	}
	defer resp.Body.Close()

	if r.facade.HasRoute("", "pwn") {
		t.Errorf("an unauthenticated POST /__omac__/activate (status %d) changed the session's routing table", resp.StatusCode)
	}
}

// TestSecurityServeDeactivateRejectsUnauthenticatedCaller extends the
// control-plane-auth property to serve-mode's /__omac__/deactivate, which
// removes a directory (and its token) from serve's management entirely.
func TestSecurityServeDeactivateRejectsUnauthenticatedCaller(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	wd := t.TempDir()
	stageSkillWithSecret(t, wd, "slack")

	// Control: activation itself works and the dir is tracked, so the
	// unauthenticated-deactivate assertion below is about the auth gap,
	// not a fixture that never got the directory active in the first
	// place.
	if _, err := s.activate(wd); err != nil {
		t.Fatalf("control: s.activate: %v", err)
	}
	s.mu.RLock()
	_, active := s.dirs[wd]
	s.mu.RUnlock()
	if !active {
		t.Fatalf("control: directory was not activated: the fixture is broken, not the security property")
	}

	srv := httptest.NewServer(s.controlMux())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/__omac__/deactivate", "application/json", strings.NewReader(`{"dir":"`+wd+`"}`))
	if err != nil {
		t.Fatalf("POST /__omac__/deactivate: %v", err)
	}
	defer resp.Body.Close()

	s.mu.RLock()
	_, stillActive := s.dirs[wd]
	s.mu.RUnlock()
	if !stillActive {
		t.Errorf("an unauthenticated POST /__omac__/deactivate (status %d) removed directory %s from serve's management: any process on the machine can tear down this session's active directories", resp.StatusCode, wd)
	}
}

// TestSecurityServeReloadRejectsUnauthenticatedCaller extends the
// control-plane-auth property to serve-mode's /__omac__/reload, which
// tears down and re-activates a directory (deactivate+activate), issuing a
// fresh per-dir token in the process — an observable side effect an
// unauthenticated caller should not be able to trigger.
func TestSecurityServeReloadRejectsUnauthenticatedCaller(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	wd := t.TempDir()
	stageSkillWithSecret(t, wd, "slack")
	if _, err := s.activate(wd); err != nil {
		t.Fatalf("control: s.activate: %v", err)
	}
	s.mu.RLock()
	tokenBefore := s.dirs[wd].Token
	s.mu.RUnlock()
	if tokenBefore == "" {
		t.Fatalf("control: activation did not issue a token: the fixture is broken, not the security property")
	}

	srv := httptest.NewServer(s.controlMux())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/__omac__/reload", "application/json", strings.NewReader(`{"dir":"`+wd+`"}`))
	if err != nil {
		t.Fatalf("POST /__omac__/reload: %v", err)
	}
	defer resp.Body.Close()

	s.mu.RLock()
	d, stillActive := s.dirs[wd]
	tokenAfter := ""
	if stillActive {
		tokenAfter = d.Token
	}
	s.mu.RUnlock()

	if !stillActive || tokenAfter != tokenBefore {
		t.Errorf("an unauthenticated POST /__omac__/reload (status %d) reloaded directory %s (token %q -> %q): any process on the machine can force a live directory through deactivate+reactivate", resp.StatusCode, wd, tokenBefore, tokenAfter)
	}
}

// TestSecurityServeReloadGlobalRejectsUnauthenticatedCaller extends the
// control-plane-auth property to serve-mode's /__omac__/reload-global,
// which unconditionally clears and rebuilds the global skill set.
func TestSecurityServeReloadGlobalRejectsUnauthenticatedCaller(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)

	// A hand-inserted global route stands in for one that was actually
	// discovered — reloadGlobals unconditionally replaces the whole map
	// with a fresh discovery pass, so this entry vanishing is the
	// observable proof that a reload ran, independent of any real skill
	// registry state.
	s.mu.Lock()
	s.global["probe"] = &skillRoute{Name: "probe", Mount: "probe", Namespace: facade.GlobalNamespace, State: facade.RouteBroken}
	s.mu.Unlock()

	srv := httptest.NewServer(s.controlMux())
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL+"/__omac__/reload-global", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /__omac__/reload-global: %v", err)
	}
	defer resp.Body.Close()

	s.mu.RLock()
	_, stillThere := s.global["probe"]
	s.mu.RUnlock()

	if !stillThere {
		t.Errorf("an unauthenticated POST /__omac__/reload-global (status %d) cleared and rebuilt the global skill set: any process on the machine can force this", resp.StatusCode)
	}
}

// TestSecurityStartDeactivateRejectsUnauthenticatedCaller extends the
// property to start-mode's /__omac__/deactivate, a distinct route sharing
// handleActivate (the same reload-triggering handler as /activate).
func TestSecurityStartDeactivateRejectsUnauthenticatedCaller(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	isolateHome(t)
	t.Setenv("TMPDIR", t.TempDir())
	workdir := t.TempDir()
	r, _ := newLiveReloader(t, workdir)
	stageForgedSkill(t, workdir, "pwn", "pwn")

	controlURL, closeControl, ok := startControlPlane(r)
	if !ok {
		t.Fatal("startControlPlane could not bind: the fixture is broken, not the security property")
	}
	t.Cleanup(closeControl)

	resp, err := http.Post(controlURL+"/__omac__/deactivate", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /__omac__/deactivate: %v", err)
	}
	defer resp.Body.Close()

	if r.facade.HasRoute("", "pwn") {
		t.Errorf("an unauthenticated POST /__omac__/deactivate (status %d) changed the session's routing table", resp.StatusCode)
	}
}

// TestSecurityStartReloadGlobalRejectsUnauthenticatedCaller extends the
// property to start-mode's /__omac__/reload-global, which — like
// /__omac__/reload and /__omac__/activate — calls r.reload() unconditionally.
func TestSecurityStartReloadGlobalRejectsUnauthenticatedCaller(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	isolateHome(t)
	t.Setenv("TMPDIR", t.TempDir())
	workdir := t.TempDir()
	r, _ := newLiveReloader(t, workdir)
	stageForgedSkill(t, workdir, "pwn", "pwn")

	controlURL, closeControl, ok := startControlPlane(r)
	if !ok {
		t.Fatal("startControlPlane could not bind: the fixture is broken, not the security property")
	}
	t.Cleanup(closeControl)

	resp, err := http.Post(controlURL+"/__omac__/reload-global", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /__omac__/reload-global: %v", err)
	}
	defer resp.Body.Close()

	if r.facade.HasRoute("", "pwn") {
		t.Errorf("an unauthenticated POST /__omac__/reload-global (status %d) changed the session's routing table", resp.StatusCode)
	}
}
