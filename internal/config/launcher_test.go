package config

import (
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
