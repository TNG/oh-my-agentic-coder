package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
)

// stageSkillWithSecret writes a workdir-local skill whose omac.yaml
// declares a required secret, so serve-mode activation classifies it as
// pending-credentials (no sidecar spawned, no network needed).
func stageSkillWithSecret(t *testing.T, workdir, name string) {
	t.Helper()
	skillDir := filepath.Join(workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	meta := "name: " + name + "\n" +
		"sidecar:\n" +
		"  command: [\"true\"]\n" +
		"  secrets:\n" +
		"    - name: API_TOKEN\n" +
		"      required: true\n"
	if err := os.WriteFile(filepath.Join(skillDir, "omac.yaml"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write omac.yaml: %v", err)
	}
}

// newServeServerForTest builds a serveServer with a real facade bound to a
// (possibly skipped) TCP port, plus empty state maps. It does not start the
// inner command or control HTTP server — tests drive the engine directly.
func newServeServerForTest(t *testing.T) *serveServer {
	t.Helper()
	isolateHome(t)
	rt := t.TempDir()
	f := facade.New("", "127.0.0.1:0", nil, 1<<20, 0, "", "test")
	// Start may fail if loopback listen is forbidden; tolerate by leaving
	// tcpPort 0 — the activation engine doesn't require a live listener
	// for pending-credentials skills.
	_ = f.Start(t.Context())
	t.Cleanup(func() { f.Close() })

	return &serveServer{
		env:        makeEnv(t.TempDir()),
		facade:     f,
		sup:        nil, // not used for pending-credentials path
		ctx:        t.Context(),
		rtDir:      rt,
		socketPath: filepath.Join(rt, "bridge.sock"),
		tcpPort:    f.TCPPort(),
		dirs:       map[string]*dirState{},
		byToken:    map[string]*dirState{},
		global:     map[string]*skillRoute{},
	}
}

func TestActivatePendingCredentials(t *testing.T) {
	s := newServeServerForTest(t)
	wd := t.TempDir()
	stageSkillWithSecret(t, wd, "slack")

	manifest, err := s.activate(wd)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if manifest["state"] != "active_partial" {
		t.Errorf("state = %v, want active_partial", manifest["state"])
	}
	token, _ := manifest["dir_token"].(string)
	if len(token) != 32 { // 16 random bytes hex-encoded
		t.Errorf("dir_token = %q (len %d), want 32 hex chars", token, len(token))
	}
	skills := manifest["skills"].([]map[string]any)
	if len(skills) != 1 {
		t.Fatalf("skills count = %d, want 1", len(skills))
	}
	sk := skills[0]
	if sk["state"] != string(facade.RoutePendingCredentials) {
		t.Errorf("skill state = %v, want pending-credentials", sk["state"])
	}
	if sk["scope"] != "workdir" {
		t.Errorf("scope = %v, want workdir", sk["scope"])
	}
	missing, _ := sk["missing"].([]string)
	if len(missing) != 1 || missing[0] != "API_TOKEN" {
		t.Errorf("missing = %v, want [API_TOKEN]", missing)
	}

	// The facade has a stub route under the dir token.
	if !s.facade.HasRoute(token, "slack") {
		t.Error("expected facade stub route under dir token")
	}
}

func TestActivateIdempotent(t *testing.T) {
	s := newServeServerForTest(t)
	wd := t.TempDir()
	stageSkillWithSecret(t, wd, "slack")

	m1, err := s.activate(wd)
	if err != nil {
		t.Fatalf("activate 1: %v", err)
	}
	m2, err := s.activate(wd)
	if err != nil {
		t.Fatalf("activate 2: %v", err)
	}
	if m1["dir_token"] != m2["dir_token"] {
		t.Errorf("token changed on re-activate: %v vs %v", m1["dir_token"], m2["dir_token"])
	}
	if len(s.dirs) != 1 {
		t.Errorf("dirs count = %d, want 1", len(s.dirs))
	}
}

func TestActivateUnknownDir(t *testing.T) {
	s := newServeServerForTest(t)
	if _, err := s.activate(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("expected error activating a non-existent dir")
	}
}

func TestDeactivateRemovesRoutesAndToken(t *testing.T) {
	s := newServeServerForTest(t)
	wd := t.TempDir()
	stageSkillWithSecret(t, wd, "slack")

	m, err := s.activate(wd)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	token := m["dir_token"].(string)
	if !s.facade.HasRoute(token, "slack") {
		t.Fatal("route should exist after activate")
	}

	s.deactivate(wd)
	if s.facade.HasRoute(token, "slack") {
		t.Error("route should be gone after deactivate")
	}
	if _, ok := s.dirs[wd]; ok {
		t.Error("dir should be removed after deactivate")
	}
	if _, ok := s.byToken[token]; ok {
		t.Error("token should be removed after deactivate")
	}
}

func TestRootsPolicy(t *testing.T) {
	s := newServeServerForTest(t)
	rootA := t.TempDir()
	rootB := t.TempDir()
	s.roots = []string{rootA, rootB}

	// A subdirectory of an allowed root is allowed.
	sub := filepath.Join(rootA, "project1")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if !s.dirAllowed(sub) {
		t.Error("subdir of root A should be allowed")
	}
	// The root itself is allowed.
	if !s.dirAllowed(rootB) {
		t.Error("root B itself should be allowed")
	}
	// A directory outside every root is rejected.
	outside := t.TempDir()
	if s.dirAllowed(outside) {
		t.Error("dir outside all roots should be rejected")
	}
	// A sibling that shares a path prefix string but not a real ancestor
	// must NOT be allowed (guard against naive HasPrefix).
	sneaky := rootA + "-evil"
	if err := os.MkdirAll(sneaky, 0o755); err != nil {
		t.Fatal(err)
	}
	if s.dirAllowed(sneaky) {
		t.Errorf("%q must not be considered under %q", sneaky, rootA)
	}

	// Activation of an outside dir is refused end-to-end.
	stageSkillWithSecret(t, outside, "slack")
	if _, err := s.activate(outside); err == nil {
		t.Error("activate outside root should fail")
	}
	// Activation inside a root succeeds.
	stageSkillWithSecret(t, sub, "slack")
	if _, err := s.activate(sub); err != nil {
		t.Errorf("activate inside root should succeed: %v", err)
	}
}

func TestEnsureServeSubcommand(t *testing.T) {
	cases := []struct {
		name     string
		inner    []string
		trailing []string
		want     []string
	}{
		{"bare opencode", []string{"opencode"}, nil, []string{"opencode", "serve"}},
		{"opencode with flags only", []string{"opencode"}, []string{"--port", "0"}, []string{"opencode", "serve"}},
		{"opencode already serve", []string{"opencode", "serve"}, nil, []string{"opencode", "serve"}},
		{"opencode explicit other subcommand", []string{"opencode"}, []string{"run", "x"}, []string{"opencode"}},
		{"absolute path opencode", []string{"/usr/bin/opencode"}, nil, []string{"/usr/bin/opencode", "serve"}},
		{"non-opencode untouched", []string{"bash"}, nil, []string{"bash"}},
		{"inner tail has subcommand", []string{"opencode", "tui"}, nil, []string{"opencode", "tui"}},
		{"inner tail flag only -> insert", []string{"opencode", "--pure"}, nil, []string{"opencode", "serve", "--pure"}},
	}
	for _, c := range cases {
		got := ensureServeSubcommand(append([]string(nil), c.inner...), c.trailing)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}

func TestRootsEmptyAllowsAny(t *testing.T) {
	s := newServeServerForTest(t)
	// No roots configured -> any directory allowed.
	if !s.dirAllowed(t.TempDir()) {
		t.Error("empty roots should allow any directory")
	}
}

func TestBaseEnvStaticVars(t *testing.T) {
	s := newServeServerForTest(t)
	s.controlBase = "http://127.0.0.1:9999"
	env := s.baseEnv()
	for _, k := range []string{"OMAC_SOCKET", "OMAC_HOST", "OMAC_PORT", "OMAC_BASE", "OMAC_VERSION", "OMAC_CONTROL_BASE", "OMAC_SKILLS"} {
		if _, ok := env[k]; !ok {
			t.Errorf("baseEnv missing %s", k)
		}
	}
	if env["OMAC_CONTROL_BASE"] != "http://127.0.0.1:9999" {
		t.Errorf("OMAC_CONTROL_BASE = %q", env["OMAC_CONTROL_BASE"])
	}
	// With no global skills, OMAC_SKILLS is empty.
	if env["OMAC_SKILLS"] != "" {
		t.Errorf("OMAC_SKILLS = %q, want empty", env["OMAC_SKILLS"])
	}
}

func TestTwoDirsDistinctTokensAndRoutes(t *testing.T) {
	s := newServeServerForTest(t)
	wdA := t.TempDir()
	wdB := t.TempDir()
	stageSkillWithSecret(t, wdA, "slack")
	stageSkillWithSecret(t, wdB, "slack")

	mA, err := s.activate(wdA)
	if err != nil {
		t.Fatalf("activate A: %v", err)
	}
	mB, err := s.activate(wdB)
	if err != nil {
		t.Fatalf("activate B: %v", err)
	}
	tokA := mA["dir_token"].(string)
	tokB := mB["dir_token"].(string)
	if tokA == tokB {
		t.Fatal("two dirs got the same token")
	}
	// Each dir's same-named skill is a distinct namespaced route.
	if !s.facade.HasRoute(tokA, "slack") || !s.facade.HasRoute(tokB, "slack") {
		t.Error("expected distinct namespaced routes for both dirs")
	}
	// A's token cannot reach B and vice versa is enforced by the token
	// being unguessable + the route key including the namespace; here we
	// just assert the routes are keyed separately.
	if tokA == "" || tokB == "" {
		t.Error("tokens must be non-empty")
	}
}
