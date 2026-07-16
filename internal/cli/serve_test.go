package cli

import (
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandbox"
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
	in2 := []string{"nono", "run", "--", "claude"}
	if got2 := injectServerListenPort(in2, cc); !equalStrings(got2, in2) {
		t.Errorf("claude-code should be a no-op: got %v, want %v", got2, in2)
	}
}

// TestSandboxServeArgvInjectsListenPort exercises the serve argv assembly
// end-to-end (the pipeline runServe actually calls), not just the
// injectServerListenPort helper in isolation. It guards against the #115 bind
// grant being dropped from the pipeline during a refactor.
func TestSandboxServeArgvInjectsListenPort(t *testing.T) {
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
	in := []string{"nono", "run", "--open-port", "5000", "--", "opencode", "serve"}
	got := injectOpenPort(in, "6000")
	want := []string{"nono", "run", "--open-port", "5000", "--open-port", "6000", "--", "opencode", "serve"}
	if !equalStrings(got, want) {
		t.Errorf("with --: got %v, want %v", got, want)
	}

	// Without a `--`, it goes right after argv[0].
	in2 := []string{"nono", "run", "--allow-cwd"}
	got2 := injectOpenPort(in2, "6000")
	want2 := []string{"nono", "--open-port", "6000", "run", "--allow-cwd"}
	if !equalStrings(got2, want2) {
		t.Errorf("without --: got %v, want %v", got2, want2)
	}

	// Empty argv is a no-op.
	if got3 := injectOpenPort(nil, "6000"); len(got3) != 0 {
		t.Errorf("empty argv: got %v, want []", got3)
	}
}

func TestInjectSandboxEnvAllow(t *testing.T) {
	in := []string{"omac", "sandbox", "run", "--profile", "default", "--", "claude"}
	got := injectSandboxEnvAllow(in, []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"})
	want := []string{
		"omac", "sandbox", "run", "--profile", "default",
		"--allow-env", "ANTHROPIC_API_KEY",
		"--allow-env", "ANTHROPIC_BASE_URL",
		"--", "claude",
	}
	if !equalStrings(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// Empty names is a no-op; empty entries are skipped.
	if got := injectSandboxEnvAllow(in, nil); !equalStrings(got, in) {
		t.Errorf("nil names should be a no-op: %v", got)
	}
	if got := injectSandboxEnvAllow(in, []string{""}); !equalStrings(got, in) {
		t.Errorf("empty entry should be skipped: %v", got)
	}

	// Non-native backend (nono) does not understand --allow-env: the argv
	// must be left untouched (env filtering is nono's own concern).
	nono := []string{"nono", "run", "--allow-cwd", "--profile", "tng-sandbox", "--", "opencode"}
	if got := injectSandboxEnvAllow(nono, []string{"ANTHROPIC_API_KEY"}); !equalStrings(got, nono) {
		t.Errorf("nono argv must be untouched: %v", got)
	}
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

	for _, c := range []struct {
		name                          string
		noSandbox, noInner, ephemeral bool
		explicitWorkdir               string
		wantNil                       bool
		wantDomain                    toolcache.Domain
		wantMode                      toolcache.Mode
		wantPath                      string
		wantCanonical                 string
	}{
		{name: "no inner skips cache", noInner: true, wantNil: true},
		{name: "no sandbox skips cache", noSandbox: true, wantNil: true},
		{name: "single workdir", explicitWorkdir: explicitWorkdir, wantDomain: toolcache.DomainWorkdir, wantMode: toolcache.ModePersistent, wantCanonical: canonicalExplicit},
		{name: "desktop shared serve cache", wantDomain: toolcache.DomainServe, wantMode: toolcache.ModePersistent, wantCanonical: canonicalLaunch},
		{name: "ephemeral cache", ephemeral: true, explicitWorkdir: explicitWorkdir, wantMode: toolcache.ModeEphemeral, wantPath: filepath.Join(sandboxTmp, "cache")},
	} {
		t.Run(c.name, func(t *testing.T) {
			scope, err := prepareServeCache(c.noSandbox, c.noInner, c.ephemeral, c.explicitWorkdir, launchWorkdir, sandboxTmp)
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

func TestRunServeRetainsCacheLockAndAllowsOnlyScope(t *testing.T) {
	isolateHome(t)
	shortTmp, err := os.MkdirTemp("/tmp", "omac-serve-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shortTmp) })
	t.Setenv("TMPDIR", shortTmp)

	workdir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "args")
	envPath := filepath.Join(t.TempDir(), "env")
	readyPath := filepath.Join(t.TempDir(), "ready")
	releasePath := filepath.Join(t.TempDir(), "release")
	t.Setenv("OMAC_SERVE_TEST_ARGS", argsPath)
	t.Setenv("OMAC_SERVE_TEST_ENV", envPath)
	t.Setenv("OMAC_SERVE_TEST_READY", readyPath)
	t.Setenv("OMAC_SERVE_TEST_RELEASE", releasePath)

	capturePath := filepath.Join(t.TempDir(), "capture")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$OMAC_SERVE_TEST_ARGS\"\n" +
		"env > \"$OMAC_SERVE_TEST_ENV\"\n" +
		": > \"$OMAC_SERVE_TEST_READY\"\n" +
		"while [ ! -f \"$OMAC_SERVE_TEST_RELEASE\" ]; do sleep 0.01; done\n"
	if err := os.WriteFile(capturePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(workdir, ".opencode", "oh-my-agentic-coder.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	configText := fmt.Sprintf("sandbox:\n  default_profile: capture\n  profiles:\n    capture:\n      command: [%q, %q, %q, %q]\n", capturePath, "--", "{{inner_cmd}}", "{{inner_args}}")
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	env, stderr := launchTestEnv(t, workdir)
	done := make(chan int, 1)
	go func() {
		done <- runServe([]string{"claude", "--sandbox", "capture", "--inner", "/bin/true"}, env)
	}()
	t.Cleanup(func() {
		if _, err := os.Stat(releasePath); os.IsNotExist(err) {
			_ = os.WriteFile(releasePath, nil, 0o600)
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("runServe exited early with %d:\n%s", code, stderr())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for capture process:\n%s", stderr())
		}
		time.Sleep(10 * time.Millisecond)
	}

	scope, err := toolcache.DescribePersistent(toolcache.DomainServe, workdir)
	if err != nil {
		t.Fatalf("describe serve cache: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured args: %v", err)
	}
	capturedArgs := strings.Fields(string(args))
	var cacheAllows []string
	scopeAllows := 0
	for i, arg := range capturedArgs {
		if arg == "--allow" && i+1 < len(capturedArgs) {
			allowed := capturedArgs[i+1]
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

	envData, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read captured environment: %v", err)
	}
	for _, key := range []string{"OMAC_CACHE_DIR", "OMAC_CACHE_MODE"} {
		want := toolcache.Environment(scope.Dir, toolcache.ModePersistent)[key]
		if got := parseEnvironment(string(envData))[key]; got != want {
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

	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("release capture process: %v", err)
	}
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
