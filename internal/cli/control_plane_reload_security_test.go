package cli

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/facade"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
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
