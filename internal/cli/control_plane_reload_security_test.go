package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
	"github.com/TNG/oh-my-agentic-coder/internal/supervisor"
)

// The live-reload control plane.
//
// `omac start` runs a small HTTP server on loopback and deliberately opens its
// port into the sandbox, so the agent can register a skill mid-session and
// bring it up without restarting the harness. POST /__omac__/reload is
// therefore reachable by design from the one party omac is confining.
//
// Reload reads the workdir registry (.opencode/sidecar.json) and the skill
// directories beside it. All of that is agent-writable — that is the premise
// the spawn-approval gate was built on, and the gate does its job: unapproved
// code is refused, never executed. But refusing to spawn is not the same as
// leaving the routing table alone. The refusal still installs a stub route,
// and it installs it under the mount the forged entry asked for.
//
// Mount names are the namespace the facade routes on, and nothing reserves
// one for the skill already using it: reload skips a skill it has already
// mounted by NAME, while the facade keys routes by MOUNT. A forged entry
// under a fresh name that claims an occupied mount is therefore not a
// duplicate to skip, and its stub overwrites the live route.

// stageForgedSkill writes a workdir-local skill that claims mount and forges
// its registry entry, as a confined agent with workdir write access can. The
// skill is never approved, so reload refuses to spawn it.
func stageForgedSkill(t *testing.T, workdir, name, mount string) {
	t.Helper()
	skillDir := filepath.Join(workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	meta := "name: " + name + "\n" +
		"type: skill\n" +
		"sidecar:\n" +
		"  command: [\"python3\", \"server.py\"]\n" +
		"  mount: " + mount + "\n"
	if err := os.WriteFile(filepath.Join(skillDir, config.MetaFileName), []byte(meta), 0o644); err != nil {
		t.Fatalf("write omac.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "server.py"), []byte("# unapproved\n"), 0o644); err != nil {
		t.Fatalf("write server.py: %v", err)
	}
	if err := registry.WithLock(workdir, func() error {
		reg, err := registry.Load(workdir)
		if err != nil {
			return err
		}
		reg.Upsert(registry.Entry{
			Name:         name,
			SkillDir:     filepath.Join(".opencode", "skills", name),
			BundleHash:   bundleHashOf(t, skillDir),
			RegisteredAt: time.Now().UTC(),
		})
		return registry.Save(workdir, reg)
	}); err != nil {
		t.Fatalf("forge registry: %v", err)
	}
}

// portOf extracts the port from an httptest server URL.
func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %s: %v", rawURL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %s: %v", rawURL, err)
	}
	return p
}

// TestSecurityReloadCannotReplaceApprovedSkillRoute asserts that a skill the
// agent authored cannot take over the mount of a skill the host approved.
func TestSecurityReloadCannotReplaceApprovedSkillRoute(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	isolateHome(t)
	workdir := t.TempDir()

	const approvedBody = "answer from the host-approved sidecar"
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(approvedBody))
	}))
	t.Cleanup(victim.Close)

	r, baseURL := newLiveReloader(t, workdir)

	// A host-approved skill, already up and serving on mount "slack".
	// Owner is set so AddRoute can enforce that a different skill cannot
	// claim this mount later (the route ownership guard in facade.AddRoute).
	r.facade.AddRoute(facade.Route{
		Mount:        "slack",
		UpstreamPort: portOf(t, victim.URL),
		Skill:        "slack",
		Owner:        "slack",
		State:        facade.RouteReady,
	})
	r.markMounted("slack", "slack")

	// Control: the victim route works before the attack. Without it, a route
	// that never served would satisfy nothing and hide a broken fixture.
	if _, body := httpGet(t, baseURL+"/slack/ping"); body != approvedBody {
		t.Fatalf("the approved route did not serve before the attack (got %q): the fixture is broken, not the security property", body)
	}

	// The agent authors a skill under a name nothing has mounted, claiming a
	// mount that something has.
	stageForgedSkill(t, workdir, "pwn", "slack")
	r.reload()

	resp, body := httpGet(t, baseURL+"/slack/ping")
	if body != approvedBody {
		t.Errorf("the approved skill's mount now answers %q with status %d: a skill the agent wrote in the workdir knocked out a skill the host approved, for the rest of the session", body, resp.StatusCode)
	}
}

// TestSecurityReloadRejectsUnauthenticatedCaller asserts that the reload
// endpoint does not act on a request that proves nothing about its caller.
//
// The endpoint changes the session's routing table, and it is reachable from
// the sandbox and from every other process on the machine. Nothing in the
// request identifies the caller as this session's agent, so the handler
// cannot tell the difference between the harness plugin doing its job and
// anything else on the box.
func TestSecurityReloadRejectsUnauthenticatedCaller(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	// Control: the same staged skill does change state when reload is driven
	// in-process, on the host side of the boundary. Proves the fixture stages
	// something reload reacts to, so the assertion below is about the caller
	// and not about nothing ever happening.
	t.Run("in-process reload does mutate", func(t *testing.T) {
		sectest.RequireLoopbackListener(t)
		isolateHome(t)
		t.Setenv("TMPDIR", t.TempDir())
		workdir := t.TempDir()
		r, _ := newLiveReloader(t, workdir)
		stageForgedSkill(t, workdir, "pwn", "pwn")
		r.reload()
		if !r.facade.HasRoute("", "pwn") {
			t.Fatal("an in-process reload installed no route for the staged skill: the fixture is broken")
		}
	})

	isolateHome(t)
	t.Setenv("TMPDIR", t.TempDir()) // startControlPlane publishes the control-info file
	workdir := t.TempDir()
	r, _ := newLiveReloader(t, workdir)
	stageForgedSkill(t, workdir, "pwn", "pwn")

	// The production control plane, not a hand-copied mux: a test that served
	// its own routes would neither see an auth layer added to startControlPlane
	// nor notice one added only to the copy.
	controlURL, closeControl, ok := startControlPlane(r)
	if !ok {
		t.Fatal("startControlPlane could not bind: the fixture is broken, not the security property")
	}
	t.Cleanup(closeControl)

	resp, err := http.Post(controlURL+"/__omac__/reload", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /__omac__/reload: %v", err)
	}
	defer resp.Body.Close()

	if r.facade.HasRoute("", "pwn") {
		t.Errorf("an unauthenticated POST changed the session's routing table (status %d): any process on the machine can drive this session's control plane", resp.StatusCode)
	}
}

// TestSecurityReloadDoesNotRotateNamespace asserts that reloading an active
// directory keeps its token, route and sidecar in place. Reload may discover
// new or previously non-ready skills, but it must not tear down the namespace
// the owning session is already using, nor hand a fresh one to whoever asked.
func TestSecurityReloadDoesNotRotateNamespace(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	s := newServeServerForTest(t)
	s.sup = supervisor.New(nil, audit.Nop(), skillSpawnAuthorizer)

	workdir := t.TempDir()
	stageSkillWithSecret(t, workdir, "slack")

	const privateBody = "answer from the live sidecar"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(privateBody))
	}))
	t.Cleanup(upstream.Close)

	manifest, err := s.activate(workdir)
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
	upstreamPort := portOf(t, upstream.URL)
	promoteToReady(t, s, workdir, "slack", token, upstreamPort)

	baseURL := "http://127.0.0.1:" + strconv.Itoa(s.tcpPort) + "/" + token + "/slack"
	resp, body := httpGet(t, baseURL+"/whoami")
	if resp.StatusCode != http.StatusOK || body != privateBody {
		t.Fatalf("the owner's base did not route before reload (status %d body %q): the fixture is broken", resp.StatusCode, body)
	}

	req := httptest.NewRequest(http.MethodPost, "/__omac__/reload",
		strings.NewReader(`{"dir":"`+workdir+`"}`))
	addServeTestToken(req)
	rec := httptest.NewRecorder()
	s.controlMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /__omac__/reload: status %d body %s", rec.Code, rec.Body.String())
	}
	var reloaded map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&reloaded); err != nil {
		t.Fatalf("decode reload response: %v", err)
	}

	s.mu.RLock()
	d, active := s.dirs[workdir]
	var activeToken string
	if d != nil {
		activeToken = d.Token
	}
	s.mu.RUnlock()
	if !active || d == nil {
		t.Fatalf("reload removed the active directory: the fixture is broken, not the security property")
	}
	if activeToken != token {
		t.Errorf("reload rotated the directory token from %q to %q: the owning session's existing URLs stop working and the caller learns a fresh namespace", token, activeToken)
	}
	if got, ok := reloaded["dir_token"]; ok {
		t.Errorf("reload returned a fresh dir_token %v: a caller that knows only the path learns a live namespace", got)
	}
	if !s.facade.HasRoute(token, "slack") {
		t.Errorf("reload removed the owning session's existing route")
	}
	if got := s.facade.UpstreamPort(token, "slack"); got != upstreamPort {
		t.Errorf("reload replaced the live upstream port: got %d, want %d", got, upstreamPort)
	}
	resp, body = httpGet(t, baseURL+"/whoami")
	if resp.StatusCode != http.StatusOK || body != privateBody {
		t.Errorf("the owner's existing base stopped routing after reload (status %d body %q): reload cut off the session it was serving", resp.StatusCode, body)
	}
}
