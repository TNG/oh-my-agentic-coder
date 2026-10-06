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
	v2 := fakeVersionBinary(t, "#!/bin/sh\nprintf '2.0.20\\n'\n")
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
		{"v2", []string{v2}, "opencode", true, nil, true, false},
		{"v1", []string{v1}, "opencode", true, nil, false, false},
		{"unknown", []string{unknown}, "opencode", true, nil, false, false},
		{"failed version", []string{failed}, "opencode", true, nil, false, false},
		{"missing", []string{"omac-missing-opencode"}, "opencode", true, nil, false, false},
		{"npm v2", []string{"npx", "-y", "@opencode/cli@2.0.20"}, "opencode", true, nil, true, false},
		{"env npm v2", []string{"env", "FOO=1", "npx", "--package=@opencode/cli@2.0.20", "opencode"}, "opencode", true, nil, true, false},
		{"legacy wrapper v1", []string{"bunx", "opencode-ai@1.2.3"}, "opencode", true, nil, false, false},
		{"unversioned wrapper", []string{"bunx", "@opencode/cli"}, "opencode", true, nil, false, false},
		{"similar package", []string{"npx", "my-opencode-ai@2.0.20"}, "opencode", true, nil, false, false},
		{"package suffix", []string{"npx", "@opencode/cli@2oops"}, "opencode", true, nil, false, false},
		{"other executable", []string{"other", "@opencode/cli@2.0.20"}, "opencode", true, nil, false, false},
		{"other harness", []string{v2}, "claude-code", true, nil, false, false},
		{"external backend", []string{v2}, "opencode", false, nil, false, false},
		{"deny exact", []string{v2}, "opencode", true, []string{"OPENCODE_CONFIG_PROJECT_DISABLE"}, false, true},
		{"deny prefix", []string{v2}, "opencode", true, []string{"OPENCODE_*"}, false, true},
		{"deny all", []string{v2}, "opencode", true, []string{"*"}, false, true},
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
				argv, err = injectOpenCodeProjectDiscovery(argv, extra, harness, tc.inner, plan)
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

func TestOpenCodeProjectDiscoveryEnvOverride(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS discovery workaround")
	}
	h, _ := config.LookupHarness("opencode")
	bin := fakeVersionBinary(t, "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '2.0.20\\n'; else printf '%s' \"$OPENCODE_CONFIG_PROJECT_DISABLE\"; fi\n")
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
		{"v1", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", v1}, nil, "", ""},
		{"unknown", []string{"env", "OPENCODE_CONFIG_PROJECT_DISABLE=0", unknown}, nil, "", ""},
		{"unset separate operand unknown", []string{"env", "-u", "OPENCODE_CONFIG_PROJECT_DISABLE", bin}, nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := sandboxPlan{Native: true, PolicyRef: "restricted", Policy: &sandboxprofile.Profile{Environment: sandboxprofile.Environment{DenyVars: tc.deny}}}
			extra := map[string]string{}
			_, err := injectOpenCodeProjectDiscovery([]string{"omac", "sandbox", "run", "--", "opencode"}, extra, h, tc.inner, plan)
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
