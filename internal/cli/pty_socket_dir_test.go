package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

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

	var verboseOut strings.Builder
	got := dir.install(argv, ptyPlan(), &verboseOut, true)
	if !strings.Contains(verboseOut.String(), "pty socket folder: "+dir.Dir) {
		t.Errorf("verbose output lacks the folder: %q", verboseOut.String())
	}
	for _, pair := range [][2]string{{"--allow-unix-dir", dir.Dir}, {"--allow-env", dir.EnvVar}} {
		if !containsPair(got, pair[0], pair[1]) {
			t.Errorf("argv lacks %s %s: %v", pair[0], pair[1], got)
		}
	}

	emptyAllow := ptyPlan()
	emptyAllow.Policy.Environment.AllowVars = nil
	if !containsPair(dir.install(argv, emptyAllow, io.Discard, false), "--allow-env", dir.EnvVar) {
		t.Error("--allow-env missing when allow_vars is empty")
	}

	extra := map[string]string{dir.EnvVar: "/host/value"}
	dir.apply(extra)
	if extra[dir.EnvVar] != dir.Dir {
		t.Errorf("apply left %q, want %q", extra[dir.EnvVar], dir.Dir)
	}

	var none *ptySocketDir
	var quiet strings.Builder
	if !slices.Equal(none.install(argv, ptyPlan(), &quiet, true), argv) || quiet.Len() != 0 {
		t.Errorf("nil install changed argv or printed %q", quiet.String())
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
		err := ptySocketDirDenied(oc, ptyPlan("XDG_*", deny), false)
		want := fmt.Sprintf(`profile "default": deny_vars entry %q denies OPENCODE_PTY_SOCKET_DIR`, deny)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("deny %q: error must name the profile, the matching entry and the variable: %v", deny, err)
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

func TestOpenPTYSocketDir(t *testing.T) {
	oc := ptyHarness(t, "opencode")

	var stderr strings.Builder
	dir, code := openPTYSocketDir(&stderr, "omac serve", oc, ptyPlan("OPENCODE_*"), false)
	if dir != nil || code != ExitConfigInvalid || !strings.HasPrefix(stderr.String(), "omac serve: pty socket folder: ") {
		t.Errorf("denied: got %v, %d, %q; want nil, ExitConfigInvalid and the prefixed message", dir, code, stderr.String())
	}

	stderr.Reset()
	parent := ptySocketParent
	ptySocketParent = filepath.Join(t.TempDir(), "missing")
	dir, code = openPTYSocketDir(&stderr, "omac start", oc, ptyPlan(), false)
	ptySocketParent = parent
	if dir != nil || code != ExitIOError || !strings.HasPrefix(stderr.String(), "omac start: pty socket folder: ") {
		t.Errorf("creation fails: got %v, %d, %q; want nil, ExitIOError and the prefixed message", dir, code, stderr.String())
	}

	dir, code = openPTYSocketDir(io.Discard, "omac start", oc, ptyPlan(), false)
	defer dir.Close()
	if dir == nil || code != ExitOK {
		t.Errorf("allowed: got %v, %d; want a folder and ExitOK", dir, code)
	}
}

func TestSandboxPlanProfileLabel(t *testing.T) {
	if got := (sandboxPlan{Name: "builtin", PolicyRef: "default"}).profileLabel(); got != "default" {
		t.Errorf("with a policy profile: %q, want default", got)
	}
	if got := (sandboxPlan{Name: "nono"}).profileLabel(); got != "nono" {
		t.Errorf("without a policy profile: %q, want nono", got)
	}
}

type ptyLaunch struct {
	code   int
	args   []string
	env    map[string]string
	stderr string
}

type ptyLaunchSpec struct {
	harness   string
	noSandbox bool
	denyVars  []string
	// customLauncher runs the capture script as the launcher, instead of
	// `{{self}} sandbox run`, the way a nono launcher is configured.
	customLauncher bool
	// openCodeVersion, when set, names the inner stand-in opencode and makes
	// it answer --version with this version, so omac classifies it.
	openCodeVersion string
}

func launchForPTYSocketDir(t *testing.T, command string, spec ptyLaunchSpec) ptyLaunch {
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

	profileJSON, err := json.Marshal(map[string]any{"environment": map[string]any{"deny_vars": spec.denyVars}})
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

	capturePath := filepath.Join(t.TempDir(), "capture")
	script := "#!/bin/sh\n"
	if spec.openCodeVersion != "" {
		capturePath = filepath.Join(filepath.Dir(capturePath), "opencode")
		script += fmt.Sprintf("if [ \"$1\" = --version ]; then echo %s; exit 0; fi\n", spec.openCodeVersion)
	}
	script += "printf '%s\\n' \"$@\" > \"$OMAC_TEST_ARGS\"\nenv > \"$OMAC_TEST_ENV\"\n"
	if err := os.WriteFile(capturePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	launcher := []string{"{{self}}", "sandbox", "run", "--profile", "captured"}
	if spec.customLauncher {
		launcher = []string{capturePath}
	}
	launcherJSON, err := json.Marshal(append(launcher, "--", "{{inner_cmd}}", "{{inner_args}}"))
	if err != nil {
		t.Fatal(err)
	}
	configText := "sandbox:\n  default_profile: capture\n  profiles:\n    capture:\n      command: " + string(launcherJSON) + "\n"
	configPath := filepath.Join(home, ".config", "omac", "config.yaml")
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	env, stderr := launchTestEnv(t, t.TempDir())
	var code int
	switch command {
	case "start":
		code = runLaunch(env, launchOpts{
			label:            "start",
			harness:          ptyHarness(t, spec.harness),
			innerCmdOverride: capturePath,
			noSandbox:        spec.noSandbox,
		})
	case "serve":
		args := []string{spec.harness, "--no-audit", "--inner", capturePath}
		if spec.noSandbox {
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

func ptySocketFolders(t *testing.T) []string {
	t.Helper()
	folders, err := filepath.Glob(filepath.Join(ptySocketParent, ptySocketPattern))
	if err != nil {
		t.Fatal(err)
	}
	return folders
}

// lateLaunchFailure sets up a launch that fails after the PTY folder exists:
// `omac start` stops at the OpenCode 2.x service pin, `omac serve` (macOS only)
// at the OpenCode 2.0.x project-discovery workaround.
func lateLaunchFailure(t *testing.T, command string) (spec ptyLaunchSpec, wantStderr string) {
	t.Helper()
	if command == "start" {
		return ptyLaunchSpec{harness: "opencode", denyVars: []string{"OPENCODE_CONFIG_DIR"}, openCodeVersion: "2.3.0"},
			"opencode v2 service pin:"
	}
	if runtime.GOOS != "darwin" {
		t.Skip("omac serve has no failure point after the folder outside macOS")
	}
	return ptyLaunchSpec{harness: "opencode", denyVars: []string{"OPENCODE_CONFIG_PROJECT_DISABLE"}, openCodeVersion: "2.0.5"},
		"opencode project discovery:"
}

func assertOpenCodeGetsOwnPTYSocketDir(t *testing.T, command string) {
	t.Helper()
	const variable = "OPENCODE_PTY_SOCKET_DIR"
	before := ptySocketFolders(t)

	t.Run("opencode sandboxed", func(t *testing.T) {
		got := launchForPTYSocketDir(t, command, ptyLaunchSpec{harness: "opencode"})
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
		name string
		spec ptyLaunchSpec
	}{
		{"other harness", ptyLaunchSpec{harness: "claude"}},
		{"no sandbox", ptyLaunchSpec{harness: "opencode", noSandbox: true}},
		{"custom launcher", ptyLaunchSpec{harness: "opencode", customLauncher: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			foldersBefore := ptySocketFolders(t)
			got := launchForPTYSocketDir(t, command, tc.spec)
			if got.code != ExitOK {
				t.Fatalf("exit = %d, want ExitOK\nstderr:\n%s", got.code, got.stderr)
			}
			if got.args == nil {
				t.Fatalf("launcher did not run\nstderr:\n%s", got.stderr)
			}
			if slices.Contains(got.args, "--allow-unix-dir") {
				t.Errorf("argv grants a unix dir: %v", got.args)
			}
			if got.env[variable] != "/host/value" {
				t.Errorf("%s = %q, want the host value /host/value", variable, got.env[variable])
			}
			if foldersAfter := ptySocketFolders(t); len(foldersAfter) != len(foldersBefore) {
				t.Errorf("launch created a folder: before %v, after %v", foldersBefore, foldersAfter)
			}
		})
	}

	t.Run("deny_vars matches", func(t *testing.T) {
		got := launchForPTYSocketDir(t, command, ptyLaunchSpec{harness: "opencode", denyVars: []string{"OPENCODE_*"}})
		if got.code != ExitConfigInvalid {
			t.Errorf("exit = %d, want ExitConfigInvalid\nstderr:\n%s", got.code, got.stderr)
		}
		if !strings.Contains(got.stderr, `deny_vars entry "OPENCODE_*" denies `+variable) {
			t.Errorf("stderr lacks the denial:\n%s", got.stderr)
		}
		if got.args != nil {
			t.Errorf("launcher ran despite the denial: %v", got.args)
		}
	})

	t.Run("launch fails after the folder exists", func(t *testing.T) {
		spec, wantStderr := lateLaunchFailure(t, command)
		foldersBefore := ptySocketFolders(t)
		got := launchForPTYSocketDir(t, command, spec)
		if got.code != ExitConfigInvalid || !strings.Contains(got.stderr, wantStderr) {
			t.Errorf("exit = %d, want ExitConfigInvalid with %q\nstderr:\n%s", got.code, wantStderr, got.stderr)
		}
		if got.args != nil {
			t.Errorf("launcher ran despite the failure: %v", got.args)
		}
		if foldersAfter := ptySocketFolders(t); len(foldersAfter) != len(foldersBefore) {
			t.Errorf("failed launch left a folder: before %v, after %v", foldersBefore, foldersAfter)
		}
	})

	if after := ptySocketFolders(t); len(after) != len(before) {
		t.Errorf("launches leaked folders: before %v, after %v", before, after)
	}
}

func TestStartOpenCodeGetsOwnPTYSocketDir(t *testing.T) {
	assertOpenCodeGetsOwnPTYSocketDir(t, "start")
}

func TestServeOpenCodeGetsOwnPTYSocketDir(t *testing.T) {
	assertOpenCodeGetsOwnPTYSocketDir(t, "serve")
}
