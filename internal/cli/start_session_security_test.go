package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// The start-mode session-report boundary.
//
// The harness plugin reports the id of the session it created to
// /__omac__/session; the id is stored and later advertised in the post-exit
// continue hint, whose text ends up in command lines the user runs. Both
// properties below follow from that: the route is a control-plane route and
// must be gated like its siblings, and only plain identifiers (the session
// package's grammar, the one enumeration already applies) may be stored or
// printed.

// TestSecuritySessionEndpointRequiresControlToken asserts that the
// session-report route refuses a caller that does not present the
// per-session control token, like every other control-plane route.
func TestSecuritySessionEndpointRequiresControlToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	isolateHome(t)
	t.Setenv("TMPDIR", t.TempDir()) // startControlPlane publishes the control-info file
	workdir := t.TempDir()
	r, _ := newLiveReloader(t, workdir)

	// The production control plane, not a hand-copied mux: a test that
	// served its own routes would not see an auth layer added to
	// startControlPlane.
	controlURL, closeControl, ok := startControlPlane(r)
	if !ok {
		t.Fatal("startControlPlane could not bind: the fixture is broken, not the security property")
	}
	t.Cleanup(closeControl)

	resp, err := http.Post(controlURL+"/__omac__/session", "application/json",
		strings.NewReader(`{"session":"ses_anon"}`))
	if err != nil {
		t.Fatalf("POST /__omac__/session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /__omac__/session without the control token returned %d, want 401: a control-plane route must not act on a request that proves nothing about its caller", resp.StatusCode)
	}
	if got := r.reportedSession(); got != "" {
		t.Errorf("a session report posted without the control token was stored (%q)", got)
	}

	// Control: with the token, the plugin's report is accepted — the route
	// is gated, not gone.
	req, err := http.NewRequest(http.MethodPost, controlURL+"/__omac__/session",
		strings.NewReader(`{"session":"ses_plugin"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Omac-Control-Token", r.controlToken)
	authResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /__omac__/session with token: %v", err)
	}
	defer authResp.Body.Close()
	if authResp.StatusCode != http.StatusOK {
		t.Errorf("POST /__omac__/session with the control token returned %d, want 200", authResp.StatusCode)
	}
	if got := r.reportedSession(); got != "ses_plugin" {
		t.Errorf("a token-bearing session report was not stored (got %q)", got)
	}
}

// TestSecuritySessionRejectsInvalidSessionID asserts that handleSession
// applies the session identifier grammar at the storage boundary: an id that
// is not a plain identifier is refused with 400 and storage is left
// untouched, while a well-formed id is stored as before.
func TestSecuritySessionRejectsInvalidSessionID(t *testing.T) {
	r := newStartReloaderForTest(t)

	post := func(id string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]string{"session": id})
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/__omac__/session", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.handleSession(w, req)
		return w
	}

	// Control: a well-formed id is accepted and stored, so the rejections
	// below are the grammar at work, not a handler that refuses everything.
	if w := post("ses_wellformed"); w.Code != http.StatusOK {
		t.Fatalf("control: a well-formed session id was rejected (status %d): the fixture is broken, not the security property", w.Code)
	}
	if got := r.reportedSession(); got != "ses_wellformed" {
		t.Fatalf("control: a well-formed session id was not stored (got %q): the fixture is broken, not the security property", got)
	}

	// Each of these fails the session identifier grammar: embedded
	// whitespace, a leading dash, and a leading double dash.
	for _, id := range []string{"ses spaced", "-ses", "--ses"} {
		if w := post(id); w.Code != http.StatusBadRequest {
			t.Errorf("session report %q was accepted (status %d): an id that fails the session identifier grammar must be refused at this boundary", id, w.Code)
		}
		if got := r.reportedSession(); got != "ses_wellformed" {
			t.Errorf("session report %q replaced the stored id (got %q): a rejected report must leave storage untouched", id, got)
		}
	}
}

// captureHint drives the real printContinueHint with stderr captured and
// returns what it printed.
func captureHint(t *testing.T, harness config.Harness, resumedID string) string {
	t.Helper()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	defer null.Close()
	stderr, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	t.Cleanup(func() { stderr.Close() })
	env := &Env{Version: "test", Workdir: t.TempDir(), Stdout: null, Stderr: writer, Stdin: null}
	printContinueHint(env, harness, resumedID, nil)
	if err := writer.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	out, err := io.ReadAll(stderr)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	return string(out)
}

// TestSecurityContinueHintSkipsUnsafeSessionID asserts defense in depth at
// the hint itself: a resumed id that fails the identifier grammar must not
// be embedded in the printed hint.
func TestSecurityContinueHintSkipsUnsafeSessionID(t *testing.T) {
	h, ok := config.LookupHarness("opencode")
	if !ok || h.Session == nil || len(h.Session.ContinueArgs) == 0 {
		t.Fatal("opencode harness or its session spec is missing: the fixture is broken, not the security property")
	}

	// Control: a well-formed id still produces the hint, so suppression is
	// owed to the id, not to a hint path that never prints.
	if out := captureHint(t, h, "ses_wellformed"); !strings.Contains(out, "ses_wellformed") {
		t.Fatalf("control: no continue hint was printed for a well-formed id (got %q): the fixture is broken, not the security property", out)
	}

	for _, id := range []string{"ses spaced", "-ses", "--ses"} {
		if out := captureHint(t, h, id); strings.Contains(out, id) {
			t.Errorf("the continue hint embedded session id %q, which fails the identifier grammar: such an id must never appear in printed command text (got %q)", id, out)
		}
	}
}
