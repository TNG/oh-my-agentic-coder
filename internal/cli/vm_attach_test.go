package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

func TestParseLaunchArgsVM(t *testing.T) {
	for _, cmd := range []string{"start", "continue"} {
		opts, code := parseLaunchArgs(cmd, []string{"--vm"}, devnullEnv(t))
		if code != ExitOK || !opts.vm {
			t.Errorf("%s --vm: code=%d vm=%v, want ExitOK and true", cmd, code, opts.vm)
		}
	}
	if _, code := parseLaunchArgs("start", []string{"--vm", "--no-sandbox"}, devnullEnv(t)); code != ExitMisuse {
		t.Errorf("--vm --no-sandbox: code = %d, want ExitMisuse", code)
	}
}

func TestVMAttachExitCode(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("x: %w", vm.ErrLimaMissing), ExitPrerequisiteMissing},
		{vm.ErrNotCreated, ExitPrerequisiteMissing},
		{vm.ErrNotRunning, ExitPrerequisiteMissing},
		{vm.ErrOnline, ExitPrerequisiteMissing},
		{vm.ErrFirewallMissing, ExitPrerequisiteMissing},
		{errors.New("docker did not answer"), ExitGeneric},
	}
	for _, c := range cases {
		if got := vmAttachExitCode(c.err); got != c.want {
			t.Errorf("vmAttachExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

// stubVMAttachFailure makes prepareVMAttach fail with err, so a launch that
// reaches the attach step ends with a non-zero code.
func stubVMAttachFailure(t *testing.T, err error) {
	t.Helper()
	orig := prepareVMAttach
	prepareVMAttach = func(context.Context, *Env) (vm.AttachInfo, *vmMarkers, error) {
		return vm.AttachInfo{}, nil, err
	}
	t.Cleanup(func() { prepareVMAttach = orig })
}

// vmCaptureFixture configures a "capture" sandbox profile whose command is
// script, so runLaunch runs end-to-end without a real sandbox backend and the
// script records what the wiring injected. Sandbox profiles are trusted only
// from the global config, so the profile goes into the isolated HOME.
func vmCaptureFixture(t *testing.T, script string) (workdir, capturePath string) {
	t.Helper()
	isolateHome(t)
	shortTmp, err := os.MkdirTemp("/tmp", "omac-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shortTmp) })
	t.Setenv("TMPDIR", shortTmp)
	// Runtime dir (sockets, sidecar logs) outside HOME, as launchCacheCapture does.
	t.Setenv("XDG_RUNTIME_DIR", shortTmp)

	workdir = t.TempDir()
	capturePath = filepath.Join(t.TempDir(), "capture")
	if err := os.WriteFile(capturePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(os.Getenv("HOME"), ".config", "omac", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	configText := fmt.Sprintf("sandbox:\n  default_profile: capture\n  profiles:\n    capture:\n      command: [%q, %q, %q, %q]\n",
		capturePath, "--", "{{inner_cmd}}", "{{inner_args}}")
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	return workdir, capturePath
}

func testAgentKey(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent_key")
	if err := os.WriteFile(p, []byte("PRIVATE KEY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The capture profile is not omac's builtin sandbox, which is the only one
// that can open the VM's ports. --vm is refused before the session
// registers with the VM; the stub fails the launch if it is reached.
func TestRunLaunchVMRefusesNonNativeProfile(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	workdir, capturePath := vmCaptureFixture(t, "#!/bin/sh\ntouch "+ran+"\n")
	env, stderr := launchTestEnv(t, workdir)
	harness, _ := config.LookupHarness("claude")
	stubVMAttachFailure(t, errors.New("prepareVMAttach must not be called on a non-native profile"))

	code := runLaunch(env, launchOpts{label: "start", harness: harness, vm: true, innerCmdOverride: capturePath})
	if code != ExitMisuse {
		t.Fatalf("runLaunch code = %d, want ExitMisuse\nstderr:\n%s", code, stderr())
	}
	if !strings.Contains(stderr(), "--vm needs omac's own sandbox") {
		t.Errorf("stderr lacks the refusal:\n%s", stderr())
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the sandbox command ran although --vm was refused")
	}
}

func TestServeVMRefusesNonNativeProfile(t *testing.T) {
	ran := filepath.Join(t.TempDir(), "ran")
	workdir, _ := vmCaptureFixture(t, "#!/bin/sh\ntouch "+ran+"\n")
	env, stderr := launchTestEnv(t, workdir)
	stubVMAttachFailure(t, errors.New("prepareVMAttach must not be called on a non-native profile"))

	code := runServe([]string{"claude", "--vm", "--inner", "/bin/true"}, env)
	if code != ExitMisuse {
		t.Fatalf("runServe code = %d, want ExitMisuse\nstderr:\n%s", code, stderr())
	}
	if !strings.Contains(stderr(), "--vm needs omac's own sandbox") {
		t.Errorf("stderr lacks the refusal:\n%s", stderr())
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the sandbox command ran although --vm was refused")
	}
}

// The existing launch tests already pin the argv without --vm; this one
// pins that no VM variable leaks into the environment either.
func TestRunLaunchWithoutVMAddsNothing(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	dump := filepath.Join(t.TempDir(), "dump")
	script := "#!/bin/sh\nenv >" + dump + "\nprintf 'ARGV:%s\\n' \"$*\" >>" + dump + "\n"
	workdir, capturePath := vmCaptureFixture(t, script)
	env, stderr := launchTestEnv(t, workdir)
	harness, _ := config.LookupHarness("claude")
	stubVMAttachFailure(t, errors.New("prepareVMAttach must not be called without --vm"))

	if code := runLaunch(env, launchOpts{label: "start", harness: harness, innerCmdOverride: capturePath}); code != ExitOK {
		t.Fatalf("runLaunch code = %d\nstderr:\n%s", code, stderr())
	}
	raw, _ := os.ReadFile(dump)
	for _, bad := range []string{"OMAC_VM_", "--vm", filepath.Join("vm", "docker"), filepath.Join("vm", "agent_key")} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("dump contains %q without --vm:\n%s", bad, raw)
		}
	}
}

func TestRunLaunchVMNeedsSandbox(t *testing.T) {
	isolateHome(t)
	code := runLaunch(makeEnv(t.TempDir()), launchOpts{label: "start", harness: config.DefaultHarness(), vm: true, noSandbox: true})
	if code != ExitMisuse {
		t.Fatalf("code = %d, want ExitMisuse", code)
	}
}

func TestVMSandboxEnv(t *testing.T) {
	tmp := t.TempDir()
	key := testAgentKey(t)
	got, err := vmSandboxEnv(vm.AttachInfo{DockerPort: 31234, SSHPort: 31022, User: "alice", AgentKeyPath: key}, tmp)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DOCKER_HOST":      "tcp://127.0.0.1:31234",
		"DOCKER_CONFIG":    filepath.Join(tmp, "vm", "docker"),
		"OMAC_VM_HOST":     "127.0.0.1",
		"OMAC_VM_SSH_PORT": "31022",
		"OMAC_VM_SSH_KEY":  filepath.Join(tmp, "vm", "agent_key"),
		"OMAC_VM_USER":     "alice",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("env = %v\nwant  %v", got, want)
	}
	if st, err := os.Stat(want["OMAC_VM_SSH_KEY"]); err != nil {
		t.Errorf("key copy: %v", err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("key copy mode = %v, want 0600", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(want["OMAC_VM_SSH_KEY"]); string(b) != "PRIVATE KEY\n" {
		t.Errorf("key copy content = %q", b)
	}
	for _, d := range []string{filepath.Join(tmp, "vm"), want["DOCKER_CONFIG"]} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, want a 0700 dir", d, err)
		}
	}
	names := slices.Sorted(maps.Keys(got))
	if !slices.Equal(names, slices.Sorted(slices.Values(vmEnvNames))) {
		t.Errorf("vmSandboxEnv sets %v, but vmEnvNames (checked against deny_vars) is %v", names, vmEnvNames)
	}
}

func TestVMSandboxEnvMissingKey(t *testing.T) {
	if _, err := vmSandboxEnv(vm.AttachInfo{AgentKeyPath: filepath.Join(t.TempDir(), "nope")}, t.TempDir()); err == nil {
		t.Fatal("vmSandboxEnv succeeded without an agent key")
	}
}

// The key copy must land in a directory omac just created, never in one that
// already exists: an existing one may be a symlink planted by a sandbox.
func TestVMSandboxEnvRefusesExistingDir(t *testing.T) {
	key := testAgentKey(t)
	tmp, target := t.TempDir(), t.TempDir()
	if err := os.Symlink(target, filepath.Join(tmp, "vm")); err != nil {
		t.Fatal(err)
	}
	if _, err := vmSandboxEnv(vm.AttachInfo{AgentKeyPath: key}, tmp); err == nil {
		t.Fatal("vmSandboxEnv accepted an existing vm directory")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("symlink target was written: %v", entries)
	}

	// A real directory planted by another session must not be reused either:
	// its docker/config.json would configure this session's docker.
	planted := t.TempDir()
	if err := os.MkdirAll(filepath.Join(planted, "vm", "docker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "vm", "docker", "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := vmSandboxEnv(vm.AttachInfo{AgentKeyPath: key}, planted); err == nil {
		t.Error("vmSandboxEnv reused a planted vm directory")
	}
}

func TestVMProfileMisuse(t *testing.T) {
	denies := func(pats ...string) *sandboxprofile.Profile {
		return &sandboxprofile.Profile{Environment: sandboxprofile.Environment{DenyVars: pats}}
	}
	if msg := vmProfileMisuse(sandboxPlan{Native: true, PolicyRef: "default", Policy: denies("AWS_*")}); msg != "" {
		t.Errorf("unrelated deny_vars refused: %s", msg)
	}
	if msg := vmProfileMisuse(sandboxPlan{Name: "nono"}); !strings.Contains(msg, "--vm needs omac's own sandbox") {
		t.Errorf("non-native plan: %q", msg)
	}
	for _, pat := range []string{"DOCKER_*", "DOCKER_CONFIG", "OMAC_VM_*", "*"} {
		msg := vmProfileMisuse(sandboxPlan{Native: true, PolicyRef: "strict", Policy: denies(pat)})
		if !strings.Contains(msg, `"strict"`) || !strings.Contains(msg, "deny_vars") {
			t.Errorf("deny_vars %q: %q, want a refusal naming the profile", pat, msg)
		}
	}
}

func TestVMSessionTracksChildUntilRelease(t *testing.T) {
	layout := vm.NewLayout(t.TempDir())
	markers := &vmMarkers{layout: layout}
	if err := markers.add(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	s := &vmSession{markers: markers, stderr: io.Discard}
	s.trackChild(4242)
	if _, err := os.Stat(filepath.Join(layout.SessionsDir, "4242")); err != nil {
		t.Fatalf("child marker missing: %v", err)
	}
	s.release()
	if entries, _ := os.ReadDir(layout.SessionsDir); len(entries) != 0 {
		t.Errorf("markers left after release: %v", entries)
	}
	// onReady runs in a goroutine and may fire after the child exited.
	s.trackChild(4243)
	if entries, _ := os.ReadDir(layout.SessionsDir); len(entries) != 0 {
		t.Errorf("marker written after release: %v", entries)
	}
}

// readyHook is what start and serve hand to sandbox.ExecWithReadyPID: it
// must register the child and still run the hook it wraps.
func TestVMSessionReadyHook(t *testing.T) {
	layout := vm.NewLayout(t.TempDir())
	s := &vmSession{markers: &vmMarkers{layout: layout}, stderr: io.Discard}
	var wrapped int
	s.readyHook(func(pid int) { wrapped = pid })(4242)
	if wrapped != 4242 {
		t.Errorf("wrapped hook got pid %d, want 4242", wrapped)
	}
	if _, err := os.Stat(filepath.Join(layout.SessionsDir, "4242")); err != nil {
		t.Errorf("child marker missing: %v", err)
	}
	s.readyHook(nil)(4243)
	if _, err := os.Stat(filepath.Join(layout.SessionsDir, "4243")); err != nil {
		t.Errorf("child marker missing without a wrapped hook: %v", err)
	}
	var none *vmSession
	if none.readyHook(nil) != nil {
		t.Error("nil session must keep a nil hook")
	}
}

func TestVMSessionNilIsNoOp(t *testing.T) {
	var s *vmSession
	argv := []string{"omac", "sandbox", "run", "--", "x"}
	if got := s.grant(argv, sandboxPlan{Native: true}); !slices.Equal(got, argv) {
		t.Errorf("nil grant changed argv: %q", got)
	}
	if s.sandboxEnv() != nil {
		t.Error("nil session has env")
	}
	s.trackChild(1)
	s.release()
}

func TestServeVMRefusesDesktop(t *testing.T) {
	if _, ok := parseServeArgs([]string{"--vm", "--for-opencode-desktop"}, devnullEnv(t)); ok {
		t.Error("parseServeArgs(--vm --for-opencode-desktop) accepted")
	}
}

// attachOrderManager checks, inside PrepareAttach, that this process's
// session marker already exists.
type attachOrderManager struct {
	fakeVMManager
	sessions  string
	sawMarker bool
	attachErr error
	agentKey  string
}

func (a *attachOrderManager) PrepareAttach(context.Context) (vm.AttachInfo, error) {
	_, err := os.Stat(filepath.Join(a.sessions, strconv.Itoa(os.Getpid())))
	a.sawMarker = err == nil
	return vm.AttachInfo{DockerPort: 1, AgentKeyPath: a.agentKey}, a.attachErr
}

func useAttachOrderManager(t *testing.T, attachErr error) (*attachOrderManager, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, err := vm.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	fake := &attachOrderManager{sessions: vm.NewLayout(root).SessionsDir, attachErr: attachErr}
	orig := newVMManager
	newVMManager = func(*Env) (vmManager, error) { return fake, nil }
	t.Cleanup(func() { newVMManager = orig })
	return fake, fake.sessions
}

func TestPrepareVMAttachRegistersBeforeChecking(t *testing.T) {
	fake, sessions := useAttachOrderManager(t, nil)
	_, markers, err := prepareVMAttach(context.Background(), devnullEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if !fake.sawMarker {
		t.Error("PrepareAttach ran before the session marker was written")
	}
	markers.release()
	if entries, _ := os.ReadDir(sessions); len(entries) != 0 {
		t.Errorf("markers left after release: %v", entries)
	}
}

func TestPrepareVMAttachFailureLeavesNoMarker(t *testing.T) {
	_, sessions := useAttachOrderManager(t, vm.ErrOnline)
	_, markers, err := prepareVMAttach(context.Background(), devnullEnv(t))
	if !errors.Is(err, vm.ErrOnline) || markers != nil {
		t.Fatalf("prepareVMAttach = (markers=%v, %v), want (nil, ErrOnline)", markers != nil, err)
	}
	if entries, _ := os.ReadDir(sessions); len(entries) != 0 {
		t.Errorf("markers left after a failed attach: %v", entries)
	}
}

func TestAttachVMKeyFailureLeavesNoMarker(t *testing.T) {
	_, sessions := useAttachOrderManager(t, nil)
	_, err := attachVM(context.Background(), devnullEnv(t), t.TempDir(), defaultSessionProtection())
	if err == nil || !strings.Contains(err.Error(), "agent key") {
		t.Fatalf("attachVM = %v, want the missing agent key error", err)
	}
	if entries, _ := os.ReadDir(sessions); len(entries) != 0 {
		t.Errorf("markers left after a failed key copy: %v", entries)
	}
}

func defaultSessionProtection() *sandboxrun.ProtectedPathSet {
	return sessionProtectedPaths(false, sandboxPlan{Native: true, Policy: sandboxprofile.DefaultProfile()})
}

func TestAttachVMDefaultProfileAttaches(t *testing.T) {
	fake, _ := useAttachOrderManager(t, nil)
	fake.agentKey = testAgentKey(t)
	s, err := attachVM(context.Background(), devnullEnv(t), t.TempDir(), defaultSessionProtection())
	if err != nil {
		t.Fatalf("attachVM with the default profile: %v", err)
	}
	s.release()
}

// The sandbox must never reach the VM state: it holds the admin key and
// the session markers. start and serve both pass sessionProtectedPaths.
func TestAttachVMRefusesReachableState(t *testing.T) {
	fake, sessions := useAttachOrderManager(t, nil)
	fake.agentKey = testAgentKey(t)
	root := filepath.Dir(sessions)
	unprotected := sandboxprofile.DefaultProfile()
	unprotected.Filesystem.OverrideDeny = []string{root}
	cases := map[string]*sandboxrun.ProtectedPathSet{
		"override_deny removes the folder": sessionProtectedPaths(false, sandboxPlan{Native: true, Policy: unprotected}),
		"serve --learn":                    sessionProtectedPaths(true, sandboxPlan{Native: true, Policy: sandboxprofile.DefaultProfile()}),
		"policy not resolved":              sessionProtectedPaths(false, sandboxPlan{Native: true}),
	}
	for name, protected := range cases {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			_, err := attachVM(context.Background(), devnullEnv(t), tmp, protected)
			if err == nil || !strings.Contains(err.Error(), root) || !strings.Contains(err.Error(), "must keep it protected") {
				t.Fatalf("attachVM = %v, want a refusal naming %s", err, root)
			}
			if entries, _ := os.ReadDir(sessions); len(entries) != 0 {
				t.Errorf("markers left after the refusal: %v", entries)
			}
			if _, err := os.Stat(filepath.Join(tmp, "vm")); err == nil {
				t.Error("agent key copied although the attach was refused")
			}
		})
	}
}

func TestVMSessionGrant(t *testing.T) {
	argv := []string{"omac", "sandbox", "run", "--profile", "default", "--", "opencode"}
	s := &vmSession{info: vm.AttachInfo{DockerPort: 31234, SSHPort: 31022}}
	got := s.grant(argv, sandboxPlan{Native: true})
	want := []string{"omac", "sandbox", "run", "--profile", "default",
		"--open-port", "31234", "--open-port", "31022",
		"--allow-env", "DOCKER_HOST", "--allow-env", "DOCKER_CONFIG",
		"--", "opencode"}
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q\nwant   %q", got, want)
	}
}

func TestServeVMWiring(t *testing.T) {
	p, ok := parseServeArgs([]string{"--vm"}, devnullEnv(t))
	if !ok || !p.vm {
		t.Fatalf("parseServeArgs(--vm): ok=%v vm=%v", ok, p.vm)
	}
	for _, other := range []string{"--no-sandbox", "--no-inner", "--learn"} {
		if _, ok := parseServeArgs([]string{"--vm", other}, devnullEnv(t)); ok {
			t.Errorf("parseServeArgs(--vm %s) accepted", other)
		}
	}
	srv := &serveServer{env: makeEnv(t.TempDir()), vmEnv: map[string]string{"DOCKER_HOST": "tcp://127.0.0.1:31234", "OMAC_VM_USER": "alice"}}
	env := srv.baseEnv()
	if env["DOCKER_HOST"] != "tcp://127.0.0.1:31234" || env["OMAC_VM_USER"] != "alice" {
		t.Errorf("baseEnv lacks the VM vars: %v", env)
	}
	if _, has := (&serveServer{env: makeEnv(t.TempDir())}).baseEnv()["DOCKER_HOST"]; has {
		t.Error("baseEnv sets DOCKER_HOST without --vm")
	}
}
