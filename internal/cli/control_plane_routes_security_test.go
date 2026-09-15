//go:build vuln

package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
