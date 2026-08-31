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

// TestSandboxProfilePolicyRef covers the launcher→policy mapping: a
// launcher profile's argv template is the ONLY place the policy reference
// is spelled, and the two namespaces must never be conflated (#173 — the
// default launcher profile is named "builtin" while its policy is
// "default").
func TestSandboxProfilePolicyRef(t *testing.T) {
	cases := []struct {
		name       string
		command    []string
		wantRef    string
		wantNative bool
	}{
		{
			name:       "separate args",
			command:    []string{"{{self}}", "sandbox", "run", "--profile", "strict", "--", "{{inner_cmd}}"},
			wantRef:    "strict",
			wantNative: true,
		},
		{
			name:       "inline form",
			command:    []string{"{{self}}", "sandbox", "run", "--profile=strict", "--", "{{inner_cmd}}"},
			wantRef:    "strict",
			wantNative: true,
		},
		{
			name:       "omitted profile defaults",
			command:    []string{"{{self}}", "sandbox", "run", "--open-port", "{{tcp_port}}", "--", "{{inner_cmd}}"},
			wantRef:    "default",
			wantNative: true,
		},
		{
			// A --profile appearing AFTER the separator belongs to the inner
			// command, not to `omac sandbox run`.
			name:       "profile after separator is the inner command's",
			command:    []string{"{{self}}", "sandbox", "run", "--", "tool", "--profile", "notmine"},
			wantRef:    "default",
			wantNative: true,
		},
		{
			name:       "external launcher is opaque",
			command:    []string{"nono", "--profile", "tng-sandbox.json", "--", "{{inner_cmd}}"},
			wantRef:    "",
			wantNative: false,
		},
		{
			// The no-sandbox debug profile runs the inner command directly.
			name:       "bare inner command is opaque",
			command:    []string{"{{inner_cmd}}", "{{inner_args}}"},
			wantRef:    "",
			wantNative: false,
		},
		{
			name:       "other sandbox subcommand is opaque",
			command:    []string{"{{self}}", "sandbox", "stage2", "--", "{{inner_cmd}}"},
			wantRef:    "",
			wantNative: false,
		},
		{
			name:       "empty command is opaque",
			command:    nil,
			wantRef:    "",
			wantNative: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, native := SandboxProfile{Command: tc.command}.PolicyRef()
			if ref != tc.wantRef || native != tc.wantNative {
				t.Errorf("PolicyRef() = (%q, %v), want (%q, %v)", ref, native, tc.wantRef, tc.wantNative)
			}
		})
	}
}

// The shipped default launcher profile is the case the bug was about.
func TestDefaultLauncherProfilePolicyRefIsDefault(t *testing.T) {
	prof, ok := DefaultLauncherConfig().Sandbox.Profiles["builtin"]
	if !ok {
		t.Fatal(`the default config must ship a "builtin" launcher profile`)
	}
	ref, native := prof.PolicyRef()
	if !native {
		t.Error("builtin must be recognized as omac's native sandbox")
	}
	if ref != "default" {
		t.Errorf("builtin policy ref = %q, want default (NOT the launcher name)", ref)
	}
}
