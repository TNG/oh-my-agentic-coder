package vm

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	write("zed", &State{SSHPort: 42023, Mode: ModeOnline, PortBlock: 2})
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
		{Name: "omac", Mode: ModeOffline, SSHPort: 42022},
		{Name: "abc", Mode: "not created"},
		{Name: "zed", Mode: ModeOnline, SSHPort: 42023, PortBlock: 2},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("rows = %+v, want %+v", rows, want)
	}
}

func TestListInstancesShowsCorruptStateAsError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for name, st := range map[string]*State{"omac": {SSHPort: 42022, Mode: ModeOffline}, "zed": {SSHPort: 42024, Mode: ModeOnline}} {
		root, _ := InstanceRoot(name)
		if err := SaveState(NewLayout(root).StatePath, st); err != nil {
			t.Fatal(err)
		}
	}
	root, _ := InstanceRoot("bad")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	broken := NewLayout(root).StatePath
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := ListInstances()
	if err != nil {
		t.Fatalf("ListInstances = %v, want the broken instance as a row", err)
	}
	if len(rows) != 3 || rows[0].Name != "omac" || rows[1].Name != "bad" || rows[2].Name != "zed" {
		t.Fatalf("rows = %+v, want omac, bad, zed", rows)
	}
	if rows[1].Mode != "error" || rows[1].Err == nil || !strings.Contains(rows[1].Err.Error(), broken) {
		t.Errorf("bad row = %+v, want state error naming %s", rows[1], broken)
	}
	if rows[2].Mode != ModeOnline || rows[2].SSHPort != 42024 {
		t.Errorf("zed row = %+v, want it listed normally", rows[2])
	}
}
