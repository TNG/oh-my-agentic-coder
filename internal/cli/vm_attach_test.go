package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
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
	prepareVMAttach = func(context.Context, *Env) (vm.AttachInfo, func(), error) {
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
	for _, bad := range []string{"OMAC_VM_", "--vm"} {
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
	scope := t.TempDir()
	key := testAgentKey(t)
	// A stale copy is overwritten, never appended to or kept.
	if err := os.MkdirAll(filepath.Join(scope, "vm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "vm", "agent_key"), []byte("OLD OLD OLD OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := vmSandboxEnv(vm.AttachInfo{DockerPort: 31234, SSHPort: 31022, User: "alice", AgentKeyPath: key}, scope)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"DOCKER_HOST":      "tcp://127.0.0.1:31234",
		"DOCKER_CONFIG":    filepath.Join(scope, "vm", "docker"),
		"OMAC_VM_HOST":     "127.0.0.1",
		"OMAC_VM_SSH_PORT": "31022",
		"OMAC_VM_SSH_KEY":  filepath.Join(scope, "vm", "agent_key"),
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
	for _, d := range []string{filepath.Join(scope, "vm"), want["DOCKER_CONFIG"]} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, want a 0700 dir", d, err)
		}
	}
}

func TestVMSandboxEnvCopiesKnownHosts(t *testing.T) {
	key := testAgentKey(t)
	src := filepath.Join(filepath.Dir(key), "known_hosts")
	if err := os.WriteFile(src, []byte("[127.0.0.1]:31022 ssh-ed25519 AAAA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := t.TempDir()
	if _, err := vmSandboxEnv(vm.AttachInfo{SSHPort: 31022, User: "alice", AgentKeyPath: key}, scope); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(scope, "vm", "known_hosts")
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("known_hosts copy: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("known_hosts mode = %v, want 0600", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(dst); string(b) != "[127.0.0.1]:31022 ssh-ed25519 AAAA\n" {
		t.Errorf("known_hosts content = %q", b)
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

func TestVMSandboxEnvMissingKey(t *testing.T) {
	if _, err := vmSandboxEnv(vm.AttachInfo{AgentKeyPath: filepath.Join(t.TempDir(), "nope")}, t.TempDir()); err == nil {
		t.Fatal("vmSandboxEnv succeeded without an agent key")
	}
}

// A sandboxed session can write the cache dir, so it could plant a symlink
// that redirects the key copy or DOCKER_CONFIG anywhere the host user writes.
func TestVMSandboxEnvRefusesSymlinks(t *testing.T) {
	key := testAgentKey(t)
	for _, planted := range []string{"vm", filepath.Join("vm", "docker")} {
		t.Run(planted, func(t *testing.T) {
			scope, target := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(scope, planted)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(scope, planted)); err != nil {
				t.Fatal(err)
			}
			if _, err := vmSandboxEnv(vm.AttachInfo{AgentKeyPath: key}, scope); err == nil {
				t.Fatalf("vmSandboxEnv accepted a symlinked %s", planted)
			}
			if entries, _ := os.ReadDir(target); len(entries) != 0 {
				t.Errorf("symlink target was written: %v", entries)
			}
		})
	}
}

// attachOrderManager checks, inside PrepareAttach, that this process's
// session marker already exists.
type attachOrderManager struct {
	fakeVMManager
	sessions  string
	sawMarker bool
	attachErr error
}

func (a *attachOrderManager) PrepareAttach(context.Context) (vm.AttachInfo, error) {
	_, err := os.Stat(filepath.Join(a.sessions, strconv.Itoa(os.Getpid())))
	a.sawMarker = err == nil
	return vm.AttachInfo{DockerPort: 1}, a.attachErr
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
	_, release, err := prepareVMAttach(context.Background(), devnullEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if !fake.sawMarker {
		t.Error("PrepareAttach ran before the session marker was written")
	}
	release()
	if entries, _ := os.ReadDir(sessions); len(entries) != 0 {
		t.Errorf("markers left after release: %v", entries)
	}
}

func TestPrepareVMAttachFailureLeavesNoMarker(t *testing.T) {
	_, sessions := useAttachOrderManager(t, vm.ErrOnline)
	_, release, err := prepareVMAttach(context.Background(), devnullEnv(t))
	if !errors.Is(err, vm.ErrOnline) || release != nil {
		t.Fatalf("prepareVMAttach = (release=%v, %v), want (nil, ErrOnline)", release != nil, err)
	}
	if entries, _ := os.ReadDir(sessions); len(entries) != 0 {
		t.Errorf("markers left after a failed attach: %v", entries)
	}
}

func TestInjectVMGrants(t *testing.T) {
	argv := []string{"omac", "sandbox", "run", "--profile", "default", "--", "opencode"}
	got := injectVMGrants(argv, vm.AttachInfo{DockerPort: 31234, SSHPort: 31022}, sandboxPlan{Native: true})
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
	for _, other := range []string{"--no-sandbox", "--no-inner"} {
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
