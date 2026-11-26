package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func ptyHarness(t *testing.T, name string) config.Harness {
	t.Helper()
	h, ok := config.LookupHarness(name)
	if !ok {
		t.Fatalf("%s harness missing", name)
	}
	return h
}

func ptyPlan(denyVars ...string) sandboxPlan {
	return sandboxPlan{
		Name:      "builtin",
		Native:    true,
		PolicyRef: "default",
		Policy:    &sandboxprofile.Profile{Environment: sandboxprofile.Environment{DenyVars: denyVars}},
	}
}

var productionPTYSocketParent = ptySocketParent

func TestPTYSocketParentDefaultIsTmp(t *testing.T) {
	if productionPTYSocketParent != "/tmp" {
		t.Errorf("default ptySocketParent = %q, want /tmp", productionPTYSocketParent)
	}
}

func TestPTYSocketPathFitsMacOSLimit(t *testing.T) {
	const tail = "/opencode-pty-4294967295/" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + ".sock"
	folderName := strings.Replace(ptySocketPattern, "*", "4294967295", 1)
	if n := len("/private/tmp/" + folderName + tail); n >= 104 {
		t.Errorf("worst-case socket path is %d bytes, want < 104", n)
	}
}

func TestNewPTYSocketDir(t *testing.T) {
	oc := ptyHarness(t, "opencode")
	first, err := newPTYSocketDir(oc, ptyPlan(), false)
	if err != nil || first == nil {
		t.Fatalf("newPTYSocketDir() = %v, %v", first, err)
	}
	defer first.Close()
	second, err := newPTYSocketDir(oc, ptyPlan(), false)
	if err != nil || second == nil {
		t.Fatalf("second newPTYSocketDir() = %v, %v", second, err)
	}
	defer second.Close()
	if first.Dir == second.Dir {
		t.Errorf("two launches share %s", first.Dir)
	}
	if filepath.Dir(first.Dir) != ptySocketParent {
		t.Errorf("parent = %s, want %s", filepath.Dir(first.Dir), ptySocketParent)
	}
	if first.EnvVar != "OPENCODE_PTY_SOCKET_DIR" {
		t.Errorf("EnvVar = %q", first.EnvVar)
	}
	info, err := os.Stat(first.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("mode = %o, want 700", info.Mode().Perm())
	}
	first.Close()
	if _, err := os.Stat(first.Dir); !os.IsNotExist(err) {
		t.Errorf("folder remains after Close: %v", err)
	}

	for name, tc := range map[string]struct {
		harness   config.Harness
		plan      sandboxPlan
		noSandbox bool
	}{
		"no sandbox":    {oc, ptyPlan(), true},
		"non-native":    {oc, sandboxPlan{}, false},
		"other harness": {ptyHarness(t, "claude-code"), ptyPlan(), false},
	} {
		got, err := newPTYSocketDir(tc.harness, tc.plan, tc.noSandbox)
		if got != nil || err != nil {
			t.Errorf("%s: newPTYSocketDir() = %v, %v; want nil, nil", name, got, err)
		}
	}
}

func TestPTYSocketDirInstallAndApply(t *testing.T) {
	dir := &ptySocketDir{Dir: "/tmp/omac-pty-1", EnvVar: "OPENCODE_PTY_SOCKET_DIR"}
	argv := []string{"omac", "sandbox", "run", "--", "opencode"}

	got := dir.install(argv, ptyPlan())
	for _, pair := range [][2]string{{"--allow-unix-dir", dir.Dir}, {"--allow-env", dir.EnvVar}} {
		if !containsPair(got, pair[0], pair[1]) {
			t.Errorf("argv lacks %s %s: %v", pair[0], pair[1], got)
		}
	}

	emptyAllow := ptyPlan()
	emptyAllow.Policy.Environment.AllowVars = nil
	if !containsPair(dir.install(argv, emptyAllow), "--allow-env", dir.EnvVar) {
		t.Error("--allow-env missing when allow_vars is empty")
	}

	extra := map[string]string{dir.EnvVar: "/host/value"}
	dir.apply(extra)
	if extra[dir.EnvVar] != dir.Dir {
		t.Errorf("apply left %q, want %q", extra[dir.EnvVar], dir.Dir)
	}

	var none *ptySocketDir
	if !slices.Equal(none.install(argv, ptyPlan()), argv) {
		t.Error("nil install changed argv")
	}
	untouched := map[string]string{dir.EnvVar: "/host/value"}
	none.apply(untouched)
	if untouched[dir.EnvVar] != "/host/value" || len(untouched) != 1 {
		t.Errorf("nil apply changed map: %v", untouched)
	}
	none.Close()
}

func containsPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

func TestPTYSocketDirDenied(t *testing.T) {
	oc := ptyHarness(t, "opencode")
	for _, deny := range []string{"OPENCODE_*", "OPENCODE_PTY_SOCKET_DIR"} {
		err := ptySocketDirDenied(oc, ptyPlan(deny), false)
		if err == nil || !strings.Contains(err.Error(), "OPENCODE_PTY_SOCKET_DIR") || !strings.Contains(err.Error(), `"default"`) {
			t.Errorf("deny %q: error must name the variable and the profile: %v", deny, err)
		}
	}
	for name, err := range map[string]error{
		"unrelated deny": ptySocketDirDenied(oc, ptyPlan("XDG_*"), false),
		"other harness":  ptySocketDirDenied(ptyHarness(t, "claude-code"), ptyPlan("*"), false),
		"no sandbox":     ptySocketDirDenied(oc, ptyPlan("*"), true),
		"non-native":     ptySocketDirDenied(oc, sandboxPlan{}, false),
	} {
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
		}
	}
}

type ptyLaunch struct {
	code   int
	args   []string
	env    map[string]string
	stderr string
}

func launchForPTYSocketDir(t *testing.T, command, harnessName string, noSandbox bool, denyVars []string) ptyLaunch {
	t.Helper()
	shortTmp, err := os.MkdirTemp("/tmp", "omac-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(shortTmp) })
	t.Setenv("TMPDIR", shortTmp)
	t.Setenv("XDG_RUNTIME_DIR", shortTmp)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("OPENCODE_PTY_SOCKET_DIR", "/host/value")

	argsPath := filepath.Join(t.TempDir(), "args")
	envPath := filepath.Join(t.TempDir(), "env")
	t.Setenv("OMAC_TEST_ARGS", argsPath)
	t.Setenv("OMAC_TEST_ENV", envPath)

	profileJSON, err := json.Marshal(map[string]any{"environment": map[string]any{"deny_vars": denyVars}})
	if err != nil {
		t.Fatal(err)
	}
	profileDir := filepath.Join(home, ".config", "omac", "sandbox-profiles")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "captured.json"), profileJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	configText := fmt.Sprintf("sandbox:\n  default_profile: capture\n  profiles:\n    capture:\n      command: [%q, %q, %q, %q, %q, %q, %q, %q]\n",
		"{{self}}", "sandbox", "run", "--profile", "captured", "--", "{{inner_cmd}}", "{{inner_args}}")
	configPath := filepath.Join(home, ".config", "omac", "config.yaml")
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	capturePath := filepath.Join(t.TempDir(), "capture")
	if err := os.WriteFile(capturePath, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OMAC_TEST_ARGS\"\nenv > \"$OMAC_TEST_ENV\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env, stderr := launchTestEnv(t, t.TempDir())
	var code int
	switch command {
	case "start":
		code = runLaunch(env, launchOpts{
			label:            "start",
			harness:          ptyHarness(t, harnessName),
			innerCmdOverride: capturePath,
			noSandbox:        noSandbox,
		})
	case "serve":
		args := []string{harnessName, "--no-audit", "--inner", capturePath}
		if noSandbox {
			args = append(args, "--no-sandbox")
		}
		code = runServe(args, env)
	default:
		t.Fatalf("unknown command %q", command)
	}
	result := ptyLaunch{code: code, stderr: stderr()}
	if args, err := os.ReadFile(argsPath); err == nil {
		result.args = strings.Fields(string(args))
	}
	if envData, err := os.ReadFile(envPath); err == nil {
		result.env = parseEnvironment(string(envData))
	}
	return result
}

func assertOpenCodeGetsOwnPTYSocketDir(t *testing.T, command string) {
	t.Helper()
	const variable = "OPENCODE_PTY_SOCKET_DIR"
	before, _ := filepath.Glob(filepath.Join(ptySocketParent, ptySocketPattern))

	t.Run("opencode sandboxed", func(t *testing.T) {
		got := launchForPTYSocketDir(t, command, "opencode", false, nil)
		if got.code != ExitOK {
			t.Fatalf("exit = %d, want ExitOK\nstderr:\n%s", got.code, got.stderr)
		}
		dir := got.env[variable]
		if !strings.HasPrefix(dir, filepath.Join(ptySocketParent, "omac-pty-")) {
			t.Fatalf("%s = %q, want a folder under %s", variable, dir, ptySocketParent)
		}
		if !containsPair(got.args, "--allow-unix-dir", dir) {
			t.Errorf("argv lacks --allow-unix-dir %s: %v", dir, got.args)
		}
		if !containsPair(got.args, "--allow-env", variable) {
			t.Errorf("argv lacks --allow-env %s: %v", variable, got.args)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("folder remains after the launch: %v", err)
		}
	})

	for _, tc := range []struct {
		name      string
		harness   string
		noSandbox bool
	}{
		{"other harness", "claude", false},
		{"no sandbox", "opencode", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := launchForPTYSocketDir(t, command, tc.harness, tc.noSandbox, nil)
			if got.code != ExitOK {
				t.Fatalf("exit = %d, want ExitOK\nstderr:\n%s", got.code, got.stderr)
			}
			if slices.Contains(got.args, "--allow-unix-dir") {
				t.Errorf("argv grants a unix dir: %v", got.args)
			}
			if got.env[variable] != "/host/value" {
				t.Errorf("%s = %q, want the host value /host/value", variable, got.env[variable])
			}
		})
	}

	t.Run("deny_vars matches", func(t *testing.T) {
		got := launchForPTYSocketDir(t, command, "opencode", false, []string{"OPENCODE_*"})
		if got.code != ExitConfigInvalid {
			t.Errorf("exit = %d, want ExitConfigInvalid\nstderr:\n%s", got.code, got.stderr)
		}
		if !strings.Contains(got.stderr, "deny_vars denies "+variable) {
			t.Errorf("stderr lacks the denial:\n%s", got.stderr)
		}
		if got.args != nil {
			t.Errorf("launcher ran despite the denial: %v", got.args)
		}
	})

	after, _ := filepath.Glob(filepath.Join(ptySocketParent, ptySocketPattern))
	if len(after) != len(before) {
		t.Errorf("launches leaked folders: before %v, after %v", before, after)
	}
}

func TestStartOpenCodeGetsOwnPTYSocketDir(t *testing.T) {
	assertOpenCodeGetsOwnPTYSocketDir(t, "start")
}

func TestServeOpenCodeGetsOwnPTYSocketDir(t *testing.T) {
	assertOpenCodeGetsOwnPTYSocketDir(t, "serve")
}

func TestServeNoInnerCreatesNoPTYSocketDir(t *testing.T) {
	omac := buildOmacWithTestPTYSocketParent(t)
	isolateHome(t)
	root := t.TempDir()
	before, _ := filepath.Glob(filepath.Join(ptySocketParent, ptySocketPattern))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, omac, "serve", "opencode", "--no-inner", "--no-audit")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "TMPDIR=.")
	stdout, err := os.Create(filepath.Join(root, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		out, err := os.ReadFile(stdout.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "OMAC_CONTROL_BASE=") {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("control-plane-only exited before readiness: %v", err)
		case <-ctx.Done():
			t.Fatal("control-plane-only readiness timed out")
		case <-time.After(10 * time.Millisecond):
		}
	}
	after, _ := filepath.Glob(filepath.Join(ptySocketParent, ptySocketPattern))
	if len(after) != len(before) {
		t.Errorf("--no-inner created a pty socket folder: before %v, after %v", before, after)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
	<-done
}
