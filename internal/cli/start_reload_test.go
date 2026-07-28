package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newStartReloaderForTest(t *testing.T) *startReloader {
	t.Helper()
	isolateHome(t)
	return &startReloader{
		env:     makeEnv(t.TempDir()),
		mounted: map[string]string{},
	}
}

func TestStartReloaderMountedTracking(t *testing.T) {
	r := newStartReloaderForTest(t)
	if r.isMounted("slack") {
		t.Fatal("nothing should be mounted yet")
	}
	r.markMounted("slack", "slack")
	r.markMounted("email", "email")
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
	r.markMounted("slack", "slack")
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

func TestStartReloaderActivateNot404(t *testing.T) {
	r := newStartReloaderForTest(t)
	r.markMounted("slack", "slack")
	mux := r.startTestMux()

	// The plugin (built for serve) calls activate; start must answer 200 with
	// a serve-shaped manifest, not 404.
	body := `{"dir":"` + r.env.Workdir + `"}`
	req := httptest.NewRequest("POST", "/__omac__/activate", stringReader(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("activate status=%d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{`"skills"`, `"dir_token"`, `"slack"`, `"base"`} {
		if !contains(out, want) {
			t.Errorf("activate manifest missing %s: %s", want, out)
		}
	}
}

func TestStartReloaderSessionReport(t *testing.T) {
	r := newStartReloaderForTest(t)
	mux := r.startTestMux()
	if r.reportedSession() != "" {
		t.Fatal("no session should be reported yet")
	}

	post := func(body string) int {
		req := httptest.NewRequest("POST", "/__omac__/session", stringReader(body))
		req.Header.Set("content-type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post(`{"session":"ses_abc"}`); code != 200 {
		t.Fatalf("session report status=%d, want 200", code)
	}
	if got := r.reportedSession(); got != "ses_abc" {
		t.Errorf("reportedSession=%q, want ses_abc", got)
	}
	// Last report wins (a run may touch more than one session).
	post(`{"session":"ses_def"}`)
	if got := r.reportedSession(); got != "ses_def" {
		t.Errorf("reportedSession=%q, want ses_def", got)
	}
	// An empty/malformed report must not clobber a known id.
	post(`{"session":""}`)
	post(`not json`)
	if got := r.reportedSession(); got != "ses_def" {
		t.Errorf("empty/bad report clobbered id: %q", got)
	}
}

// startTestMux builds the same routes startControlPlane wires, for testing.
func (r *startReloader) startTestMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/__omac__/reload", r.handleReload)
	m.HandleFunc("/__omac__/dirs", r.handleDirs)
	m.HandleFunc("/__omac__/activate", r.handleActivate)
	m.HandleFunc("/__omac__/deactivate", r.handleActivate)
	m.HandleFunc("/__omac__/reload-global", r.handleReloadGlobalStart)
	m.HandleFunc("/__omac__/session", r.handleSession)
	return m
}

func stringReader(s string) *strings.Reader { return strings.NewReader(s) }
func contains(s, sub string) bool           { return strings.Contains(s, sub) }
