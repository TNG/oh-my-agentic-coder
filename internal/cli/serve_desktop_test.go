package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDesktopStateHome(t *testing.T) {
	for _, tc := range []struct {
		raw, want, wantErr string
	}{
		{raw: "/home/a/.local/state", want: "/home/a/.local/state"},
		{raw: "/home/a/.local/state/", want: "/home/a/.local/state"},
		{raw: "/home/a/x/../state", want: "/home/a/state"},
		{raw: "relative/dir", wantErr: `XDG_STATE_HOME="relative/dir" must be an absolute path`},
		{raw: "", wantErr: `because HOME is not set. Set HOME, or export XDG_STATE_HOME`},
	} {
		got, err := desktopStateHome(tc.raw)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("desktopStateHome(%q) error = %v, want %q", tc.raw, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("desktopStateHome(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
}

// Each setup that cannot protect OpenCode Desktop's stores is refused
// before serve sets anything up, with a message naming the cause.
func TestRunServeDesktopRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		launcher  []string // global sandbox profile "custom"; nil keeps the default
		stateHome string
		want      string
	}{
		{name: "no sandbox", args: []string{"--no-sandbox"}, want: "--for-opencode-desktop cannot be combined with --no-sandbox"},
		{name: "learn", args: []string{"--learn"}, want: "--for-opencode-desktop cannot be combined with --learn"},
		{name: "other backend", launcher: []string{"/bin/echo", "{{inner_cmd}}"}, want: `needs omac's built-in sandbox, and the sandbox profile "custom" uses another one`},
		{name: "policy not loaded", launcher: []string{"{{self}}", "sandbox", "run", "--profile", "missing", "--", "{{inner_cmd}}"}, want: `cannot load the sandbox policy "missing"`},
		{name: "relative state home", stateHome: "relative/dir", want: `XDG_STATE_HOME="relative/dir" must be an absolute path`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			if tc.launcher != nil {
				writeGlobalConfig(t, "custom", tc.launcher)
			}
			if tc.stateHome != "" {
				t.Setenv("XDG_STATE_HOME", tc.stateHome)
			}
			env, stderr := launchTestEnv(t, t.TempDir())
			args := append([]string{"opencode", "--for-opencode-desktop", "--inner", "/bin/true"}, tc.args...)
			if code := runServe(args, env); code != ExitConfigInvalid {
				t.Fatalf("exit = %d, want %d; stderr:\n%s", code, ExitConfigInvalid, stderr())
			}
			if got := stderr(); !strings.Contains(got, tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

// runServe hands the Desktop grants to the built-in sandbox and creates
// the OpenCode runtime folder it grants.
func TestRunServeDesktopGrantsReachTheSandbox(t *testing.T) {
	isolateHome(t)
	shortTmp := shortSocketDir(t)
	t.Setenv("TMPDIR", shortTmp)
	t.Setenv("XDG_RUNTIME_DIR", shortTmp)
	state := filepath.Join(t.TempDir(), "desktop")
	t.Setenv("XDG_STATE_HOME", state)
	argvPath := filepath.Join(t.TempDir(), "argv")
	t.Setenv(recordArgvEnv, argvPath)

	env, stderr := launchTestEnv(t, t.TempDir())
	if code := runServe([]string{"opencode", "--for-opencode-desktop", "--inner", "/bin/true"}, env); code != ExitOK {
		t.Fatalf("exit = %d; stderr:\n%s", code, stderr())
	}
	raw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("the sandbox was not launched: %v; stderr:\n%s", err, stderr())
	}
	argv := strings.Split(string(raw), "\n")
	flags := argv[:slices.Index(argv, "--")]
	runtimeDir := filepath.Join(state, "opencode")
	if !slices.Contains(flags, "--for-opencode-desktop") {
		t.Errorf("--for-opencode-desktop missing: %v", argv)
	}
	if i := slices.Index(flags, runtimeDir); i < 1 || flags[i-1] != "--allow-literal" {
		t.Errorf("--allow-literal %s missing: %v", runtimeDir, argv)
	}
	if fi, err := os.Stat(runtimeDir); err != nil || !fi.IsDir() {
		t.Errorf("runtime folder %s not created: %v", runtimeDir, err)
	}
}

// shortSocketDir returns a new folder short enough for serve's sockets
// below it (<dir>/omac/omac-serve-<n>/bridge.sock), which macOS limits to
// 104 bytes. It tries /tmp, the temp folder and that folder's parent:
// a sandboxed test run can get a long temp folder and no access to /tmp.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	const maxLen = 104 - len("/omac/omac-serve-0123456789/bridge.sock")
	for _, parent := range []string{"/tmp", os.TempDir(), filepath.Dir(os.TempDir())} {
		dir, err := os.MkdirTemp(parent, "s")
		if err != nil {
			continue
		}
		if len(dir) < maxLen {
			t.Cleanup(func() { os.RemoveAll(dir) })
			return dir
		}
		os.Remove(dir)
	}
	t.Skip("no writable folder with a path short enough for unix sockets")
	return ""
}

// The sandbox gets the Desktop flag and only the runtime subdirectory of
// the state dir, never the whole state dir (which holds Desktop's stores).
func TestInjectDesktopGrants(t *testing.T) {
	argv := []string{"omac", "sandbox", "run", "--profile", "default", "--", "opencode", "serve"}
	var stderr bytes.Buffer
	out := injectDesktopGrants(argv, &stderr, []string{"/w/project"}, "/s/desktop")
	sep := slices.Index(out, "--")
	if sep < 0 {
		t.Fatalf("argv lost its -- separator: %v", out)
	}
	flags := out[:sep]
	hasPair := func(flag, value string) bool {
		for i := 0; i+1 < len(flags); i++ {
			if flags[i] == flag && flags[i+1] == value {
				return true
			}
		}
		return false
	}
	if !slices.Contains(flags, "--for-opencode-desktop") {
		t.Errorf("--for-opencode-desktop missing: %v", out)
	}
	if !hasPair("--allow-literal", "/s/desktop/opencode") {
		t.Errorf("--allow-literal for the runtime dir missing: %v", out)
	}
	if !hasPair("--allow-literal", "/w/project") {
		t.Errorf("--allow-literal for the project missing: %v", out)
	}
	if hasPair("--allow", "/s/desktop") || hasPair("--allow-literal", "/s/desktop") {
		t.Errorf("the whole Desktop state dir is granted: %v", out)
	}
	if !slices.Equal(out[sep:], argv[5:]) {
		t.Errorf("inner command changed: %v", out[sep:])
	}
}
