package cli

import (
	"encoding/json"
	"fmt"
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

// postJSON sends body to the control plane and decodes the JSON response.
func postJSON(t *testing.T, url, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response from %s: %v", url, err)
	}
	return out
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

	// Control, as a control-plane client: the path is all an attacker lacks,
	// and every sandboxed session can enumerate directories. The read carries
	// the control token every such client holds, so the fixture asserts the
	// no-token-leak property without depending on the listing being
	// reachable unauthenticated.
	status, dirs := getJSONControlAuthed(t, control.URL+"/__omac__/dirs")
	if status != http.StatusOK {
		t.Fatalf("control: GET /__omac__/dirs: status %d (body %v): the fixture is broken, not the security property", status, dirs)
	}
	if !strings.Contains(fmt.Sprint(dirs), victimDir) {
		t.Fatalf("the active directory was not listed at all (%v): the fixture is broken", dirs)
	}
	if strings.Contains(fmt.Sprint(dirs), token) {
		t.Errorf("GET /__omac__/dirs disclosed the dir token: the directory listing hands out the key to every active session's skills")
	}

	// A second session, or any local process, names the victim's directory.
	replay := postJSON(t, control.URL+"/__omac__/activate", fmt.Sprintf("{%q:%q}", "dir", victimDir))

	// Asserting absence rather than difference: a token that is merely
	// rotated on re-activation is still a live namespace key handed to a
	// caller that proved nothing, and it additionally cuts off the session
	// that owns the directory.
	if got, _ := replay["dir_token"].(string); got != "" {
		t.Errorf("re-activating an already-active directory returned the live token %q: a caller that knows only the path gains that session's skills, credentials included", got)
	}
}

// getJSONControlAuthed performs a GET with the control-plane token and
// returns the response status and decoded JSON body. Every sandboxed session
// holds this token, so it authenticates the control plane's callers in
// general, not the owner of any particular workdir.
func getJSONControlAuthed(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	addServeTestToken(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response from %s: %v", url, err)
	}
	return resp.StatusCode, out
}

// postJSONControlAuthed posts body with the control-plane bearer token and
// returns the response status and decoded JSON body. Every sandboxed session
// holds this token, so it authenticates the control plane's callers in
// general, not the owner of any particular workdir.
func postJSONControlAuthed(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build POST %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	addServeTestToken(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response from %s: %v", url, err)
	}
	return resp.StatusCode, out
}

// TestSecurityReloadDoesNotRemintDirToken asserts that a reload of an active
// directory preserves that directory's namespace: the reload response carries
// no dir_token, and the token issued at that directory's first activation
// keeps routing. The control token held by every control-plane client does not
// identify a workdir's owner, so a reload response must never issue the
// workdir's namespace key to whoever asked, and must not re-key the namespace
// the owning session already holds.
func TestSecurityReloadDoesNotRemintDirToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)

	wd := t.TempDir()
	stageSkillWithSecret(t, wd, "slack")

	manifest, err := s.activate(wd)
	if err != nil {
		t.Fatalf("control: activate: %v", err)
	}
	token, _ := manifest["dir_token"].(string)
	if token == "" {
		t.Fatal("control: first activation returned no dir token: the fixture is broken, not the security property")
	}

	control := httptest.NewServer(s.controlMux())
	t.Cleanup(control.Close)

	status, out := postJSONControlAuthed(t, control.URL+"/__omac__/reload",
		fmt.Sprintf("{%q:%q}", "dir", wd))
	if status != http.StatusOK {
		t.Fatalf("control: POST /__omac__/reload: status %d (body %v): the fixture is broken, not the security property", status, out)
	}

	// The token is issued exactly once, at first activation. A reload
	// response reaches whoever asked for the reload, so it must carry no
	// dir_token at all — a merely different token would be a live namespace
	// key in the hands of a caller that proved nothing.
	if got, _ := out["dir_token"].(string); got != "" {
		t.Errorf("reload response carried dir_token %q: a workdir's namespace key must not be issued a second time", got)
	}

	// The directory keeps its identity across reload: the token handed out
	// at first activation is still the one stored and still routes, so the
	// owning session is neither re-keyed nor cut off.
	s.mu.RLock()
	stored, active := "", false
	if d, ok := s.dirs[wd]; ok {
		stored, active = d.Token, true
	}
	s.mu.RUnlock()
	if !active || stored != token {
		t.Errorf("reload re-keyed the workdir namespace: active=%v, stored token %q, token issued at first activation %q", active, stored, token)
	}
	if !s.facade.HasRoute(token, "slack") {
		t.Error("the token issued at first activation no longer routes after reload: the owning session lost access to its own skills")
	}
}

// TestSecurityDeactivateActivatePairDoesNotRemintDirToken asserts the same
// property through an explicit deactivate followed by activate: the token
// issued at first activation stays bound to the directory for the life of the
// serve session, so the pair must neither hand a namespace key to the caller
// nor invalidate the token the owning session holds.
func TestSecurityDeactivateActivatePairDoesNotRemintDirToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)

	wd := t.TempDir()
	stageSkillWithSecret(t, wd, "slack")

	manifest, err := s.activate(wd)
	if err != nil {
		t.Fatalf("control: activate: %v", err)
	}
	token, _ := manifest["dir_token"].(string)
	if token == "" {
		t.Fatal("control: first activation returned no dir token: the fixture is broken, not the security property")
	}

	control := httptest.NewServer(s.controlMux())
	t.Cleanup(control.Close)

	if status, out := postJSONControlAuthed(t, control.URL+"/__omac__/deactivate",
		fmt.Sprintf("{%q:%q}", "dir", wd)); status != http.StatusOK {
		t.Fatalf("control: POST /__omac__/deactivate: status %d (body %v): the fixture is broken, not the security property", status, out)
	}

	status, out := postJSONControlAuthed(t, control.URL+"/__omac__/activate",
		fmt.Sprintf("{%q:%q}", "dir", wd))
	if status != http.StatusOK {
		t.Fatalf("control: POST /__omac__/activate: status %d (body %v): the fixture is broken, not the security property", status, out)
	}

	// Re-activation after deactivation is a re-activation like any other:
	// its response goes to whoever asked, so it must carry no dir_token.
	if got, _ := out["dir_token"].(string); got != "" {
		t.Errorf("activate response carried dir_token %q after a deactivate: a workdir's namespace key must not be issued a second time", got)
	}

	// The directory's namespace identity survives the pair: same token
	// stored, same token routing, owning session unaffected.
	s.mu.RLock()
	stored, active := "", false
	if d, ok := s.dirs[wd]; ok {
		stored, active = d.Token, true
	}
	s.mu.RUnlock()
	if !active || stored != token {
		t.Errorf("deactivate+activate re-keyed the workdir namespace: active=%v, stored token %q, token issued at first activation %q", active, stored, token)
	}
	if !s.facade.HasRoute(token, "slack") {
		t.Error("the token issued at first activation no longer routes after deactivate+activate: the owning session lost access to its own skills")
	}
}
