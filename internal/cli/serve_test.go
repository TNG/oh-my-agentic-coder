package cli

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandbox"
	"github.com/tngtech/oh-my-agentic-coder/internal/skilltrust"
	"github.com/tngtech/oh-my-agentic-coder/internal/supervisor"
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
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
	// Record a host approval so the spawn-approval gate lets activation reach
	// its pending-credentials/route logic (these tests exercise the activation
	// engine, not the gate; the caller has already isolated HOME/XDG).
	hash, err := config.BundleHash(skillDir)
	if err != nil {
		t.Fatalf("bundle hash: %v", err)
	}
	if err := skilltrust.Approve(name, hash, skillDir); err != nil {
		t.Fatalf("approve: %v", err)
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
		harness:    config.DefaultHarness(),
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

func TestInjectServerListenPort(t *testing.T) {
	oc, _ := config.LookupHarness("opencode")
	cc, _ := config.LookupHarness("claude-code")

	// A server harness gets its listen port allowlisted, spliced before `--`.
	in := []string{"omac", "sandbox", "run", "--profile", "tng-default", "--open-port", "5000", "--", "opencode", "serve"}
	got := injectServerListenPort(in, oc)
	want := []string{"omac", "sandbox", "run", "--profile", "tng-default", "--open-port", "5000", "--listen-port", "4096", "--", "opencode", "serve"}
	if !equalStrings(got, want) {
		t.Errorf("opencode: got %v, want %v", got, want)
	}

	// A harness with no server mode is a no-op (nothing to allowlist).
	in2 := []string{"external-sbx", "run", "--", "claude"}
	if got2 := injectServerListenPort(in2, cc); !equalStrings(got2, in2) {
		t.Errorf("claude-code should be a no-op: got %v, want %v", got2, in2)
	}
}

// TestSandboxServeArgvInjectsListenPort exercises the serve argv assembly
// end-to-end (the pipeline runServe actually calls), not just the
// injectServerListenPort helper in isolation. It guards against the #115 bind
// grant being dropped from the pipeline during a refactor.
func TestSandboxServeArgvInjectsListenPort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oc, _ := config.LookupHarness("opencode")
	prof := config.SandboxProfile{
		Command:  []string{"omac", "sandbox", "run", "--", "{{inner_cmd}}", "{{inner_args}}"},
		InnerCmd: []string{"opencode"},
	}
	in := sandbox.Inputs{Workdir: "/w", InnerCmd: []string{"opencode", "serve"}}

	argv, err := sandboxServeArgv(prof, in, "51234", oc)
	if err != nil {
		t.Fatalf("sandboxServeArgv: %v", err)
	}
	joined := strings.Join(argv, " ")

	// The opencode server binds 4096; the pipeline must allowlist it for bind.
	if !contains(joined, "--listen-port 4096") {
		t.Errorf("serve argv missing --listen-port 4096: %s", joined)
	}
	// The control-plane port is opened for connect too.
	if !contains(joined, "--open-port 51234") {
		t.Errorf("serve argv missing --open-port 51234: %s", joined)
	}
	// Grants splice before the `--` separator, not after it.
	if lp, sep := strings.Index(joined, "--listen-port"), strings.Index(joined, " -- "); lp < 0 || sep < 0 || lp > sep {
		t.Errorf("--listen-port must appear before `--`: %s", joined)
	}

	// Empty control port skips only the control-plane grant; the #115 bind
	// grant still applies.
	noCP, err := sandboxServeArgv(prof, in, "", oc)
	if err != nil {
		t.Fatalf("sandboxServeArgv (no control port): %v", err)
	}
	nj := strings.Join(noCP, " ")
	if contains(nj, "--open-port") {
		t.Errorf("empty control port should add no --open-port: %s", nj)
	}
	if !contains(nj, "--listen-port 4096") {
		t.Errorf("listen-port grant must still apply without a control port: %s", nj)
	}
}

func TestPrepareAndGrantOpenCodeRuntimeDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	oc, _ := config.LookupHarness("opencode")
	prof := config.SandboxProfile{
		Command:  []string{"omac", "sandbox", "run", "--", "{{inner_cmd}}", "{{inner_args}}"},
		InnerCmd: []string{"opencode"},
	}
	in := sandbox.Inputs{Workdir: t.TempDir(), InnerCmd: []string{"opencode", "serve"}}

	if err := prepareSandboxDirs(oc.SandboxCreateDirs); err != nil {
		t.Fatalf("prepareSandboxDirs: %v", err)
	}
	argv, err := sandboxServeArgv(prof, in, "", oc)
	if err != nil {
		t.Fatalf("sandboxServeArgv: %v", err)
	}
	info, err := os.Stat(filepath.Join(home, ".local", "share", "opentui"))
	if err != nil {
		t.Fatalf("opentui runtime dir was not created before sandbox launch: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("opentui runtime dir mode = %o; want 700", got)
	}
	if !contains(strings.Join(argv, " "), "--allow ~/.local/share/opentui") {
		t.Fatalf("serve argv missing opentui read/write grant: %v", argv)
	}
}

func TestPrepareAndGrantOpenCodeRuntimeDirsUsesXDGDataHome(t *testing.T) {
	xdgDataHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", xdgDataHome)
	oc, _ := config.LookupHarness("opencode")
	prof := config.SandboxProfile{
		Command:  []string{"omac", "sandbox", "run", "--", "{{inner_cmd}}", "{{inner_args}}"},
		InnerCmd: []string{"opencode"},
	}
	in := sandbox.Inputs{Workdir: t.TempDir(), InnerCmd: []string{"opencode", "serve"}}

	if err := prepareSandboxDirs(oc.SandboxCreateDirs); err != nil {
		t.Fatalf("prepareSandboxDirs: %v", err)
	}
	argv, err := sandboxServeArgv(prof, in, "", oc)
	if err != nil {
		t.Fatalf("sandboxServeArgv: %v", err)
	}
	want := filepath.Join(xdgDataHome, "opentui")
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("XDG opentui runtime dir was not created: info=%v err=%v", info, err)
	}
	if !contains(strings.Join(argv, " "), "--allow "+want) {
		t.Fatalf("serve argv missing XDG opentui read/write grant: %v", argv)
	}
}

func TestPrepareSandboxDirsRejectsFileTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(target, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := prepareSandboxDirs([]string{target})
	if err == nil || !strings.Contains(err.Error(), "create "+target) {
		t.Fatalf("prepareSandboxDirs error = %v; want create error", err)
	}
}

func TestServerExposureWarning(t *testing.T) {
	oc, _ := config.LookupHarness("opencode")
	cc, _ := config.LookupHarness("claude-code")

	// opencode server + auth env UNSET -> warn (names the port + the env var).
	unset := func(string) string { return "" }
	w := serverExposureWarning(oc, unset)
	if w == "" || !strings.Contains(w, "4096") || !strings.Contains(w, "OPENCODE_SERVER_PASSWORD") {
		t.Errorf("expected warning naming :4096 and OPENCODE_SERVER_PASSWORD, got %q", w)
	}

	// Auth env SET -> no warning (the exposed port is protected).
	set := func(k string) string {
		if k == "OPENCODE_SERVER_PASSWORD" {
			return "secret"
		}
		return ""
	}
	if w := serverExposureWarning(oc, set); w != "" {
		t.Errorf("auth set should suppress the warning, got %q", w)
	}

	// Harness with no server mode -> nothing to warn about.
	if w := serverExposureWarning(cc, unset); w != "" {
		t.Errorf("non-server harness should not warn, got %q", w)
	}
}

func TestInjectOpenPort(t *testing.T) {
	// With a `--` separator, the flag goes right before it.
	in := []string{"external-sbx", "run", "--open-port", "5000", "--", "opencode", "serve"}
	got := injectOpenPort(in, "6000")
	want := []string{"external-sbx", "run", "--open-port", "5000", "--open-port", "6000", "--", "opencode", "serve"}
	if !equalStrings(got, want) {
		t.Errorf("with --: got %v, want %v", got, want)
	}

	// Without a `--`, it goes right after argv[0].
	in2 := []string{"external-sbx", "run", "--allow-cwd"}
	got2 := injectOpenPort(in2, "6000")
	want2 := []string{"external-sbx", "--open-port", "6000", "run", "--allow-cwd"}
	if !equalStrings(got2, want2) {
		t.Errorf("without --: got %v, want %v", got2, want2)
	}

	// Empty argv is a no-op.
	if got3 := injectOpenPort(nil, "6000"); len(got3) != 0 {
		t.Errorf("empty argv: got %v, want []", got3)
	}
}

func TestInjectSandboxEnvAllow(t *testing.T) {
	builtinProf := config.DefaultLauncherConfig().Sandbox.Profiles["builtin"]
	argv, err := sandbox.Expand(builtinProf, sandbox.Inputs{
		Workdir:  "/w",
		Socket:   "/w/bridge.sock",
		TCPPort:  6000,
		InnerCmd: []string{"claude"},
		TmpDir:   "/w/tmp",
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	got := injectSandboxEnvAllow(argv, []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"})
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--allow-env ANTHROPIC_API_KEY") ||
		!strings.Contains(joined, "--allow-env ANTHROPIC_BASE_URL") {
		t.Errorf("must gain --allow-env flags: %v", got)
	}

	// Empty names is a no-op; empty entries are skipped.
	if g := injectSandboxEnvAllow(argv, nil); !equalStrings(g, argv) {
		t.Errorf("nil names should be a no-op: %v", g)
	}
	if g := injectSandboxEnvAllow(argv, []string{""}); !equalStrings(g, argv) {
		t.Errorf("empty entry should be skipped: %v", g)
	}
}

func TestInjectUserOpenPorts(t *testing.T) {
	profiles := config.DefaultLauncherConfig().Sandbox.Profiles
	builtinProf := profiles["builtin"]
	argv, err := sandbox.Expand(builtinProf, sandbox.Inputs{
		Workdir:  "/w",
		Socket:   "/w/bridge.sock",
		TCPPort:  6000,
		InnerCmd: []string{"claude"},
		TmpDir:   "/w/tmp",
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	got := injectUserOpenPorts(argv, []int{3000, 4173})
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--open-port 3000") || !strings.Contains(joined, "--open-port 4173") {
		t.Errorf("missing open-port flags: %s", joined)
	}

	if g := injectUserOpenPorts(argv, nil); !equalStrings(g, argv) {
		t.Errorf("empty inject should be no-op: %v", g)
	}
}

func TestIntMultiFlag(t *testing.T) {
	var m intMultiFlag
	if err := m.Set("3000"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("0"); err == nil {
		t.Error("port 0 should be rejected")
	}
	if err := m.Set("abc"); err == nil {
		t.Error("non-integer should be rejected")
	}
	if len(m) != 1 || m[0] != 3000 {
		t.Errorf("got %v", m)
	}
}

// TestForwardHarnessEnvEmptyProfileForwardsOperationalMinimum: with an
// empty-allow_vars profile, omac fails closed — it seeds ONLY the operational
// minimum (so HOME/PATH keep the harness runnable), does NOT auto-forward the
// harness's provider-auth vars, strips everything else, and warns.
func TestForwardHarnessEnvEmptyProfileForwardsOperationalMinimum(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stageProfile(t, home, `{"meta": {"name": "default"}}`)

	old := emptyAllowVarsWarnDelay
	emptyAllowVarsWarnDelay = 0
	defer func() { emptyAllowVarsWarnDelay = old }()

	env, _, errBuf, drain := newPipeEnv(t, "")
	plan := nativePlanForTest(t)
	harness := config.Harness{Name: "test", SandboxEnvAllow: []string{"ANTHROPIC_API_KEY"}}
	argv := []string{"/usr/bin/omac", "sandbox", "run", "--profile", "default", "--", "x"}

	got := forwardHarnessEnv(env, argv, harness, plan)
	drain()

	joined := strings.Join(got, " ")
	// Operational minimum is forwarded so the harness runs.
	for _, want := range []string{"--allow-env HOME", "--allow-env PATH"} {
		if !strings.Contains(joined, want) {
			t.Errorf("empty profile should forward %q; got %v", want, got)
		}
	}
	// Provider-auth vars are NOT auto-forwarded on an empty profile.
	if strings.Contains(joined, "ANTHROPIC_API_KEY") {
		t.Errorf("empty profile must not auto-forward provider-auth vars; got %v", got)
	}
	warning := errBuf.String()
	for _, want := range []string{
		"allow_vars",
		plan.PolicyPath,
		"not updated by omac upgrades",
		"installer or original source",
		`["*"] is not recommended`,
	} {
		if !strings.Contains(warning, want) {
			t.Errorf("empty-allow-vars warning missing %q; got:\n%s", want, warning)
		}
	}
}

// TestForwardHarnessEnvNonEmptyProfileInjects: a profile with an explicit
// allowlist still gets the harness auth vars injected as --allow-env.
func TestForwardHarnessEnvNonEmptyProfileInjects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stageProfile(t, home, `{"meta": {"name": "default"}, "environment": {"allow_vars": ["HOME"]}}`)

	env, _, errBuf, drain := newPipeEnv(t, "")
	plan := nativePlanForTest(t)
	harness := config.Harness{Name: "test", SandboxEnvAllow: []string{"ANTHROPIC_API_KEY"}}
	argv := []string{"/usr/bin/omac", "sandbox", "run", "--profile", "default", "--", "x"}

	got := forwardHarnessEnv(env, argv, harness, plan)
	drain()

	if !strings.Contains(strings.Join(got, " "), "--allow-env ANTHROPIC_API_KEY") {
		t.Errorf("non-empty profile: expected --allow-env injection; got %v", got)
	}
	if strings.Contains(errBuf.String(), "allow_vars") {
		t.Errorf("non-empty profile must not warn; got:\n%s", errBuf.String())
	}
}

// nativePlanForTest resolves the launch plan for a minimal native launcher
// profile, so a test's staged policy file (stageProfile) is what the plan's
// policy-derived behaviour is read from.
func nativePlanForTest(t *testing.T) sandboxPlan {
	t.Helper()
	lc := config.LauncherConfig{Sandbox: config.SandboxConfig{
		DefaultProfile: "builtin",
		Profiles: map[string]config.SandboxProfile{"builtin": {
			Command: []string{"{{self}}", "sandbox", "run", "--profile", "default", "--", "x"},
		}},
	}}
	plan, err := resolveSandboxPlan(lc, "")
	if err != nil {
		t.Fatalf("resolveSandboxPlan: %v", err)
	}
	return plan
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReloadGlobalsEmptyIsNoop(t *testing.T) {
	s := newServeServerForTest(t)
	// No global skills registered (isolated HOME/XDG), so reloadGlobals
	// just tears down nothing and re-activates nothing.
	if err := s.reloadGlobals(); err != nil {
		t.Fatalf("reloadGlobals: %v", err)
	}
	if len(s.global) != 0 {
		t.Errorf("global count = %d, want 0", len(s.global))
	}
}

func TestReloadGlobalEndpointExists(t *testing.T) {
	s := newServeServerForTest(t)
	mux := s.controlMux()
	req := httptest.NewRequest("POST", "/__omac__/reload-global", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	// With no global skills it should still succeed (200) and return a list.
	if rec.Code != 200 {
		t.Fatalf("reload-global status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "skills") {
		t.Errorf("reload-global body missing skills: %s", rec.Body.String())
	}
}

func TestDirsEndpointDoesNotLeakTokens(t *testing.T) {
	s := newServeServerForTest(t)
	wdA := t.TempDir()
	wdB := t.TempDir()
	stageSkillWithSecret(t, wdA, "slack")
	stageSkillWithSecret(t, wdB, "slack")
	if _, err := s.activate(wdA); err != nil {
		t.Fatalf("activate A: %v", err)
	}
	if _, err := s.activate(wdB); err != nil {
		t.Fatalf("activate B: %v", err)
	}

	req := httptest.NewRequest("GET", "/__omac__/dirs", nil)
	rec := httptest.NewRecorder()
	s.controlMux().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("dirs status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "dir_token") {
		t.Errorf("/__omac__/dirs leaked dir_token: %s", rec.Body.String())
	}
}

func TestRootsEmptyAllowsAny(t *testing.T) {
	s := newServeServerForTest(t)
	// No roots configured -> any directory allowed.
	if !s.dirAllowed(t.TempDir()) {
		t.Error("empty roots should allow any directory")
	}
}

func TestResolveDesktopStateDir(t *testing.T) {
	t.Run("not desktop mode: no injection", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "/whatever")
		dir, inject := resolveDesktopStateDir(false)
		if dir != "" || inject {
			t.Errorf("resolveDesktopStateDir(false) = (%q, %v), want (\"\", false)", dir, inject)
		}
	})

	t.Run("respects an explicit XDG_STATE_HOME (nothing to set)", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "/custom/state")
		dir, setEnv := resolveDesktopStateDir(true)
		if dir != "" || setEnv {
			t.Errorf("got (%q, %v), want (\"\", false)", dir, setEnv)
		}
	})

	t.Run("defaults to the Desktop dir when unset", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("XDG_DATA_HOME", "")
		t.Setenv("XDG_STATE_HOME", "") // empty == unset
		want := filepath.Join(home, "Library", "Application Support", "ai.opencode.desktop")
		if err := os.MkdirAll(want, 0o755); err != nil {
			t.Fatal(err)
		}
		dir, setEnv := resolveDesktopStateDir(true)
		if dir != want || !setEnv {
			t.Errorf("got (%q, %v), want (%q, true)", dir, setEnv, want)
		}
	})
}

func TestBaseEnvStaticVars(t *testing.T) {
	s := newServeServerForTest(t)
	s.controlBase = "http://127.0.0.1:9999"
	s.cacheEnv = map[string]string{
		"XDG_CACHE_HOME":   "/cache/xdg",
		"GOCACHE":          "/cache/go-build",
		"GOMODCACHE":       "/cache/go-mod",
		"NPM_CONFIG_CACHE": "/cache/npm",
		"PIP_CACHE_DIR":    "/cache/pip",
		"CARGO_HOME":       "/cache/cargo",
		"OMAC_CACHE_DIR":   "/cache",
		"OMAC_CACHE_MODE":  "persistent",
	}
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
	for k, want := range s.cacheEnv {
		if got := env[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestPrepareServeCache(t *testing.T) {
	isolateHome(t)
	launchWorkdir := t.TempDir()
	explicitWorkdir := t.TempDir()
	sandboxTmp := t.TempDir()
	canonicalLaunch, err := filepath.EvalSymlinks(launchWorkdir)
	if err != nil {
		t.Fatalf("canonical launch workdir: %v", err)
	}
	canonicalExplicit, err := filepath.EvalSymlinks(explicitWorkdir)
	if err != nil {
		t.Fatalf("canonical explicit workdir: %v", err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("cache:\n  scope: config\n"), 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	canonicalCfg, err := filepath.EvalSymlinks(cfgPath)
	if err != nil {
		t.Fatalf("canonical cfg: %v", err)
	}

	for _, c := range []struct {
		name                          string
		noSandbox, noInner, ephemeral bool
		scope                         config.CacheScope
		explicitWorkdir               string
		cfgPath                       string
		wantNil                       bool
		wantDomain                    toolcache.Domain
		wantMode                      toolcache.Mode
		wantPath                      string
		wantCanonical                 string
	}{
		{name: "no inner skips cache", noInner: true, wantNil: true},
		{name: "no sandbox skips cache", noSandbox: true, wantNil: true},
		{name: "global default shares one cache", scope: config.CacheScopeGlobal, wantDomain: toolcache.DomainShared, wantMode: toolcache.ModePersistent},
		{name: "config scope keys on config path", scope: config.CacheScopeConfig, cfgPath: cfgPath, wantDomain: toolcache.DomainConfig, wantMode: toolcache.ModePersistent, wantCanonical: canonicalCfg},
		{name: "config scope without file falls back to shared", scope: config.CacheScopeConfig, wantDomain: toolcache.DomainShared, wantMode: toolcache.ModePersistent},
		{name: "workdir scope, single workdir", scope: config.CacheScopeWorkdir, explicitWorkdir: explicitWorkdir, wantDomain: toolcache.DomainWorkdir, wantMode: toolcache.ModePersistent, wantCanonical: canonicalExplicit},
		{name: "workdir scope, multi-dir serve cache", scope: config.CacheScopeWorkdir, wantDomain: toolcache.DomainServe, wantMode: toolcache.ModePersistent, wantCanonical: canonicalLaunch},
		{name: "ephemeral cache", ephemeral: true, explicitWorkdir: explicitWorkdir, wantMode: toolcache.ModeEphemeral, wantPath: filepath.Join(sandboxTmp, "cache")},
	} {
		t.Run(c.name, func(t *testing.T) {
			scope, err := prepareServeCache(c.noSandbox, c.noInner, c.ephemeral, c.scope, c.explicitWorkdir, launchWorkdir, c.cfgPath, sandboxTmp)
			if err != nil {
				t.Fatalf("prepareServeCache: %v", err)
			}
			if c.wantNil {
				if scope != nil {
					t.Errorf("scope = %#v, want nil", scope)
				}
				return
			}
			if scope == nil {
				t.Fatal("scope = nil")
			}
			t.Cleanup(func() {
				if err := scope.Close(); err != nil {
					t.Errorf("close scope: %v", err)
				}
			})
			if scope.Domain != c.wantDomain {
				t.Errorf("domain = %q, want %q", scope.Domain, c.wantDomain)
			}
			if scope.Mode != c.wantMode {
				t.Errorf("mode = %q, want %q", scope.Mode, c.wantMode)
			}
			if scope.Dir != c.wantPath && c.wantMode == toolcache.ModeEphemeral {
				t.Errorf("dir = %q, want %q", scope.Dir, c.wantPath)
			}
			if scope.CanonicalPath != c.wantCanonical {
				t.Errorf("canonical path = %q, want %q", scope.CanonicalPath, c.wantCanonical)
			}
		})
	}
}

func TestRunServeRejectsEphemeralCacheWithoutSandbox(t *testing.T) {
	stderr, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	t.Cleanup(func() { stderr.Close() })
	env := devnullEnv(t)
	env.Workdir = t.TempDir()
	env.Stderr = writer
	if code := runServe([]string{"--ephemeral-cache", "--no-sandbox"}, env); code != ExitMisuse {
		t.Errorf("exit = %d, want ExitMisuse (%d)", code, ExitMisuse)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	output, err := io.ReadAll(stderr)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if !strings.Contains(string(output), "--ephemeral-cache cannot be used with --no-sandbox") {
		t.Errorf("stderr = %q, want invalid combination error", output)
	}
}

func TestRunServeInnerFlagsNeedDashDash(t *testing.T) {
	t.Run("dash-dash form forwards harness flags", func(t *testing.T) {
		opts, ok := parseServeArgs([]string{"opencode", "--verbose", "--", "--port", "4096", "--print-logs"}, devnullEnv(t))
		if !ok {
			t.Fatal("parseServeArgs() returned false")
		}
		if !opts.verbose {
			t.Error("verbose = false, want true")
		}
		if opts.harness.Name != "opencode" {
			t.Errorf("harness = %q, want opencode", opts.harness.Name)
		}
		want := []string{"--port", "4096", "--print-logs"}
		if !reflect.DeepEqual(opts.innerArgs, want) {
			t.Errorf("innerArgs = %v, want %v", opts.innerArgs, want)
		}
	})

	t.Run("mixed form fails and points at dash-dash", func(t *testing.T) {
		stderr, writer, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe stderr: %v", err)
		}
		t.Cleanup(func() { stderr.Close() })
		env := devnullEnv(t)
		env.Workdir = t.TempDir()
		env.Stderr = writer
		if code := runServe([]string{"opencode", "--verbose", "run", "--model", "x"}, env); code != ExitMisuse {
			t.Errorf("exit = %d, want ExitMisuse (%d)", code, ExitMisuse)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close stderr writer: %v", err)
		}
		output, err := io.ReadAll(stderr)
		if err != nil {
			t.Fatalf("read stderr: %v", err)
		}
		if !strings.Contains(string(output), "pass harness flags after --") {
			t.Errorf("stderr = %q, want inner-args -- hint", output)
		}
		got := string(output)
		errAt := strings.Index(got, "flag provided but not defined")
		hintAt := strings.Index(got, "pass harness flags after --")
		usageAt := strings.Index(got, "Usage:")
		if errAt < 0 || hintAt < 0 || usageAt < 0 || !(errAt < hintAt && hintAt < usageAt) {
			t.Errorf("want error, then -- hint, then Usage; got %q", got)
		}
		if !strings.Contains(got, "Args after -- go to the harness") {
			t.Errorf("stderr = %q, want Usage to explain --", got)
		}
	})
}

func TestRunServeRetainsCacheLockAndAllowsOnlyScope(t *testing.T) {
	isolateHome(t)
	shortTmp, err := os.MkdirTemp("/tmp", "omac-serve-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shortTmp) })
	t.Setenv("TMPDIR", shortTmp)

	workdir := t.TempDir()

	// Capture the assembled serve argv/env via the exec seam and block the
	// "child" (as the real sandbox would) until the test releases it, so we
	// can assert the cache scope is held active during the session and
	// cleared afterwards — without launching a real subprocess.
	orig := execWithReady
	t.Cleanup(func() { execWithReady = orig })
	ready := make(chan struct{})
	release := make(chan struct{})
	var gotArgv []string
	gotEnv := map[string]string{}
	execWithReady = func(argv []string, extraEnv map[string]string, onReady func()) (int, error) {
		gotArgv = append([]string(nil), argv...)
		for _, kv := range os.Environ() {
			if i := strings.IndexByte(kv, '='); i >= 0 {
				gotEnv[kv[:i]] = kv[i+1:]
			}
		}
		for k, v := range extraEnv {
			gotEnv[k] = v
		}
		if onReady != nil {
			onReady()
		}
		close(ready)
		<-release
		return ExitOK, nil
	}
	releaseOnce := func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	t.Cleanup(releaseOnce)

	env, stderr := launchTestEnv(t, workdir)
	done := make(chan int, 1)
	go func() {
		done <- runServe([]string{"claude", "--inner", "/bin/true"}, env)
	}()

	select {
	case <-ready:
	case code := <-done:
		t.Fatalf("runServe exited early with %d:\n%s", code, stderr())
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for exec:\n%s", stderr())
	}

	scope, err := toolcache.DescribeShared()
	if err != nil {
		t.Fatalf("describe serve cache: %v", err)
	}
	var cacheAllows []string
	scopeAllows := 0
	for i, arg := range gotArgv {
		if arg == "--allow" && i+1 < len(gotArgv) {
			allowed := gotArgv[i+1]
			cacheAllows = append(cacheAllows, allowed)
			if allowed == scope.Dir {
				scopeAllows++
			}
		}
	}
	if scopeAllows != 1 {
		t.Errorf("cache scope %q appears in --allow %d times, want 1: %v", scope.Dir, scopeAllows, cacheAllows)
	}
	for _, allowed := range cacheAllows {
		if allowed == filepath.Dir(scope.Dir) {
			t.Errorf("sandbox grants broad cache root %q: %v", allowed, cacheAllows)
		}
	}

	for _, key := range []string{"OMAC_CACHE_DIR", "OMAC_CACHE_MODE"} {
		want := toolcache.Environment(scope.Dir, toolcache.ModePersistent)[key]
		if got := gotEnv[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	active, err := toolcache.ClearAll()
	if err != nil {
		t.Fatalf("clear active cache: %v", err)
	}
	if len(active) != 1 || active[0].Path != scope.Dir || active[0].Status != toolcache.ClearActive {
		t.Errorf("active clear results = %#v, want active %q", active, scope.Dir)
	}

	releaseOnce()
	select {
	case code := <-done:
		if code != ExitOK {
			t.Errorf("runServe exit = %d, want ExitOK\nstderr:\n%s", code, stderr())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for runServe to exit:\n%s", stderr())
	}

	inactive, err := toolcache.ClearAll()
	if err != nil {
		t.Fatalf("clear inactive cache: %v", err)
	}
	if len(inactive) != 1 || inactive[0].Path != scope.Dir || inactive[0].Status != toolcache.ClearRemoved {
		t.Errorf("inactive clear results = %#v, want removed %q", inactive, scope.Dir)
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

func TestRediscoverPicksUpNewSkill(t *testing.T) {
	s := newServeServerForTest(t)
	wd := t.TempDir()
	stageSkillWithSecret(t, wd, "slack")

	m1, err := s.activate(wd)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if len(m1["skills"].([]map[string]any)) != 1 {
		t.Fatalf("want 1 skill initially, got %d", len(m1["skills"].([]map[string]any)))
	}

	// Install a second skill AFTER the dir is already active.
	stageSkillWithSecret(t, wd, "email")

	// A repeat activate must re-discover and surface the new skill — no
	// manual reload.
	m2, err := s.activate(wd)
	if err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	skills := m2["skills"].([]map[string]any)
	if len(skills) != 2 {
		t.Fatalf("want 2 skills after rediscover, got %d", len(skills))
	}
	names := map[string]bool{}
	for _, sk := range skills {
		names[sk["name"].(string)] = true
	}
	if !names["slack"] || !names["email"] {
		t.Errorf("expected both slack and email, got %v", names)
	}
	// Token is stable across rediscover (same activation).
	if m1["dir_token"] != m2["dir_token"] {
		t.Errorf("token changed on rediscover: %v -> %v", m1["dir_token"], m2["dir_token"])
	}
}

func TestCheckGlobalDriftRefusesUnregistered(t *testing.T) {
	s := newServeServerForTest(t)
	// Stage a global skill on disk but never register it.
	stageUserGlobalSkill(t, "weather")

	code := s.checkGlobalDrift()
	if code == ExitOK {
		t.Fatal("expected serve to refuse on an unregistered global skill")
	}
	if code != ExitPrerequisiteMissing {
		t.Errorf("exit code = %d, want ExitPrerequisiteMissing (%d)", code, ExitPrerequisiteMissing)
	}
}

func TestCheckGlobalDriftCleanWhenNoGlobals(t *testing.T) {
	s := newServeServerForTest(t)
	// Isolated HOME/XDG => no global skills at all.
	if code := s.checkGlobalDrift(); code != ExitOK {
		t.Errorf("expected ExitOK with no global skills, got %d", code)
	}
}

// stageSkillWithPassthroughSecret writes a skill whose required secret is
// declared in BOTH secrets: and env_passthrough: — the shape internal/config's
// SidecarMeta docs bless as "the fallback for environments where the keychain
// is unavailable (sandboxed CI runners, headless servers)", and which
// supervisor.buildEnv honours at spawn time.
func stageSkillWithPassthroughSecret(t *testing.T, workdir, name string) {
	t.Helper()
	skillDir := filepath.Join(workdir, ".opencode", "skills", name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	// `true` exits immediately, so the health probe fails fast and the route
	// ends up broken-on-spawn. That is fine and is the point: this test asserts
	// resolution got PAST the credential gate, not that a sidecar came up. The
	// tiny timeouts keep that failure at ~100ms rather than the 5s default.
	meta := "name: " + name + "\n" +
		"sidecar:\n" +
		"  command: [\"true\"]\n" +
		"  env_passthrough:\n" +
		"    - API_TOKEN\n" +
		"  secrets:\n" +
		"    - name: API_TOKEN\n" +
		"      required: true\n" +
		"  health:\n" +
		"    path: /status\n" +
		"    initial_delay_ms: 10\n" +
		"    timeout_ms: 100\n" +
		"    interval_ms: 10\n"
	if err := os.WriteFile(filepath.Join(skillDir, "omac.yaml"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write omac.yaml: %v", err)
	}
	hash, err := config.BundleHash(skillDir)
	if err != nil {
		t.Fatalf("bundle hash: %v", err)
	}
	if err := skilltrust.Approve(name, hash, skillDir); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

// TestActivateEnvPassthroughSecretIsNotPending is issue #174's Failure 2:
// serve's own copy of the resolution rule never consulted env_passthrough, so
// on a headless runner a skill whose required secret came from the shell was
// reported pending-credentials with a "missing credentials" hint — even though
// the supervisor would have injected the value and the sidecar would have
// started fine. `omac start` honoured the fallback; serve did not.
func TestActivateEnvPassthroughSecretIsNotPending(t *testing.T) {
	s := newServeServerForTest(t)
	// Resolution now SUCCEEDS, so bringUp reaches the spawn path — which the
	// default test server leaves nil because it only ever exercised
	// pending-credentials skills.
	s.sup = supervisor.New(nil, audit.Nop(), skillSpawnAuthorizer)
	t.Cleanup(func() { s.sup.ShutdownAll(time.Second) })
	wd := t.TempDir()
	stageSkillWithPassthroughSecret(t, wd, "slack")
	t.Setenv("API_TOKEN", "shell-supplied")

	manifest, err := s.activate(wd)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	skills := manifest["skills"].([]map[string]any)
	if len(skills) != 1 {
		t.Fatalf("skills count = %d, want 1", len(skills))
	}
	sk := skills[0]
	if sk["state"] == string(facade.RoutePendingCredentials) {
		t.Errorf("state = pending-credentials, but the secret is supplied via env_passthrough: %v", sk)
	}
	if missing, _ := sk["missing"].([]string); len(missing) != 0 {
		t.Errorf("missing = %v, want none — the sidecar receives API_TOKEN from the host env", missing)
	}
}

// TestActivateEmptyPassthroughSecretIsStillPending is the other side of the
// fallback: env_passthrough forwards a variable even when it is empty, and an
// empty token is no token, so this must NOT be treated as satisfied.
func TestActivateEmptyPassthroughSecretIsStillPending(t *testing.T) {
	s := newServeServerForTest(t)
	wd := t.TempDir()
	stageSkillWithPassthroughSecret(t, wd, "slack")
	t.Setenv("API_TOKEN", "")

	manifest, err := s.activate(wd)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	sk := manifest["skills"].([]map[string]any)[0]
	if sk["state"] != string(facade.RoutePendingCredentials) {
		t.Errorf("state = %v, want pending-credentials for an empty exported value", sk["state"])
	}
}
