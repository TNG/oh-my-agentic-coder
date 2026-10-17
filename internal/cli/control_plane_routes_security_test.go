package cli

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// ---- control-plane invariants: token memory, minting, bounds, gating ----

// controlRoundTrip issues a request against a control-plane endpoint and
// returns the response status and body. An empty token sends the request
// without credentials.
func controlRoundTrip(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if token != "" {
		req.Header.Set("X-Omac-Control-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read response: %v", method, url, err)
	}
	return resp.StatusCode, string(raw)
}

// jsonStrField returns the named string field of a JSON object body, or ""
// when the body is not a JSON object or the field is absent.
func jsonStrField(raw, field string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	v, _ := m[field].(string)
	return v
}

// failingEntropyReader stands in for a CSPRNG that cannot produce bytes.
type failingEntropyReader struct{}

func (failingEntropyReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy source unavailable")
}

// mintEntropyHelperEnv marks the re-exec'd test binary that performs the
// actual mint under a failing entropy source. The mint must be observed
// from a child process: crypto/rand.Read reacts to a failing rand.Reader
// with an irrecoverable runtime fatal (Go issue 66821), so an in-process
// probe with the current mint would take the whole test binary down
// instead of reporting the violation.
const mintEntropyHelperEnv = "OMAC_TEST_MINT_ENTROPY_HELPER"

// TestSecurityMintTokenFailsClosedOnEntropyFailure guards token minting:
// when the crypto entropy source fails, minting must report the failure and
// produce no token value. Degraded tokens would defeat the purpose of
// every bearer-token check in the process.
func TestSecurityMintTokenFailsClosedOnEntropyFailure(t *testing.T) {
	if os.Getenv(mintEntropyHelperEnv) == "1" {
		mintTokenWithFailingEntropy(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSecurityMintTokenFailsClosedOnEntropyFailure$")
	cmd.Env = append(os.Environ(), mintEntropyHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("mintToken did not fail closed while the entropy source was failing (child process outcome: %v)\n%s", err, out)
	}
}

// mintTokenWithFailingEntropy is the child half of
// TestSecurityMintTokenFailsClosedOnEntropyFailure: mint a token while the
// entropy source fails and report every way the fail-closed contract can be
// broken. The mint is invoked through reflection so this file compiles both
// before the fix (token-only signature, no failure channel) and after it
// (token, error). The failure must surface through rand.Reader: that is the
// only seam through which a caller can observe and refuse the degraded
// mint instead of receiving a silently degraded token.
func mintTokenWithFailingEntropy(t *testing.T) {
	orig := rand.Reader
	rand.Reader = failingEntropyReader{}
	defer func() { rand.Reader = orig }()

	defer func() {
		if rec := recover(); rec != nil {
			t.Errorf("mintToken panicked under entropy failure (%v): the failure must reach the caller as an error so the operation can be refused", rec)
		}
	}()

	res := reflect.ValueOf(mintToken).Call(nil)
	if len(res) < 2 {
		t.Errorf("mintToken returns only a token value: an entropy failure cannot reach the caller, so the mint cannot fail closed")
		return
	}
	if res[1].IsNil() {
		t.Errorf("mintToken reported no error although the entropy source fails: session bearer tokens must not be minted without crypto-grade randomness")
	}
	if tok := res[0].String(); tok != "" {
		t.Errorf("mintToken produced a token (%q) although the entropy source fails: no token value may be produced without crypto-grade randomness", tok)
	}
}

// TestSecurityReloadDoesNotReissueDirToken guards the once-only emission of a
// directory token: the first activation hands the token to the caller that
// brought the directory online, and no later control-plane operation may
// hand out a token for the same directory again.
func TestSecurityReloadDoesNotReissueDirToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	wd := t.TempDir()
	stageSkillWithSecret(t, wd, "slack")

	first, err := s.activate(wd)
	if err != nil {
		t.Fatalf("control: s.activate: %v", err)
	}
	firstTok, _ := first["dir_token"].(string)
	if firstTok == "" {
		t.Fatalf("control: activation returned no dir_token: the fixture is broken, not the security property")
	}

	srv := httptest.NewServer(s.controlMux())
	t.Cleanup(srv.Close)

	// A reload of an already-activated directory must never respond with a
	// token for it.
	status, raw := controlRoundTrip(t, http.MethodPost, srv.URL+"/__omac__/reload", s.controlToken, fmt.Sprintf(`{"dir":%q}`, wd))
	if status != http.StatusOK {
		t.Fatalf("POST /__omac__/reload returned %d (%s): the control-token holder must still be able to reload", status, raw)
	}
	if tok := jsonStrField(raw, "dir_token"); tok != "" {
		t.Errorf("reload of an already-activated directory re-issued dir_token %q (first activation issued %q): a directory token is emitted exactly once per directory", tok, firstTok)
	}

	// The once-only guarantee must also survive a deactivate/reactivate
	// cycle: the directory keeps it for its lifetime.
	status, raw = controlRoundTrip(t, http.MethodPost, srv.URL+"/__omac__/deactivate", s.controlToken, fmt.Sprintf(`{"dir":%q}`, wd))
	if status != http.StatusOK {
		t.Fatalf("POST /__omac__/deactivate returned %d (%s): the control-token holder must still be able to deactivate", status, raw)
	}
	status, raw = controlRoundTrip(t, http.MethodPost, srv.URL+"/__omac__/activate", s.controlToken, fmt.Sprintf(`{"dir":%q}`, wd))
	if status != http.StatusOK {
		t.Fatalf("POST /__omac__/activate returned %d (%s): the control-token holder must still be able to reactivate", status, raw)
	}
	if tok := jsonStrField(raw, "dir_token"); tok != "" {
		t.Errorf("reactivation after a deactivation re-issued dir_token %q: a directory token is emitted exactly once per directory", tok)
	}

	// Control: the cycle must leave the directory active again, so the
	// assertions above are about token emission, not a broken cycle.
	s.mu.RLock()
	_, active := s.dirs[wd]
	s.mu.RUnlock()
	if !active {
		t.Errorf("reload/deactivate/reactivate left directory %s inactive", wd)
	}
}

// TestSecurityActivateEnforcesDirectoryCapAndRoot guards activation bounds:
// with --root configured, only directories under it may activate, and a
// serve session keeps a maximum number of active directories instead of
// growing without bound.
func TestSecurityActivateEnforcesDirectoryCapAndRoot(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	base := t.TempDir()
	allowedRoot := filepath.Join(base, "allowed")
	if err := os.MkdirAll(allowedRoot, 0o755); err != nil {
		t.Fatalf("mkdir allowed root: %v", err)
	}
	s.roots = []string{allowedRoot}

	srv := httptest.NewServer(s.controlMux())
	t.Cleanup(srv.Close)

	// Outside the configured root: refused, even for the control-token
	// holder.
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside dir: %v", err)
	}
	if status, raw := controlRoundTrip(t, http.MethodPost, srv.URL+"/__omac__/activate", s.controlToken, fmt.Sprintf(`{"dir":%q}`, outside)); status == http.StatusOK {
		t.Errorf("activation of a directory outside the configured --root was accepted (%d): %s", status, raw)
	}

	// Inside the root: distinct directories are accepted one after another
	// until the session's bound on active directories is reached. Some
	// activation within this probe being refused proves such a bound
	// exists; the probe sits well above any plausible default cap.
	const probe = 1000
	refused := false
	for i := 0; i < probe; i++ {
		dir := filepath.Join(allowedRoot, fmt.Sprintf("d%03d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir probe dir: %v", err)
		}
		status, _ := controlRoundTrip(t, http.MethodPost, srv.URL+"/__omac__/activate", s.controlToken, fmt.Sprintf(`{"dir":%q}`, dir))
		if i == 0 && status != http.StatusOK {
			t.Fatalf("first activation inside the root returned %d: the fixture must work before any bound is reached", status)
		}
		if status != http.StatusOK {
			refused = true
			break
		}
	}
	if !refused {
		t.Errorf("%d distinct directories were activated on one serve session and none was refused: activation must enforce a maximum number of active directories", probe)
	}
}

// TestSecuritySessionWriteRequiresControlToken guards the start-mode session
// endpoint: the recorded session id backs the post-exit resume hint, so only
// the session's control-token holder may change it.
func TestSecuritySessionWriteRequiresControlToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	isolateHome(t)
	t.Setenv("TMPDIR", t.TempDir()) // startControlPlane publishes its control-info file here
	workdir := t.TempDir()
	r, _ := newLiveReloader(t, workdir)

	controlURL, closeControl, ok := startControlPlane(r)
	if !ok {
		t.Fatal("startControlPlane could not bind: the fixture is broken, not the security property")
	}
	t.Cleanup(closeControl)

	status, raw := controlRoundTrip(t, http.MethodPost, controlURL+"/__omac__/session", "", `{"session":"ses_from_a_passerby"}`)
	if status != http.StatusUnauthorized {
		t.Errorf("tokenless POST /__omac__/session returned %d (%s), want 401", status, raw)
	}
	if got := r.reportedSession(); got != "" {
		t.Errorf("tokenless POST /__omac__/session changed the recorded session id (now %q): the endpoint must require the control token", got)
	}

	// Control: the control-token holder's report still lands, so the
	// assertion above is about authorization, not a broken endpoint.
	status, raw = controlRoundTrip(t, http.MethodPost, controlURL+"/__omac__/session", r.controlToken, `{"session":"ses_from_the_agent"}`)
	if status != http.StatusOK {
		t.Fatalf("authorized POST /__omac__/session returned %d (%s): the endpoint must keep working for the control-token holder", status, raw)
	}
	if got := r.reportedSession(); got != "ses_from_the_agent" {
		t.Errorf("authorized POST /__omac__/session did not record the session id (got %q)", got)
	}
}

// TestSecurityDirsEndpointRequiresControlToken guards the serve-mode
// directory listing: which directories a session manages is per-session
// state, so the endpoint must refuse callers that do not present the
// control token.
func TestSecurityDirsEndpointRequiresControlToken(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	wd := t.TempDir()
	stageSkillWithSecret(t, wd, "slack")
	if _, err := s.activate(wd); err != nil {
		t.Fatalf("control: s.activate: %v", err)
	}

	srv := httptest.NewServer(s.controlMux())
	t.Cleanup(srv.Close)

	status, raw := controlRoundTrip(t, http.MethodGet, srv.URL+"/__omac__/dirs", "", "")
	if status != http.StatusUnauthorized {
		t.Errorf("tokenless GET /__omac__/dirs returned %d (%s), want 401", status, raw)
	}
	if strings.Contains(raw, wd) {
		t.Errorf("tokenless GET /__omac__/dirs disclosed the active workdir %q: %s", wd, raw)
	}
	// Match the JSON key, not the substring: workdir paths may embed the
	// test's own name.
	if strings.Contains(strings.ToLower(raw), `"token"`) {
		t.Errorf("tokenless GET /__omac__/dirs response discloses a token field: %s", raw)
	}

	// Control: the control-token holder still gets the listing.
	status, raw = controlRoundTrip(t, http.MethodGet, srv.URL+"/__omac__/dirs", s.controlToken, "")
	if status != http.StatusOK || !strings.Contains(raw, wd) {
		t.Errorf("authorized GET /__omac__/dirs returned %d (%s): the control-token holder must still get the listing", status, raw)
	}
}
