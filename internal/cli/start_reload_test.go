package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newStartReloaderForTest(t *testing.T) *startReloader {
	t.Helper()
	isolateHome(t)
	return &startReloader{
		env:     makeEnv(t.TempDir()),
		mounted: map[string]struct{}{},
	}
}

func TestStartReloaderMountedTracking(t *testing.T) {
	r := newStartReloaderForTest(t)
	if r.isMounted("slack") {
		t.Fatal("nothing should be mounted yet")
	}
	r.markMounted("slack", "email")
	if !r.isMounted("slack") || !r.isMounted("email") {
		t.Error("markMounted did not record names")
	}
	if r.isMounted("jira") {
		t.Error("unexpected mount")
	}
}

func TestStartReloaderReloadSkipsMissingSecret(t *testing.T) {
	r := newStartReloaderForTest(t)
	wd := r.env.Workdir

	// Stage + register a skill that requires a secret (none stored) so
	// reload must classify it not-ready and NOT mount it.
	stageSkillWithSecret(t, wd, "slack")
	// Register it workdir-local so reload's registry scan finds it.
	if code := runRegister([]string{"slack", "--no-secrets"}, r.env); code != ExitOK {
		t.Fatalf("register exit=%d", code)
	}

	added := r.reload()
	if len(added) != 0 {
		t.Errorf("expected no skills mounted (missing secret), got %v", added)
	}
	if r.isMounted("slack") {
		t.Error("slack should not be mounted with a missing required secret")
	}
}

func TestStartReloaderDirsEndpoint(t *testing.T) {
	r := newStartReloaderForTest(t)
	r.markMounted("slack")
	mux := r.startTestMux()
	req := httptest.NewRequest("GET", "/__omac__/dirs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("dirs status=%d", rec.Code)
	}
	if body := rec.Body.String(); body == "" {
		t.Error("empty dirs body")
	}
}

// startTestMux builds the same routes startControlPlane wires, for testing.
func (r *startReloader) startTestMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/__omac__/reload", r.handleReload)
	m.HandleFunc("/__omac__/dirs", r.handleDirs)
	return m
}
