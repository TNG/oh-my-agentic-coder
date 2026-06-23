package config

import (
	"path/filepath"
	"testing"
)

func TestGlobalSkillsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "") // force the ~/.config fallback

	oc, ok := LookupHarness("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}
	cc, ok := LookupHarness("claude")
	if !ok {
		t.Fatal("claude harness not found")
	}

	if got, want := oc.GlobalSkillsDir(), filepath.Join(home, ".config", "opencode", "skills"); got != want {
		t.Fatalf("opencode GlobalSkillsDir = %q, want %q", got, want)
	}
	if got, want := cc.GlobalSkillsDir(), filepath.Join(home, ".claude", "skills"); got != want {
		t.Fatalf("claude GlobalSkillsDir = %q, want %q", got, want)
	}

	// OpenCode honors $XDG_CONFIG_HOME; Claude Code does not (uses ~/.claude).
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got, want := oc.GlobalSkillsDir(), filepath.Join(xdg, "opencode", "skills"); got != want {
		t.Fatalf("opencode GlobalSkillsDir (XDG) = %q, want %q", got, want)
	}
	if got, want := cc.GlobalSkillsDir(), filepath.Join(home, ".claude", "skills"); got != want {
		t.Fatalf("claude GlobalSkillsDir (XDG set) = %q, want %q", got, want)
	}
}
