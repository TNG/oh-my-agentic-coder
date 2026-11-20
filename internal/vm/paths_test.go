package vm

import (
	"path/filepath"
	"testing"
)

func TestDefaultRootHonorsXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/x/state")
	got, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/x/state", "omac", "vm"); got != want {
		t.Errorf("DefaultRoot() = %q, want %q", got, want)
	}
}

func TestDefaultRootFallsBackToLocalState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	got, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/home/u", ".local", "state", "omac", "vm"); got != want {
		t.Errorf("DefaultRoot() = %q, want %q", got, want)
	}
}

func TestNewLayoutPaths(t *testing.T) {
	l := NewLayout("/r")
	cases := map[string]string{
		l.LimaHome: "/r/lima", l.AdminKey: "/r/admin_key", l.AgentKey: "/r/agent_key",
		l.StatePath: "/r/state.json", l.KnownHosts: "/r/known_hosts",
		l.SessionsDir: "/r/sessions", l.LimaYAML: "/r/omac.yaml",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("layout path = %q, want %q", got, want)
		}
	}
}

func TestUserConfigPathUsesXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	if got, want := UserConfigPath(), filepath.Join("/x/cfg", "omac", "vm.yaml"); got != want {
		t.Errorf("UserConfigPath() = %q, want %q", got, want)
	}
}
