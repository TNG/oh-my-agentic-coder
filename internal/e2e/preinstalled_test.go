//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

func TestPreinstalledEnabled(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"1", true},
		{"true", false},
	}
	for _, tc := range cases {
		t.Setenv(preinstalledEnvVar, tc.value)
		if got := preinstalledEnabled(); got != tc.want {
			t.Errorf("%s=%q: got %v, want %v", preinstalledEnvVar, tc.value, got, tc.want)
		}
	}
}

func TestCheckPreinstalledVersion(t *testing.T) {
	cases := []struct {
		name     string
		spec     string
		reported string
		wantErr  bool
	}{
		{"plain match", "opencode-ai@1.18.29", "1.18.29\n", false},
		{"scoped package match", "@anthropic-ai/claude-code@2.1.197", "2.1.197 (Claude Code)\n", false},
		{"v prefix", "codex@0.142.5", "codex-cli v0.142.5\n", false},
		{"trailing period", "@github/copilot@1.0.68", "copilot 1.0.68.\n", false},
		{"mismatch", "opencode-ai@1.18.29", "1.18.30\n", true},
		{"unparseable output", "opencode-ai@1.18.29", "garbage\n", true},
		{"latest has no version to compare", "opencode-ai", "9.9.9\n", false},
		{"scoped latest has no version to compare", "@anthropic-ai/claude-code", "9.9.9\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPreinstalledVersion("h", tc.spec, tc.reported)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestCheckPreinstalledVersionNamesBothVersions(t *testing.T) {
	err := checkPreinstalledVersion("opencode", "opencode-ai@1.18.29", "1.18.30\n")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"opencode", "1.18.29", "1.18.30"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
