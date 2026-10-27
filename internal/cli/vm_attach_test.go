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
// that can open the VM's SSH port. --vm is refused before the session
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
	got, err := vmSandboxEnv(vm.AttachInfo{SSHPort: 31022, User: "alice", AgentKeyPath: key}, tmp)
	if err != nil {
		t.Fatal(err)
	}
	// No DOCKER_* vars: the session reaches Docker only through `omac vm exec`.
	want := map[string]string{
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
	if st, err := os.Stat(filepath.Join(tmp, "vm")); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Errorf("vm dir: %v, want a 0700 dir", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "vm", "docker")); !os.IsNotExist(err) {
		t.Errorf("vm/docker: %v, want absent", err)
	}
	names := slices.Sorted(maps.Keys(got))
	if !slices.Equal(names, slices.Sorted(slices.Values(vmEnvNames))) {
		t.Errorf("vmSandboxEnv sets %v, but vmEnvNames (checked against deny_vars) is %v", names, vmEnvNames)
	}
}

// The host's pinned known_hosts must end up where ssh inside the sandbox
// looks for it, or the sandbox silently trusts the first key it sees.
func TestVMSandboxEnvKnownHostsReachesSandboxSSH(t *testing.T) {
	layout := vm.NewLayout(t.TempDir())
	const pinned = "[127.0.0.1]:31022 ssh-ed25519 AAAA\n"
	for path, content := range map[string]string{layout.AgentKey: "PRIVATE KEY\n", layout.KnownHosts: pinned} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	vars, err := vmSandboxEnv(vm.AttachInfo{SSHPort: 31022, User: "alice", AgentKeyPath: layout.AgentKey}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conn, ok, err := vm.ConnFromEnv(func(k string) string { return vars[k] })
	if err != nil || !ok {
		t.Fatalf("ConnFromEnv = %v, %v", ok, err)
	}
	st, err := os.Stat(conn.Target.KnownHosts)
	if err != nil {
		t.Fatalf("known_hosts where sandbox ssh reads it: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("known_hosts mode = %v, want 0600", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(conn.Target.KnownHosts); string(b) != pinned {
		t.Errorf("known_hosts content = %q, want %q", b, pinned)
	}
}

func TestVMSandboxEnvWithoutKnownHosts(t *testing.T) {
	scope := t.TempDir()
	if _, err := vmSandboxEnv(vm.AttachInfo{AgentKeyPath: testAgentKey(t)}, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(scope, "vm", "known_hosts")); !os.IsNotExist(err) {
		t.Fatalf("known_hosts: %v, want absent", err)
	}
}

// Without the pinned entry, the session's ssh would trust whatever key it
// sees first, so a known_hosts that exists but cannot be read is an error.
func TestVMSandboxEnvUnreadableKnownHosts(t *testing.T) {
	key := testAgentKey(t)
	if err := os.Mkdir(filepath.Join(filepath.Dir(key), "known_hosts"), 0o755); err != nil {
		t.Fatal(err)
	}
	scope := t.TempDir()
	if _, err := vmSandboxEnv(vm.AttachInfo{AgentKeyPath: key}, scope); err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("vmSandboxEnv = %v, want an error naming known_hosts", err)
	}
	if _, err := os.Stat(filepath.Join(scope, "vm", "agent_key")); !os.IsNotExist(err) {
		t.Fatalf("agent_key copy: %v, want absent", err)
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
	// its known_hosts would pin a VM of that session's choice.
	planted := t.TempDir()
	if err := os.MkdirAll(filepath.Join(planted, "vm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, "vm", "known_hosts"), []byte("planted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := vmSandboxEnv(vm.AttachInfo{AgentKeyPath: key}, planted); err == nil {
		t.Error("vmSandboxEnv reused a planted vm directory")
	}
}

func TestVMProfileMisuse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	denies := func(pats ...string) *sandboxprofile.Profile {
		p := sandboxprofile.DefaultProfile()
		p.Environment.DenyVars = pats
		return p
	}
	if msg := vmProfileMisuse(sandboxPlan{Native: true, PolicyRef: "default", Policy: denies("AWS_*", "DOCKER_*")}); msg != "" {
		t.Errorf("unrelated deny_vars refused: %s", msg)
	}
	if msg := vmProfileMisuse(sandboxPlan{Name: "nono"}); !strings.Contains(msg, "--vm needs omac's own sandbox") {
		t.Errorf("non-native plan: %q", msg)
	}
	for _, pat := range []string{"OMAC_VM_*", "OMAC_VM_SSH_KEY", "*"} {
		msg := vmProfileMisuse(sandboxPlan{Native: true, PolicyRef: "strict", Policy: denies(pat)})
		if !strings.Contains(msg, `"strict"`) || !strings.Contains(msg, "deny_vars") {
			t.Errorf("deny_vars %q: %q, want a refusal naming the profile", pat, msg)
		}
	}
}

// The sandbox must never reach the VM state: it holds the admin key and the
// session markers.
func TestVMProfileMisuseRefusesReachableState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root, err := vm.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	unprotected := sandboxprofile.DefaultProfile()
	unprotected.Filesystem.OverrideDeny = []string{root}
	learnLauncher := config.SandboxProfile{Command: []string{"{{self}}", "sandbox", "run", "--learn", "--", "{{inner_cmd}}"}}
	cases := []struct {
		name string
		plan sandboxPlan
		want string
	}{
		{"override_deny unblocks the state", sandboxPlan{Native: true, PolicyRef: "custom", Policy: unprotected}, root},
		{"launcher runs --learn", sandboxPlan{Name: "learner", Native: true, Launcher: learnLauncher, Policy: sandboxprofile.DefaultProfile()}, "--learn"},
		{"policy not loaded", sandboxPlan{Native: true, PolicyRef: "broken", PolicyErr: errors.New("bad json")}, "bad json"},
	}
	for _, c := range cases {
		if msg := vmProfileMisuse(c.plan); !strings.Contains(msg, c.want) {
			t.Errorf("%s: %q, want a refusal containing %q", c.name, msg, c.want)
		}
	}
	builtin := config.DefaultLauncherConfig().Sandbox.Profiles["builtin"].Command
	builtinLearn := injectSandboxFlag(slices.Clone(builtin), "--learn", "")
	plan := sandboxPlan{Name: "builtin-learn", Native: true, PolicyRef: "default", Launcher: config.SandboxProfile{Command: builtinLearn}, Policy: sandboxprofile.DefaultProfile()}
	if msg := vmProfileMisuse(plan); !strings.Contains(msg, "--learn") {
		t.Errorf("builtin launcher with --learn: %q, want a refusal", msg)
	}
	for _, cmd := range [][]string{
		builtin,
		{"{{self}}", "sandbox", "run", "--", "{{inner_cmd}}", "--learn"},
	} {
		plan := sandboxPlan{Native: true, PolicyRef: "default", Launcher: config.SandboxProfile{Command: cmd}, Policy: sandboxprofile.DefaultProfile()}
		if msg := vmProfileMisuse(plan); msg != "" {
			t.Errorf("launcher %q refused: %s", cmd, msg)
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
	if got := s.grant(argv); !slices.Equal(got, argv) {
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
	return vm.AttachInfo{SSHPort: 1, AgentKeyPath: a.agentKey}, a.attachErr
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
	if _, err := attachVM(context.Background(), devnullEnv(t), t.TempDir()); err == nil {
		t.Fatal("attachVM succeeded without an agent key, want an error")
	}
	if entries, _ := os.ReadDir(sessions); len(entries) != 0 {
		t.Errorf("markers left after a failed key copy: %v", entries)
	}
}

// The opencode v2 pin hands the sandbox its own XDG_STATE_HOME, so the
// baseline entries no longer name the host's VM state. Every native launch,
// with or without --vm, must still keep the sandbox out of it.
func TestSandboxDeniesKeepHostVMStateUnderAnotherStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	hostRoot, err := vm.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	native := config.SandboxProfile{Command: []string{"{{self}}", "sandbox", "run", "--"}}
	argv := injectSandboxDenies([]string{"omac", "sandbox", "run", "--", "opencode"}, nil, native)
	flags, err := sandboxprofile.ParseFlags(argv[3:])
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, ok := sandboxrun.NewProtectedPathSet(sandboxprofile.DefaultProfile()).IsProtected(hostRoot); ok {
		t.Fatal("the baseline alone protects the host's VM state; this test no longer checks the launch's own deny")
	}
	session, _ := sandboxprofile.Merge(sandboxprofile.DefaultProfile(), flags)
	if _, ok := sandboxrun.NewProtectedPathSet(session).IsProtected(hostRoot); !ok {
		t.Errorf("a session with its own XDG_STATE_HOME can reach the host's VM state %s; argv %q", hostRoot, argv)
	}
}

// The XDG spec says to ignore a relative XDG_STATE_HOME; `sandbox run`
// refuses a relative --deny-literal, so passing one on stops every launch.
func TestSandboxDeniesIgnoreRelativeStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "rel")
	native := config.SandboxProfile{Command: []string{"{{self}}", "sandbox", "run", "--"}}
	argv := injectSandboxDenies([]string{"omac", "sandbox", "run", "--", "opencode"}, nil, native)
	want := []string{"omac", "sandbox", "run", "--deny-literal", filepath.Join(home, ".local", "state", "omac", "vm"), "--", "opencode"}
	if !equalStrings(argv, want) {
		t.Fatalf("injectSandboxDenies = %v, want %v", argv, want)
	}
	flags, err := sandboxprofile.ParseFlags(argv[3:])
	if err != nil {
		t.Fatal(err)
	}
	session, _ := sandboxprofile.Merge(sandboxprofile.DefaultProfile(), flags)
	if err := session.Validate(); err != nil {
		t.Errorf("the launch's sandbox profile is invalid: %v", err)
	}
}

func TestVMSessionGrant(t *testing.T) {
	argv := []string{"omac", "sandbox", "run", "--profile", "default", "--", "opencode"}
	s := &vmSession{info: vm.AttachInfo{SSHPort: 31022}}
	got := s.grant(argv)
	// Only the ssh port: no Docker port and no --allow-env DOCKER_*.
	want := []string{"omac", "sandbox", "run", "--profile", "default",
		"--open-port", "31022",
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
	srv := &serveServer{env: makeEnv(t.TempDir()), vmEnv: map[string]string{"OMAC_VM_SSH_PORT": "31022", "OMAC_VM_USER": "alice"}}
	env := srv.baseEnv()
	if env["OMAC_VM_SSH_PORT"] != "31022" || env["OMAC_VM_USER"] != "alice" {
		t.Errorf("baseEnv lacks the VM vars: %v", env)
	}
	if _, has := (&serveServer{env: makeEnv(t.TempDir())}).baseEnv()["OMAC_VM_USER"]; has {
		t.Error("baseEnv sets OMAC_VM_USER without --vm")
	}
}
