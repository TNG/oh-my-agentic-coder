package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandbox"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestOpenCodeProjectDiscoveryEnvironment(t *testing.T) {
	h, _ := config.LookupHarness("opencode")
	v20 := fakeVersionBinary(t, "#!/bin/sh\nprintf '2.0.20\\n'\n")
	v21 := fakeVersionBinary(t, "#!/bin/sh\nprintf '2.1.2\\n'\n")
	v1 := fakeVersionBinary(t, "#!/bin/sh\nprintf '1.17.12\\n'\n")
	unknown := fakeVersionBinary(t, "#!/bin/sh\nprintf 'dev\\n'\n")
	failed := fakeVersionBinary(t, "#!/bin/sh\nexit 1\n")
	cases := []struct {
		name    string
		inner   []string
		harness string
		native  bool
		deny    []string
		want    bool
		blocked bool
	}{
		{"v2.0", []string{v20}, "opencode", true, nil, true, false},
		{"v2.1", []string{v21}, "opencode", true, nil, false, false},
		{"v1", []string{v1}, "opencode", true, nil, false, false},
		{"unknown", []string{unknown}, "opencode", true, nil, false, false},
		{"failed version", []string{failed}, "opencode", true, nil, false, false},
		{"missing", []string{"omac-missing-opencode"}, "opencode", true, nil, false, false},
		{"npm v2.0", []string{"npx", "-y", "@opencode/cli@2.0.20"}, "opencode", true, nil, true, false},
		{"npm v2.1", []string{"npx", "-y", "@opencode/cli@2.1.2"}, "opencode", true, nil, false, false},
		{"npm v2 major only", []string{"npx", "-y", "@opencode/cli@2"}, "opencode", true, nil, true, false},
		{"env npm v2", []string{"env", "FOO=1", "npx", "--package=@opencode/cli@2.0.20", "opencode"}, "opencode", true, nil, true, false},
		{"legacy wrapper v1", []string{"bunx", "opencode-ai@1.2.3"}, "opencode", true, nil, false, false},
		{"unversioned wrapper", []string{"bunx", "@opencode/cli"}, "opencode", true, nil, false, false},
		{"similar package", []string{"npx", "my-opencode-ai@2.0.20"}, "opencode", true, nil, false, false},
		{"package suffix", []string{"npx", "@opencode/cli@2oops"}, "opencode", true, nil, false, false},
		{"other executable", []string{"other", "@opencode/cli@2.0.20"}, "opencode", true, nil, false, false},
		{"other harness", []string{v20}, "claude-code", true, nil, false, false},
		{"external backend", []string{v20}, "opencode", false, nil, false, false},
		{"deny exact", []string{v20}, "opencode", true, []string{"OPENCODE_CONFIG_PROJECT_DISABLE"}, false, true},
		{"deny prefix", []string{v20}, "opencode", true, []string{"OPENCODE_*"}, false, true},
		{"deny all", []string{v20}, "opencode", true, []string{"*"}, false, true},
		{"v2.1 denied", []string{v21}, "opencode", true, []string{"OPENCODE_*"}, false, false},
		{"v1 denied", []string{v1}, "opencode", true, []string{"OPENCODE_*"}, false, false},
	}
	for _, caller := range []string{"start", "serve"} {
		for _, tc := range cases {
			t.Run(caller+"/"+tc.name, func(t *testing.T) {
				profile := config.DefaultLauncherConfig().Sandbox.Profiles["builtin"]
				in := sandbox.Inputs{Workdir: t.TempDir(), TCPPort: 61500, InnerCmd: tc.inner}
				var argv []string
				var err error
				if caller == "serve" {
					argv, err = sandboxServeArgv(profile, in, "", h)
				} else {
					argv, err = sandbox.Expand(profile, in)
				}
				if err != nil {
					t.Fatal(err)
				}
				policy := &sandboxprofile.Profile{Environment: sandboxprofile.Environment{AllowVars: []string{"PATH"}, DenyVars: tc.deny}}
				plan := sandboxPlan{Native: tc.native, PolicyRef: "restricted", Policy: policy}
				harness, _ := config.LookupHarness(tc.harness)
				extra := map[string]string{"TMPDIR": "/tmp/keep"}
				argv, _, err = injectOpenCodeProjectDiscovery(argv, extra, harness, tc.inner, plan)
				blocked := tc.blocked && runtime.GOOS == "darwin"
				if blocked {
					if err == nil || !strings.Contains(err.Error(), "deny_vars") || !strings.Contains(err.Error(), "OPENCODE_CONFIG_PROJECT_DISABLE") || !strings.Contains(err.Error(), "restricted") {
						t.Fatalf("expected actionable policy denial, got %v", err)
					}
					if extra["OPENCODE_CONFIG_PROJECT_DISABLE"] != "" {
						t.Fatal("denied launch injected workaround")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				flags, err := sandboxprofile.ParseFlags(argv[3:])
				if err != nil {
					t.Fatal(err)
				}
				merged, _ := sandboxprofile.Merge(policy, flags)
				ambient := []string{"PATH=/usr/bin", "AMBIENT_SECRET=hidden"}
				for k, v := range extra {
					ambient = append(ambient, k+"="+v)
				}
				filtered := sandboxprofile.FilterEnv(ambient, sandboxprofile.EffectiveAllowVars(merged.Environment.AllowVars), merged.Environment.DenyVars, nil)
				got := parseEnvironment(strings.Join(filtered, "\n"))
				want := ""
				if tc.want && runtime.GOOS == "darwin" {
					want = "1"
				}
				if got["OPENCODE_CONFIG_PROJECT_DISABLE"] != want {
					t.Errorf("filtered discovery disable = %q, want %q", got["OPENCODE_CONFIG_PROJECT_DISABLE"], want)
				}
				if got["AMBIENT_SECRET"] != "" || (got["TMPDIR"] != "/tmp/keep" && !sandboxprofile.EnvVarMatches("TMPDIR", tc.deny)) {
					t.Errorf("unrelated environment changed: %v", got)
				}
			})
		}
	}
}

func TestClassifyOpenCode(t *testing.T) {
	v20 := fakeVersionBinary(t, "#!/bin/sh\nprintf '2.0.32\\n'\n")
	v21 := fakeVersionBinary(t, "#!/bin/sh\nprintf '2.1.0\\n'\n")
	v1 := fakeVersionBinary(t, "#!/bin/sh\nprintf '1.18.32\\n'\n")
	unknown := fakeVersionBinary(t, "#!/bin/sh\nprintf 'dev\\n'\n")
	cases := []struct {
		name  string
		inner []string
		want  openCodeVersionKind
	}{
		{"2.0 direct", []string{v20}, openCodeV20x},
		{"2.1 direct", []string{v21}, openCodeV21Plus},
		{"v1 direct", []string{v1}, openCodeV1},
		{"unknown direct", []string{unknown}, openCodeUndecidable},
		{"missing binary", []string{"omac-missing-opencode"}, openCodeOther},
		{"env-wrapped 2.0", []string{"env", "FOO=1", v20, "run"}, openCodeV20x},
		{"env-wrapped 2.1", []string{"env", "FOO=1", v21, "run"}, openCodeV21Plus},
		{"2.0 spec", []string{"npx", "-y", "@opencode/cli@2.0.32"}, openCodeV20x},
		{"2.1 spec", []string{"npx", "-y", "@opencode/cli@2.1.0"}, openCodeV21Plus},
		{"2.0 minor spec", []string{"bunx", "opencode-ai@2.0"}, openCodeV20x},
		{"2.1 minor spec", []string{"bunx", "opencode-ai@2.1"}, openCodeV21Plus},
		{"2.1 pre-release spec", []string{"npx", "-y", "@opencode/cli@2.1-rc"}, openCodeV21Plus},
		{"2.0 pre-release spec", []string{"bunx", "opencode-ai@2.0-rc"}, openCodeV20x},
		{"2.0 patch pre-release spec", []string{"bunx", "opencode-ai@2.0.0-rc.1"}, openCodeV20x},
		{"2.1 build metadata spec", []string{"bunx", "opencode-ai@2.1.4+build.7"}, openCodeV21Plus},
		{"major-only spec stays conservative", []string{"bunx", "opencode-ai@2"}, openCodeV2MajorOnly},
		{"major-only pre-release spec stays conservative", []string{"bunx", "opencode-ai@2-rc"}, openCodeUndecidable},
		{"unversioned wrapper", []string{"bunx", "opencode-ai"}, openCodeUndecidable},
		{"similar package", []string{"npx", "my-opencode-ai@2.0.32"}, openCodeUndecidable},
		{"v1 spec", []string{"bunx", "opencode-ai@1.2.3"}, openCodeV1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOpenCode(tc.inner); got != tc.want {
				t.Errorf("classifyOpenCode(%v) = %v, want %v", tc.inner, got, tc.want)
			}
		})
	}
}

func TestOpenCodeProjectDiscoveryEnvOverride(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS discovery workaround")
	}
	h, _ := config.LookupHarness("opencode")
	bin := fakeVersionBinary(t, "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '2.0.20\\n'; else printf '%s' \"$OPENCODE_CONFIG_PROJECT_DISABLE\"; fi\n")
	v21 := fakeVersionBinary(t, "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '2.1.2\\n'; else printf '%s' \"$OPENCODE_CONFIG_PROJECT_DISABLE\"; fi\n")
	v1 := fakeVersionBinary(t, "#!/bin/sh\nprintf '1.17.12\\n'\n")
	unknown := fakeVersionBinary(t, "#!/bin/sh\nprintf 'dev\\n'\n")
	cases := []struct {
		name      string
		inner     []string
		deny      []string
		wantError string
		wantValue string
	}{
		{"zero", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", bin, "serve"}, nil, "env", ""},
		{"empty", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=", bin, "serve"}, nil, "env", ""},
		{"repeated", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", "OPENCODE_CONFIG_PROJECT_DISABLE=1", bin}, nil, "env", ""},
		{"nested", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=1", "/usr/bin/env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", bin}, nil, "env", ""},
		{"one", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=1", bin, "serve"}, nil, "", "1"},
		{"nested one", []string{"env", "FOO=1", "env", "OPENCODE_CONFIG_PROJECT_DISABLE=1", bin}, nil, "", "1"},
		{"ignore environment", []string{"env", "-i", bin}, nil, "env", ""},
		{"unset option", []string{"env", "--unset=OPENCODE_CONFIG_PROJECT_DISABLE", bin}, nil, "env", ""},
		{"deny first", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", bin}, []string{"OPENCODE_CONFIG_PROJECT_DISABLE"}, "deny_vars", ""},
		{"v2.1", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", v21}, nil, "", ""},
		{"deny v2.1", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", v21}, []string{"OPENCODE_CONFIG_PROJECT_DISABLE"}, "", ""},
		{"v1", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", v1}, nil, "", ""},
		{"unknown", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", unknown}, nil, "", ""},
		{"unset separate operand unknown", []string{"env", "-u", "OPENCODE_CONFIG_PROJECT_DISABLE", bin}, nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := sandboxPlan{Native: true, PolicyRef: "restricted", Policy: &sandboxprofile.Profile{Environment: sandboxprofile.Environment{DenyVars: tc.deny}}}
			extra := map[string]string{}
			_, _, err := injectOpenCodeProjectDiscovery([]string{"omac", "sandbox", "run", "--", "opencode"}, extra, h, tc.inner, plan)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || !strings.Contains(err.Error(), "OPENCODE_CONFIG_PROJECT_DISABLE") {
					t.Fatalf("expected %s conflict, got %v", tc.wantError, err)
				}
				if len(extra) != 0 {
					t.Fatalf("conflict injected environment: %v", extra)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if extra["OPENCODE_CONFIG_PROJECT_DISABLE"] != tc.wantValue {
				t.Fatalf("injection=%v, want value %q", extra, tc.wantValue)
			}
			if tc.wantValue == "1" {
				cmd := exec.Command(tc.inner[0], tc.inner[1:]...)
				cmd.Env = []string{"PATH=/usr/bin:/bin", "OPENCODE_CONFIG_PROJECT_DISABLE=1"}
				out, err := cmd.CombinedOutput()
				if err != nil || string(out) != "1" {
					t.Fatalf("after inner env wrapper: %q, error=%v", out, err)
				}
			}
		})
	}
}

func TestOpenCodeProjectDiscoveryErrorsNameTheVersion(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS discovery workaround")
	}
	h, _ := config.LookupHarness("opencode")
	v20 := fakeVersionBinary(t, "#!/bin/sh\nprintf '2.0.20\\n'\n")
	denied := &sandboxprofile.Profile{Environment: sandboxprofile.Environment{DenyVars: []string{"OPENCODE_CONFIG_PROJECT_DISABLE"}}}
	open := &sandboxprofile.Profile{}
	cases := []struct {
		name     string
		inner    []string
		policy   *sandboxprofile.Profile
		wantHint bool
	}{
		{"major-only deny_vars hints", []string{"bunx", "opencode-ai@2"}, denied, true},
		{"major-only conflicting assignment hints", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", "bunx", "opencode-ai@2"}, open, true},
		{"major-only env options hint", []string{"env", "-i", "bunx", "opencode-ai@2"}, open, true},
		{"2.0.x deny_vars stays specific", []string{v20}, denied, false},
		{"2.0.x conflicting assignment stays specific", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", v20}, open, false},
		{"2.0.x env options stay specific", []string{"env", "-i", v20}, open, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := sandboxPlan{Native: true, PolicyRef: "restricted", Policy: tc.policy}
			_, class, err := injectOpenCodeProjectDiscovery([]string{"omac", "sandbox", "run", "--", "opencode"}, map[string]string{}, h, tc.inner, plan)
			if err == nil {
				t.Fatal("expected a launch error")
			}
			if !strings.Contains(err.Error(), "OPENCODE_CONFIG_PROJECT_DISABLE") {
				t.Fatalf("error does not name the variable: %v", err)
			}
			hinted := strings.Contains(err.Error(), "pin @2.1 or newer")
			if hinted != tc.wantHint {
				t.Errorf("hint present = %v, want %v: %v", hinted, tc.wantHint, err)
			}
			if hinted && class != openCodeV2MajorOnly {
				t.Errorf("hint shown for class %v, want openCodeV2MajorOnly", class)
			}
			if !hinted && !strings.Contains(err.Error(), "OpenCode 2.0.x") {
				t.Errorf("error does not name OpenCode 2.0.x: %v", err)
			}
		})
	}
}

func TestOpenCodeProjectDiscoveryDeniedLaunch(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS discovery workaround")
	}
	omac := filepath.Join(t.TempDir(), "omac")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", omac, "../../cmd/omac")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
		wantEnv  string
		noInner  bool
	}{
		{"start denied", []string{"start"}, ExitConfigInvalid, "", false},
		{"serve denied", []string{"serve"}, ExitConfigInvalid, "", false},
		{"start no-sandbox", []string{"start", "--no-sandbox"}, ExitOK, "host-value", false},
		{"serve no-sandbox", []string{"serve", "--no-sandbox"}, ExitOK, "host-value", false},
		{"serve learn", []string{"serve", "--learn"}, ExitOK, "unset", false},
		{"serve no-inner", []string{"serve", "--no-inner"}, ExitOK, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHome(t)
			root := t.TempDir()
			capture := filepath.Join(root, "discovery-env")
			t.Setenv("OPENCODE_CONFIG_PROJECT_DISABLE", "host-value")
			t.Setenv("OMAC_TEST_DISCOVERY_CAPTURE", capture)
			dir, err := sandboxprofile.ProfileDir()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "default.json"), []byte(`{"environment":{"allow_vars":["PATH"],"deny_vars":["OPENCODE_CONFIG_PROJECT_DISABLE"]}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := fakeVersionBinary(t, "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '2.0.20\\n'; else printf '%s' \"${OPENCODE_CONFIG_PROJECT_DISABLE-unset}\" > \"$OMAC_TEST_DISCOVERY_CAPTURE\"; fi\n")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			args := append(append([]string{}, tc.args...), "--inner", bin, "--no-audit")
			cmd := exec.CommandContext(ctx, omac, args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "TMPDIR=.")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
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
			if tc.noInner {
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
						t.Fatalf("control-plane-only exited before readiness: %v\n%s", err, stderr.String())
					case <-ctx.Done():
						t.Fatal("control-plane-only readiness timed out")
					case <-time.After(10 * time.Millisecond):
					}
				}
				if err := cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Fatal(err)
				}
				<-done
				if _, err := os.Stat(capture); !os.IsNotExist(err) {
					t.Fatalf("no-inner ran fake command: %v", err)
				}
				return
			}
			err = <-done
			if ctx.Err() != nil {
				t.Fatalf("launch timed out: %v", ctx.Err())
			}
			code := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatal(err)
				}
				code = exitErr.ExitCode()
			}
			if code != tc.wantCode {
				if tc.name == "serve learn" && strings.Contains(stderr.String(), "sandbox_apply: Operation not permitted") {
					t.Skip("learn reached native execution, but nested Seatbelt blocks the fake's environment capture")
				}
				t.Fatalf("launch exit=%d, want %d; stderr:\n%s", code, tc.wantCode, stderr.String())
			}
			if tc.wantCode == ExitConfigInvalid {
				if !strings.Contains(stderr.String(), "opencode project discovery:") || !strings.Contains(stderr.String(), "deny_vars denies OPENCODE_CONFIG_PROJECT_DISABLE") {
					t.Fatalf("wrong launch denial: %s", stderr.String())
				}
				return
			}
			out, err := os.ReadFile(capture)
			if err != nil || string(out) != tc.wantEnv {
				t.Fatalf("fake discovery environment=%q, want %q, read error=%v", out, tc.wantEnv, err)
			}
		})
	}
}

// runCapturingStderr redirects env.Stderr into a temp file and returns what fn wrote.
func runCapturingStderr(t *testing.T, env *Env, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stderr")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	env.Stderr = f
	fn()
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestWarnOpenCodeAgentsAccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A temp home sits inside granted baseline trees (for example /var/folders
	// on macOS), so the warning is only observable when ~/.agents is denied
	// outright, and its absence when the profile grants ~/.agents directly.
	profile := func(fs sandboxprofile.Filesystem) *sandboxprofile.Profile {
		return &sandboxprofile.Profile{Environment: sandboxprofile.Environment{AllowVars: []string{"PATH"}}, Filesystem: fs}
	}
	deniedPlan := sandboxPlan{Native: true, PolicyRef: "restricted", Policy: profile(sandboxprofile.Filesystem{Deny: []string{"~/.agents"}})}
	grantedPlan := sandboxPlan{Native: true, PolicyRef: "restricted", Policy: profile(sandboxprofile.Filesystem{Read: []string{"~/.agents"}})}
	argv := []string{"omac", "sandbox", "run", "--", "opencode"}
	cases := []struct {
		name  string
		plan  sandboxPlan
		class openCodeVersionKind
		want  string
	}{
		{"2.0.x", deniedPlan, openCodeV20x, "may fail with EPERM"},
		{"major-only", deniedPlan, openCodeV2MajorOnly, "may fail with EPERM"},
		{"2.1 plus", deniedPlan, openCodeV21Plus, "silently do not load"},
		{"denied adds hint", deniedPlan, openCodeV20x, "filesystem deny also blocks"},
		{"granted stays silent", grantedPlan, openCodeV20x, ""},
		{"v1", deniedPlan, openCodeV1, ""},
		{"undecidable", deniedPlan, openCodeUndecidable, ""},
		{"other", deniedPlan, openCodeOther, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &Env{Workdir: t.TempDir()}
			out := runCapturingStderr(t, env, func() {
				warnOpenCodeAgentsAccess(env, argv, tc.plan, tc.class)
			})
			if tc.want == "" {
				if out != "" {
					t.Errorf("expected no warning, got %q", out)
				}
				return
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("warning %q does not mention %q", out, tc.want)
			}
		})
	}
}

func TestWarnOpenCodeMajorOnlySpec(t *testing.T) {
	cases := []struct {
		name  string
		class openCodeVersionKind
		warns bool
	}{
		{"major-only warns", openCodeV2MajorOnly, true},
		{"2.0.x silent", openCodeV20x, false},
		{"2.1 plus silent", openCodeV21Plus, false},
		{"undecidable silent", openCodeUndecidable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &Env{}
			out := runCapturingStderr(t, env, func() {
				warnOpenCodeMajorOnlySpec(env, tc.class)
			})
			if tc.warns != (out != "") {
				t.Errorf("warning output %q, want warns=%v", out, tc.warns)
			}
			if tc.warns && !strings.Contains(out, "Pin @2.1 or newer") {
				t.Errorf("warning %q does not mention the pin hint", out)
			}
		})
	}
}
