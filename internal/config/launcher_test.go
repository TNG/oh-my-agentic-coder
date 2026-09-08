package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSandboxBriefingParsesFromYAML(t *testing.T) {
	const in = `
sandbox:
  default_profile: tng-default
  briefing: |
    custom sandbox note
`
	var lc LauncherConfig
	if err := yaml.Unmarshal([]byte(in), &lc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := "custom sandbox note\n"
	if lc.Sandbox.Briefing != want {
		t.Errorf("Sandbox.Briefing = %q; want %q", lc.Sandbox.Briefing, want)
	}
}

func TestSandboxBriefingEmptyWhenAbsent(t *testing.T) {
	var lc LauncherConfig
	if err := yaml.Unmarshal([]byte("sandbox:\n  default_profile: builtin\n"), &lc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if lc.Sandbox.Briefing != "" {
		t.Errorf("Sandbox.Briefing = %q; want empty", lc.Sandbox.Briefing)
	}
}

func TestValidateSandbox(t *testing.T) {
	// Valid: the built-in sandbox (or unset, which defaults to it).
	for _, p := range []string{"", "builtin"} {
		if err := validateSandbox(SandboxConfig{DefaultProfile: p}, "cfg"); err != nil {
			t.Errorf("default_profile %q should be valid: %v", p, err)
		}
	}

	// Removed external backends and unknown names are rejected, pointing the
	// user at the built-in sandbox.
	for _, p := range []string{"nono", "nono-netprofile", "custom"} {
		err := validateSandbox(SandboxConfig{DefaultProfile: p}, "cfg")
		if err == nil {
			t.Errorf("default_profile %q should be rejected", p)
			continue
		}
		if !strings.Contains(err.Error(), "builtin") {
			t.Errorf("error for %q should mention builtin: %v", p, err)
		}
	}

	// no-sandbox-debug is rejected too, but its fix is the CLI flag, not builtin.
	err := validateSandbox(SandboxConfig{DefaultProfile: "no-sandbox-debug"}, "cfg")
	if err == nil {
		t.Error("default_profile \"no-sandbox-debug\" should be rejected")
	} else if !strings.Contains(err.Error(), "--no-sandbox") {
		t.Errorf("error for no-sandbox-debug should point at --no-sandbox: %v", err)
	}

	// Custom launcher profiles are no longer supported.
	err = validateSandbox(SandboxConfig{
		Profiles: map[string]any{"x": nil},
	}, "cfg")
	if err == nil {
		t.Error("a non-empty profiles block should be rejected")
	}
}

func TestSandboxProfilePathParsesFromYAML(t *testing.T) {
	const in = "sandbox:\n  profile_path: ./sandbox.json\n"
	var lc LauncherConfig
	if err := yaml.Unmarshal([]byte(in), &lc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if lc.Sandbox.ProfilePath != "./sandbox.json" {
		t.Errorf("Sandbox.ProfilePath = %q; want %q", lc.Sandbox.ProfilePath, "./sandbox.json")
	}
}

func TestResolveSandboxProfileRefUnset(t *testing.T) {
	var lc LauncherConfig
	ref, err := lc.ResolveSandboxProfileRef("/whatever/config.yaml", "/work")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref != "" {
		t.Errorf("unset profile_path should resolve to %q, got %q", "", ref)
	}
}

func TestResolveSandboxProfileRefAbsolute(t *testing.T) {
	dir := t.TempDir()
	prof := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(prof, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	lc := LauncherConfig{Sandbox: SandboxConfig{ProfilePath: prof}}
	// cfgPath/workdir are irrelevant for an absolute path.
	ref, err := lc.ResolveSandboxProfileRef("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref != prof {
		t.Errorf("ResolveSandboxProfileRef = %q; want %q", ref, prof)
	}
}

func TestResolveSandboxProfileRefMissingIsHardError(t *testing.T) {
	lc := LauncherConfig{Sandbox: SandboxConfig{ProfilePath: "/does/not/exist.json"}}
	_, err := lc.ResolveSandboxProfileRef("/whatever/config.yaml", "/work")
	if err == nil {
		t.Fatal("a missing profile_path should be a hard error")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should mention the file is missing: %v", err)
	}
}

func TestResolveSandboxProfileRefDirectoryIsError(t *testing.T) {
	dir := t.TempDir()
	lc := LauncherConfig{Sandbox: SandboxConfig{ProfilePath: dir}}
	_, err := lc.ResolveSandboxProfileRef("", "")
	if err == nil {
		t.Fatal("a directory profile_path should be an error")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error should mention it is a directory: %v", err)
	}
}

func TestResolveSandboxProfileRefProjectRelative(t *testing.T) {
	workdir := t.TempDir()
	// The project layer anchors on the project root (workdir), NOT the config
	// dir (<workdir>/.opencode) — so the file lives at <workdir>/sandbox.json.
	prof := filepath.Join(workdir, "sandbox.json")
	if err := os.WriteFile(prof, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := ProjectLauncherConfigPath(workdir)
	lc := LauncherConfig{Sandbox: SandboxConfig{ProfilePath: "./sandbox.json"}}
	ref, err := lc.ResolveSandboxProfileRef(cfgPath, workdir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref != prof {
		t.Errorf("project-relative ref = %q; want %q", ref, prof)
	}
}

func TestResolveSandboxProfileRefGlobalRelative(t *testing.T) {
	// A global-layer config anchors a relative path on the config directory.
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.yaml")
	prof := filepath.Join(cfgDir, "sandbox.json")
	if err := os.WriteFile(prof, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A workdir is supplied, but cfgPath is NOT that workdir's project config,
	// so the global (config-dir) anchor is used.
	lc := LauncherConfig{Sandbox: SandboxConfig{ProfilePath: "sandbox.json"}}
	ref, err := lc.ResolveSandboxProfileRef(cfgPath, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref != prof {
		t.Errorf("global-relative ref = %q; want %q", ref, prof)
	}
}

func TestResolveSandboxProfileRefRelativeWithoutCfgPath(t *testing.T) {
	// A relative profile_path with no cfgPath is a caller bug and must error.
	lc := LauncherConfig{Sandbox: SandboxConfig{ProfilePath: "./sandbox.json"}}
	_, err := lc.ResolveSandboxProfileRef("", "/work")
	if err == nil {
		t.Fatal("a relative profile_path with no cfgPath should error")
	}
}

func TestSandboxDeprecationWarnings(t *testing.T) {
	if w := (SandboxConfig{}).DeprecationWarnings(); len(w) != 0 {
		t.Errorf("empty sandbox config should have no warnings, got %v", w)
	}
	w := SandboxConfig{DefaultProfile: "builtin"}.DeprecationWarnings()
	if len(w) != 1 || !strings.Contains(w[0], "default_profile") {
		t.Errorf("a set default_profile should warn about default_profile, got %v", w)
	}
}
