package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// fakeVersionBinary writes an executable sh script that prints script as its --version output.
func fakeVersionBinary(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a sh script")
	}
	bin := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestOpenCodeIsV2(t *testing.T) {
	cases := []struct {
		name   string
		script string
		want   bool
	}{
		{"v2 plain", "#!/bin/sh\necho 2.0.18\n", true},
		{"v2 tagged", "#!/bin/sh\necho v2.1.0\n", true},
		{"v1 plain", "#!/bin/sh\necho 1.18.32\n", false},
		{"v1 npm triple", "#!/bin/sh\necho opencode-cli/1.17.12 linux-x64 node-v20.0.0\n", false},
		{"failing --version treated as current", "#!/bin/sh\nexit 1\n", true},
		{"empty output treated as current", "#!/bin/sh\nprintf ''\n", true},
		{"no semver treated as current", "#!/bin/sh\necho dev\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeVersionBinary(t, tc.script)
			if got := openCodeIsV2([]string{bin}); got != tc.want {
				t.Errorf("openCodeIsV2(%s) = %v, want %v", tc.script, got, tc.want)
			}
		})
	}
}

// An unresolvable inner is checkInnerBinary's job; detection must not panic or treat it as v2.
func TestOpenCodeIsV2SkipsMissingBinary(t *testing.T) {
	if openCodeIsV2([]string{"omac-definitely-not-on-path-xyz"}) {
		t.Error("missing binary must not be treated as v2")
	}
	if openCodeIsV2(nil) || openCodeIsV2([]string{}) {
		t.Error("empty inner must not be treated as v2")
	}
}

// Core of the fix: a v2 inner gets a free port, a symlink-seeded config dir, and a private state dir.
func TestPinOpenCodeV2ServiceSeedsConfig(t *testing.T) {
	h, ok := config.LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}

	// ConfigHome() resolves $XDG_CONFIG_HOME/opencode, so the test owns it.
	cfgRoot := t.TempDir()
	realCfg := filepath.Join(cfgRoot, "opencode")
	if err := os.MkdirAll(filepath.Join(realCfg, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realCfg, "opencode.json"), []byte(`{"model":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realCfg, "service.json"), []byte(`{"port":49374,"password":"hostpw"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfgRoot)

	sandboxTmp := t.TempDir()
	inner := []string{fakeVersionBinary(t, "#!/bin/sh\necho 2.0.18\n")}

	pin, err := pinOpenCodeV2Service(h, inner, sandboxTmp)
	if err != nil {
		t.Fatalf("pinOpenCodeV2Service: %v", err)
	}
	if pin == nil {
		t.Fatal("v2 inner command produced no pin")
	}
	if pin.Port < 1 || pin.Port > 65535 {
		t.Errorf("port out of range: %d", pin.Port)
	}
	if want := filepath.Join(sandboxTmp, "opencode-config"); pin.CfgDir != want {
		t.Errorf("CfgDir = %q, want %q", pin.CfgDir, want)
	}
	if want := filepath.Join(sandboxTmp, "opencode-state"); pin.StateDir != want {
		t.Errorf("StateDir = %q, want %q", pin.StateDir, want)
	}

	// Only the pinned port: the host's password and port must not leak into the sandboxed service identity.
	raw, err := os.ReadFile(filepath.Join(pin.CfgDir, "service.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("service.json %q: %v", raw, err)
	}
	if len(cfg) != 1 {
		t.Errorf("service.json = %v, want exactly the port field", cfg)
	}
	if port, _ := cfg["port"].(float64); int(port) != pin.Port {
		t.Errorf("service.json port = %v, want %d", cfg["port"], pin.Port)
	}
	if st, err := os.Lstat(filepath.Join(pin.CfgDir, "service.json")); err != nil {
		t.Fatal(err)
	} else if st.Mode()&os.ModeSymlink != 0 {
		t.Error("service.json must be the private pin, not a symlink to the host config")
	}

	// User config entries come through as symlinks to the real config home.
	if st, err := os.Lstat(filepath.Join(pin.CfgDir, "opencode.json")); err != nil {
		t.Fatal(err)
	} else if st.Mode()&os.ModeSymlink == 0 {
		t.Error("opencode.json should be a symlink into the real config home")
	}
	if got, err := os.ReadFile(filepath.Join(pin.CfgDir, "opencode.json")); err != nil {
		t.Errorf("reading through symlink: %v", err)
	} else if string(got) != `{"model":"x"}` {
		t.Errorf("symlinked opencode.json = %q", got)
	}
	if st, err := os.Lstat(filepath.Join(pin.CfgDir, "plugins")); err != nil {
		t.Fatal(err)
	} else if st.Mode()&os.ModeSymlink == 0 {
		t.Error("plugins/ should be a symlink into the real config home")
	}

	// The host's own service config stays untouched.
	if host, err := os.ReadFile(filepath.Join(realCfg, "service.json")); err != nil {
		t.Fatal(err)
	} else if string(host) != `{"port":49374,"password":"hostpw"}` {
		t.Errorf("host service.json modified: %s", host)
	}
}

// v1 (the pinned e2e suite) keeps the unchanged path: no pin, no dirs, no extra grants.
func TestPinOpenCodeV2ServiceSkipsV1(t *testing.T) {
	h, _ := config.LookupHarness("opencode")
	sandboxTmp := t.TempDir()
	inner := []string{fakeVersionBinary(t, "#!/bin/sh\necho 1.17.12\n")}

	pin, err := pinOpenCodeV2Service(h, inner, sandboxTmp)
	if err != nil {
		t.Fatalf("pinOpenCodeV2Service: %v", err)
	}
	if pin != nil {
		t.Fatalf("v1 must not be pinned, got %+v", pin)
	}
	if _, err := os.Stat(filepath.Join(sandboxTmp, "opencode-config")); !os.IsNotExist(err) {
		t.Error("v1 launch must not seed a config dir")
	}
}

// The pin is opencode-specific; another harness (even with a v2-looking binary) gets nothing.
func TestPinOpenCodeV2ServiceSkipsOtherHarness(t *testing.T) {
	cc, ok := config.LookupHarness("claude-code")
	if !ok {
		t.Fatal("claude-code harness not found")
	}
	sandboxTmp := t.TempDir()
	inner := []string{fakeVersionBinary(t, "#!/bin/sh\necho 2.0.18\n")}

	pin, err := pinOpenCodeV2Service(cc, inner, sandboxTmp)
	if err != nil {
		t.Fatalf("pinOpenCodeV2Service: %v", err)
	}
	if pin != nil {
		t.Fatalf("non-opencode harness must not be pinned, got %+v", pin)
	}
	if _, err := os.Stat(filepath.Join(sandboxTmp, "opencode-config")); !os.IsNotExist(err) {
		t.Error("non-opencode launch must not seed a config dir")
	}
}

// install splices both flags on native plans (allow-env is load-bearing: FilterEnv strips the env otherwise) and nothing on non-native.
func TestOpenCodeServicePinInstall(t *testing.T) {
	pin := &openCodeServicePin{Port: 59991, CfgDir: "/tmp/t/opencode-config", StateDir: "/tmp/t/opencode-state"}
	in := []string{"omac", "sandbox", "run", "--profile", "default", "--", "opencode"}

	native := sandboxPlan{Native: true, PolicyRef: "default"}
	want := []string{"omac", "sandbox", "run", "--profile", "default",
		"--open-port", "59991", "--allow-env", "OPENCODE_CONFIG_DIR", "--", "opencode"}
	if got := pin.install(in, native); !equalStrings(got, want) {
		t.Errorf("install(native): got %v, want %v", got, want)
	}

	if got := pin.install(in, sandboxPlan{Native: false}); !equalStrings(got, in) {
		t.Errorf("install(non-native) must not splice flags: got %v, want %v", got, in)
	}
}

// apply lands both env keys in the extra map without clobbering unrelated keys.
func TestOpenCodeServicePinApply(t *testing.T) {
	pin := &openCodeServicePin{Port: 59991, CfgDir: "/tmp/t/opencode-config", StateDir: "/tmp/t/opencode-state"}

	extra := map[string]string{"TMPDIR": "/tmp/t"}
	pin.apply(extra)
	if extra["OPENCODE_CONFIG_DIR"] != pin.CfgDir {
		t.Errorf("OPENCODE_CONFIG_DIR = %q, want %q", extra["OPENCODE_CONFIG_DIR"], pin.CfgDir)
	}
	if extra["XDG_STATE_HOME"] != pin.StateDir {
		t.Errorf("XDG_STATE_HOME = %q, want %q", extra["XDG_STATE_HOME"], pin.StateDir)
	}
	if extra["TMPDIR"] != "/tmp/t" {
		t.Errorf("apply must not clobber unrelated keys, TMPDIR = %q", extra["TMPDIR"])
	}
}
