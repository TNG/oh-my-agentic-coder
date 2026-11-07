package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// The serve-mode directory token.
//
// `omac serve` hosts many workdirs behind one facade port. A session's skills
// are reachable only under its directory token, a random 16-byte value minted
// at activation: the token is the namespace key and the whole of the access
// control between one workdir's skills and another's. Those skills can hold
// API credentials, so a session holding a second session's token has that
// session's reach.
//
// The control plane's port is opened into every sandbox on purpose — the
// harness plugin activates and reloads through it — so a confined agent is an
// intended client of these endpoints. GET /__omac__/dirs was already fixed to
// list directories without their tokens for exactly that reason. Re-activating
// a directory that is already active reaches the same token by a different
// route: the caller names a directory it does not own, and the response is
// that directory's live manifest, token included.
//
// Nothing about a POST identifies its sender. The session that owns the
// directory was handed its token when it activated and has no need to be told
// again, so re-issuing it can only ever inform someone else.
//
// The control token, though, is daemon-wide: every activated session holds
// it, so it proves only "one of this daemon's sessions" — never "the session
// that owns that directory". The routes that end or re-run a live namespace
// (reload, deactivate) therefore also demand the target directory's own
// token, a reload response never carries one, and the /dirs and /global
// listings, which name every live workdir, sit behind the control token
// like every other control-plane route.

// controlRequest performs one HTTP round trip against the control plane.
// controlToken and dirToken are sent as headers when non-empty; leaving
// controlToken empty models a caller that holds no daemon credential. It
// returns the response status, the raw body, and the body decoded as a JSON
// object (nil when the body is not one).
func controlRequest(t *testing.T, method, url, body, controlToken, dirToken string) (int, string, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if controlToken != "" {
		req.Header.Set("X-Omac-Control-Token", controlToken)
	}
	if dirToken != "" {
		req.Header.Set("X-Omac-Dir-Token", dirToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response from %s %s: %v", method, url, err)
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		out = nil
	}
	return resp.StatusCode, string(raw), out
}

// activateWorkdirForTest activates workdir in-process — the way a session's
// own first activation runs — and returns the dir token that activation
// emitted. A workdir without a live token would make the properties below
// untestable, so a missing token is a broken fixture, not a pass.
func activateWorkdirForTest(t *testing.T, s *serveServer, workdir string) string {
	t.Helper()
	stageSkillWithSecret(t, workdir, "slack")
	manifest, err := s.activate(workdir)
	if err != nil {
		t.Fatalf("activate %s: %v", workdir, err)
	}
	token, _ := manifest["dir_token"].(string)
	if token == "" {
		t.Fatal("the first activation returned no dir token: the fixture is broken, not the security property")
	}
	return token
}

// TestSecurityActivateDoesNotDiscloseDirToken asserts that activating a
// directory that is already active does not hand its token to the caller.
func TestSecurityActivateDoesNotDiscloseDirToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)

	victimDir := t.TempDir()
	stageSkillWithSecret(t, victimDir, "slack")

	// The victim's own session activates first and receives the token, which
	// is how it reaches its own skills.
	manifest, err := s.activate(victimDir)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	token, _ := manifest["dir_token"].(string)
	if token == "" {
		t.Fatal("the first activation returned no dir token: the fixture is broken, not the security property")
	}

	control := httptest.NewServer(s.controlMux())
	t.Cleanup(control.Close)

	// Control: any session holding the control token can see which
	// directories are live, which is what makes the check below meaningful —
	// a second session needs only the path.
	dirsStatus, dirsRaw, dirs := controlRequest(t, http.MethodGet, control.URL+"/__omac__/dirs", "", s.controlToken, "")
	if dirsStatus != http.StatusOK {
		t.Fatalf("GET /__omac__/dirs with the control token answered %d (body=%s): the fixture is broken, not the security property", dirsStatus, dirsRaw)
	}
	if !strings.Contains(fmt.Sprint(dirs), victimDir) {
		t.Fatalf("the active directory was not listed at all (%v): the fixture is broken", dirs)
	}
	if strings.Contains(fmt.Sprint(dirs), token) {
		t.Errorf("GET /__omac__/dirs disclosed the dir token: the directory listing hands out the key to every active session's skills")
	}

	// A second session names the victim's directory. It holds the daemon-wide
	// control token — every session does — but no proof of ownership.
	replayStatus, replayRaw, replay := controlRequest(t, http.MethodPost, control.URL+"/__omac__/activate", fmt.Sprintf(`{"dir":%q}`, victimDir), s.controlToken, "")
	if replayStatus != http.StatusOK {
		t.Fatalf("re-activation answered %d (body=%s): the fixture is broken, not the security property", replayStatus, replayRaw)
	}

	// Asserting absence rather than difference: a token that is merely
	// rotated on re-activation is still a live namespace key handed to a
	// caller that proved nothing, and it additionally cuts off the session
	// that owns the directory.
	if got, _ := replay["dir_token"].(string); got != "" {
		t.Errorf("re-activating an already-active directory returned the live token %q: a caller that knows only the path gains that session's skills, credentials included", got)
	}
}

// TestSecurityReloadRequiresDirToken asserts that reloading a live workdir
// demands proof that the caller owns that workdir's namespace: its dir token,
// which only the session that activated the workdir holds. The daemon-wide
// control token is not that proof, and a reload denied for lack of it must
// leave the workdir's state untouched.
func TestSecurityReloadRequiresDirToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	wd := t.TempDir()
	token := activateWorkdirForTest(t, s, wd)

	control := httptest.NewServer(s.controlMux())
	t.Cleanup(control.Close)
	body := fmt.Sprintf(`{"dir":%q}`, wd)

	for name, dirToken := range map[string]string{
		"no dir token":    "",
		"wrong dir token": "definitely-not-the-live-token",
	} {
		status, raw, _ := controlRequest(t, http.MethodPost, control.URL+"/__omac__/reload", body, s.controlToken, dirToken)
		if status != http.StatusForbidden {
			t.Errorf("%s: POST /__omac__/reload for a live workdir answered %d, want 403: re-running a namespace requires proof that the caller owns it (body=%s)", name, status, raw)
		}
		if strings.Contains(raw, "dir_token") {
			t.Errorf("%s: the rejected reload response carried a dir_token: a caller that failed the ownership check must learn nothing about the namespace (body=%s)", name, raw)
		}
		s.mu.RLock()
		d, active := s.dirs[wd]
		live := ""
		if active {
			live = d.Token
		}
		s.mu.RUnlock()
		if !active || live != token {
			t.Errorf("%s: the rejected reload disturbed the live namespace (active=%v, token preserved=%v): rejection must happen before any side effect", name, active, live == token)
		}
	}

	// The owning session presents its token and keeps the ability to reload
	// its own workdir.
	status, raw, _ := controlRequest(t, http.MethodPost, control.URL+"/__omac__/reload", body, s.controlToken, token)
	if status != http.StatusOK {
		t.Errorf("reload with the directory's own dir token answered %d, want 200 (body=%s)", status, raw)
	}
}

// TestSecurityDeactivateRequiresDirToken asserts the same ownership proof for
// deactivation, which ends a live workdir's namespace entirely: a caller with
// only the control token must not be able to shut down a namespace it does
// not own.
func TestSecurityDeactivateRequiresDirToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	wd := t.TempDir()
	token := activateWorkdirForTest(t, s, wd)

	control := httptest.NewServer(s.controlMux())
	t.Cleanup(control.Close)
	body := fmt.Sprintf(`{"dir":%q}`, wd)

	for name, dirToken := range map[string]string{
		"no dir token":    "",
		"wrong dir token": "definitely-not-the-live-token",
	} {
		status, raw, _ := controlRequest(t, http.MethodPost, control.URL+"/__omac__/deactivate", body, s.controlToken, dirToken)
		if status != http.StatusForbidden {
			t.Errorf("%s: POST /__omac__/deactivate for a live workdir answered %d, want 403: ending a namespace requires proof that the caller owns it (body=%s)", name, status, raw)
		}
		s.mu.RLock()
		_, active := s.dirs[wd]
		s.mu.RUnlock()
		if !active {
			t.Errorf("%s: the rejected deactivate removed the live namespace: rejection must happen before any side effect", name)
		}
	}

	// The owning session presents its token and keeps the ability to
	// deactivate its own workdir.
	status, raw, _ := controlRequest(t, http.MethodPost, control.URL+"/__omac__/deactivate", body, s.controlToken, token)
	if status != http.StatusOK {
		t.Errorf("deactivate with the directory's own dir token answered %d, want 200 (body=%s)", status, raw)
	}
	s.mu.RLock()
	_, stillActive := s.dirs[wd]
	s.mu.RUnlock()
	if stillActive {
		t.Error("deactivate with the directory's own dir token left the workdir active")
	}
}

// TestSecurityReloadResponseDoesNotDiscloseDirToken asserts that a reload
// response never carries the workdir's namespace token: the session that owns
// the workdir already holds it, and nothing about the response can prove who
// else is reading it. A first activation of a previously unknown workdir must
// still emit the token exactly once — that one-time handover is how a session
// legitimately obtains its key.
func TestSecurityReloadResponseDoesNotDiscloseDirToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	wd := t.TempDir()
	token := activateWorkdirForTest(t, s, wd)

	control := httptest.NewServer(s.controlMux())
	t.Cleanup(control.Close)

	// The owner's own reload, presenting its dir token.
	status, raw, out := controlRequest(t, http.MethodPost, control.URL+"/__omac__/reload", fmt.Sprintf(`{"dir":%q}`, wd), s.controlToken, token)
	if status != http.StatusOK {
		t.Fatalf("reload with the directory's own dir token answered %d (body=%s): the fixture is broken, not the security property", status, raw)
	}
	if got, _ := out["dir_token"].(string); got != "" || strings.Contains(raw, "dir_token") {
		t.Errorf("the reload response carried the namespace token (%q): the owner already holds it, so a token here can only reach a caller that does not own the namespace", got)
	}

	// Control: a first activation of a previously unknown workdir still emits
	// the dir token exactly once — the fix must not break the one legitimate
	// way a session receives its key.
	fresh := t.TempDir()
	status, raw, out = controlRequest(t, http.MethodPost, control.URL+"/__omac__/activate", fmt.Sprintf(`{"dir":%q}`, fresh), s.controlToken, "")
	if status != http.StatusOK {
		t.Fatalf("first activation answered %d (body=%s): the fixture is broken, not the security property", status, raw)
	}
	if got, _ := out["dir_token"].(string); got == "" {
		t.Error("a first activation returned no dir token: a session must still receive its namespace key when it activates")
	}
}

// TestSecurityDirsAndGlobalRequireControlToken asserts that the control
// plane's read endpoints, which enumerate the live workdirs and the global
// skill surface, refuse callers without the control token like every other
// /__omac__/* route — and that what they return carries no namespace tokens.
func TestSecurityDirsAndGlobalRequireControlToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	wd := t.TempDir()
	token := activateWorkdirForTest(t, s, wd)

	control := httptest.NewServer(s.controlMux())
	t.Cleanup(control.Close)

	for name, path := range map[string]string{
		"GET /__omac__/dirs":   "/__omac__/dirs",
		"GET /__omac__/global": "/__omac__/global",
	} {
		status, raw, _ := controlRequest(t, http.MethodGet, control.URL+path, "", "", "")
		if status != http.StatusUnauthorized {
			t.Errorf("%s without the control token answered %d, want 401: the control plane must not serve callers without the daemon credential (body=%s)", name, status, raw)
		}
		if strings.Contains(raw, wd) {
			t.Errorf("%s without the control token named a live workdir: an unauthenticated caller must learn nothing (body=%s)", name, raw)
		}
	}

	// Authenticated, the listings serve their intended clients — and still
	// carry no namespace tokens.
	status, raw, out := controlRequest(t, http.MethodGet, control.URL+"/__omac__/dirs", "", s.controlToken, "")
	if status != http.StatusOK {
		t.Errorf("GET /__omac__/dirs with the control token answered %d, want 200 (body=%s)", status, raw)
	} else if !strings.Contains(fmt.Sprint(out), wd) {
		t.Errorf("the dir listing omitted the active workdir %s: the fixture is broken, not the security property", wd)
	}
	if strings.Contains(raw, token) || strings.Contains(raw, "dir_token") {
		t.Errorf("the authenticated dir listing carried a namespace token: %s", raw)
	}

	status, raw, out = controlRequest(t, http.MethodGet, control.URL+"/__omac__/global", "", s.controlToken, "")
	if status != http.StatusOK {
		t.Errorf("GET /__omac__/global with the control token answered %d, want 200 (body=%s)", status, raw)
	} else if _, ok := out["skills"]; !ok {
		t.Errorf("the global listing returned no skills list: the fixture is broken, not the security property (%s)", raw)
	}
	if strings.Contains(raw, token) {
		t.Errorf("the authenticated global listing carried a namespace token: %s", raw)
	}
}
