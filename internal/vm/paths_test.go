package vm

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"omac", "x", "a-b-1", "abcdefghijklmnop"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "1a", "-a", "A", "a_b", "a.b", "a/b", "..", "abcdefghijklmnopq"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", bad)
		}
	}
}

func TestInstanceRootAndConfigPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/x/state")
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	cases := []struct{ name, root, cfg string }{
		{"omac", "/x/state/omac/vm", "/x/cfg/omac/vm.yaml"},
		{"work", "/x/state/omac/vms/work", "/x/cfg/omac/vms/work.yaml"},
	}
	for _, c := range cases {
		root, err := InstanceRoot(c.name)
		if err != nil || root != filepath.FromSlash(c.root) {
			t.Errorf("InstanceRoot(%q) = %q, %v; want %q", c.name, root, err, c.root)
		}
		if got := InstanceConfigPath(c.name); got != filepath.FromSlash(c.cfg) {
			t.Errorf("InstanceConfigPath(%q) = %q, want %q", c.name, got, c.cfg)
		}
	}
	if _, err := InstanceRoot("../x"); err == nil {
		t.Error("InstanceRoot with a path-like name must fail")
	}
}

func TestListInstances(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	rows, err := ListInstances()
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty state: rows=%v err=%v", rows, err)
	}
	write := func(name string, st *State) {
		t.Helper()
		root, err := InstanceRoot(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if st != nil {
			if err := SaveState(NewLayout(root).StatePath, st); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("omac", &State{SSHPort: 42022, Mode: ModeOffline})
	write("zed", &State{SSHPort: 42023, Mode: ModeOnline})
	write("abc", nil)
	base, _ := stateBase()
	if err := os.MkdirAll(filepath.Join(base, "vms", "Bad_Name"), 0o700); err != nil {
		t.Fatal(err)
	}
	rows, err = ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	want := []InstanceRow{
		{"omac", ModeOffline, 42022},
		{"abc", "not created", 0},
		{"zed", ModeOnline, 42023},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v, want %+v", rows, want)
	}
}
