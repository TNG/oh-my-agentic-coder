package config

import (
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

	// Removed or unknown backends are rejected with a migration hint.
	for _, p := range []string{"nono", "nono-netprofile", "no-sandbox-debug", "custom"} {
		err := validateSandbox(SandboxConfig{DefaultProfile: p}, "cfg")
		if err == nil {
			t.Errorf("default_profile %q should be rejected", p)
			continue
		}
		if !strings.Contains(err.Error(), "builtin") {
			t.Errorf("error for %q should mention builtin: %v", p, err)
		}
	}

	// Custom launcher profiles are no longer supported.
	err := validateSandbox(SandboxConfig{
		Profiles: map[string]SandboxProfile{"x": {}},
	}, "cfg")
	if err == nil {
		t.Error("a non-empty profiles block should be rejected")
	}
}

