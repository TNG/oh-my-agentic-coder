package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
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
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
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

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ready"))
	}))
	t.Cleanup(upstream.Close)
	promoteToReady(t, s, victimDir, "slack", token, portOf(t, upstream.URL))

	control := httptest.NewServer(s.controlMux())
	t.Cleanup(control.Close)

	// Control: the directory is discoverable without its token, which is what
	// makes the disclosure below reachable — an attacker needs only the path.
	dirs := postJSONGet(t, control.URL+"/__omac__/dirs")
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
	replaySkills, _ := replay["skills"].([]any)
	for _, raw := range replaySkills {
		sk, _ := raw.(map[string]any)
		if sk["scope"] != "workdir" {
			continue
		}
		if _, ok := sk["base"]; ok {
			t.Errorf("re-activating an already-active directory returned a skill base URL: the URL carries the live namespace")
		}
		if _, ok := sk["socket_base"]; ok {
			t.Errorf("re-activating an already-active directory returned a skill socket base: the path carries the live namespace")
		}
	}
}

// TestSecurityActivateDoesNotDiscloseNamespaceOnRepeat asserts that a caller
// re-activating an already-active directory learns neither the dir token nor
// any per-skill URL that embeds the live namespace. The first activation is
// the owner's only disclosure; repeats must withhold the namespace even when
// the skill has become READY since then.
func TestSecurityActivateDoesNotDiscloseNamespaceOnRepeat(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)

	victimDir := t.TempDir()
	stageSkillWithSecret(t, victimDir, "slack")

	const privateBody = "answer from the private namespace"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(privateBody))
	}))
	t.Cleanup(upstream.Close)

	manifest, err := s.activate(victimDir)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	token, _ := manifest["dir_token"].(string)
	if token == "" {
		t.Fatal("the first activation returned no dir token: the fixture is broken, not the security property")
	}
	if s.tcpPort == 0 {
		t.Fatal("no facade TCP port: the fixture is broken, not the security property")
	}
	promoteToReady(t, s, victimDir, "slack", token, portOf(t, upstream.URL))

	// Model the owner's first manifest now that its sidecar is READY: that
	// response is the one legitimate place the namespace is disclosed.
	s.mu.RLock()
	d := s.dirs[victimDir]
	s.mu.RUnlock()
	if d == nil {
		t.Fatal("directory was not active: the fixture is broken, not the security property")
	}
	d.mu.Lock()
	d.tokenEmitted = false
	d.mu.Unlock()
	first, err := s.activate(victimDir)
	if err != nil {
		t.Fatalf("owner activation: %v", err)
	}
	if got, _ := first["dir_token"].(string); got != token {
		t.Fatalf("owner activation returned token %q, want %q: the fixture is broken", got, token)
	}
	var ownerBase string
	for _, sk := range first["skills"].([]map[string]any) {
		if sk["scope"] != "workdir" || sk["mount"] != "slack" {
			continue
		}
		ownerBase, _ = sk["base"].(string)
		if ownerBase == "" {
			t.Fatal("owner activation returned no base URL for its READY skill: the fixture is broken")
		}
		if socketBase, _ := sk["socket_base"].(string); socketBase == "" {
			t.Fatal("owner activation returned no socket base for its READY skill: the fixture is broken")
		}
	}
	if ownerBase == "" {
		t.Fatal("owner activation returned no READY workdir skill: the fixture is broken")
	}
	resp, err := http.Get(ownerBase + "/whoami")
	if err != nil {
		t.Fatalf("GET owner base: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != privateBody {
		t.Fatalf("owner base did not route before repeat activation (status %d body %q): the fixture is broken", resp.StatusCode, body)
	}

	// A global skill's namespace is the shared global one, not the withheld
	// directory capability. Seeding one here prevents an overbroad fix that
	// strips every skill from passing the workdir assertions below.
	s.mu.Lock()
	s.global["shared"] = &skillRoute{
		Name:      "shared",
		Mount:     "shared",
		Namespace: facade.GlobalNamespace,
		State:     facade.RouteReady,
	}
	s.mu.Unlock()

	// A second caller names the same directory. It must receive no token and
	// no URL from which the live namespace can be recovered.
	repeat, err := s.activate(victimDir)
	if err != nil {
		t.Fatalf("repeat activation: %v", err)
	}
	if got, ok := repeat["dir_token"]; ok {
		t.Errorf("repeat activation returned dir_token %v: a caller that knows only the path gains the live namespace", got)
	}
	sawGlobal := false
	for _, sk := range repeat["skills"].([]map[string]any) {
		if sk["scope"] == "global" && sk["mount"] == "shared" {
			sawGlobal = true
			if base, _ := sk["base"].(string); base == "" {
				t.Errorf("repeat activation removed a global skill's base URL: the shared global namespace is not the withheld directory capability")
			}
			if socketBase, _ := sk["socket_base"].(string); socketBase == "" {
				t.Errorf("repeat activation removed a global skill's socket base: the shared global namespace is not the withheld directory capability")
			}
		}
		if sk["scope"] != "workdir" {
			continue
		}
		base, ok := sk["base"]
		if !ok {
			if _, ok := sk["socket_base"]; ok {
				t.Errorf("repeat activation returned a skill socket base: the path carries the live namespace")
			}
			continue
		}
		t.Errorf("repeat activation returned a skill base URL %v: the URL carries the live namespace", base)
		if raw, _ := base.(string); raw != "" {
			leaked, err := http.Get(raw + "/whoami")
			if err == nil {
				leaked.Body.Close()
				if leaked.StatusCode == http.StatusOK {
					t.Errorf("the namespace in the leaked base URL was usable against the facade")
				}
			}
		}
	}
	if !sawGlobal {
		t.Fatal("repeat activation did not include the READY global skill: the fixture is broken, not the security property")
	}
}

// postJSONGet performs a GET and decodes the JSON response.
func postJSONGet(t *testing.T, url string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
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
	return out
}
