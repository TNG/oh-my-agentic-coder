package vm

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConnFromEnvComplete(t *testing.T) {
	key := filepath.Join("/cache", "vm", "agent_key")
	getenv := envMap(map[string]string{
		EnvVMHost:    "127.0.0.1",
		EnvVMSSHPort: "31022",
		EnvVMSSHKey:  key,
		EnvVMUser:    "alice",
	})
	ci, ok, err := ConnFromEnv(getenv)
	if err != nil || !ok {
		t.Fatalf("ConnFromEnv = %v, %v", ok, err)
	}
	want := SSHTarget{User: "alice", Port: 31022, KeyPath: key, KnownHosts: filepath.Join("/cache", "vm", "known_hosts")}
	if ci.Target != want || !ci.FromSandbox {
		t.Fatalf("got %+v, want %+v FromSandbox", ci, want)
	}
}

func TestConnFromEnvAbsent(t *testing.T) {
	_, ok, err := ConnFromEnv(envMap(nil))
	if ok || err != nil {
		t.Fatalf("ConnFromEnv(empty) = %v, %v; want false, nil", ok, err)
	}
}

func TestConnFromEnvIncomplete(t *testing.T) {
	cases := map[string]map[string]string{
		"port only":             {EnvVMSSHPort: "31022"},
		"no user":               {EnvVMSSHPort: "31022", EnvVMSSHKey: "/k"},
		"bad port":              {EnvVMSSHPort: "x", EnvVMSSHKey: "/k", EnvVMUser: "alice"},
		"port too big":          {EnvVMSSHPort: "70000", EnvVMSSHKey: "/k", EnvVMUser: "alice"},
		"user is an ssh option": {EnvVMSSHPort: "31022", EnvVMSSHKey: "/k", EnvVMUser: "-oProxyCommand=id"},
		"user with uppercase":   {EnvVMSSHPort: "31022", EnvVMSSHKey: "/k", EnvVMUser: "Alice"},
		"user too long":         {EnvVMSSHPort: "31022", EnvVMSSHKey: "/k", EnvVMUser: strings.Repeat("a", 33)},
		"relative key":          {EnvVMSSHPort: "31022", EnvVMSSHKey: "k", EnvVMUser: "alice"},
		"key with space":        {EnvVMSSHPort: "31022", EnvVMSSHKey: "/a b/k", EnvVMUser: "alice"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			_, ok, err := ConnFromEnv(envMap(env))
			if !ok || err == nil {
				t.Fatalf("ConnFromEnv = %v, %v; want true + error", ok, err)
			}
		})
	}
}

func TestConnFromState(t *testing.T) {
	l := NewLayout(t.TempDir())
	if err := SaveState(l.StatePath, &State{SSHPort: 31055, Mode: ModeOffline}); err != nil {
		t.Fatal(err)
	}
	ci, err := ResolveConn(envMap(nil), l, "Bob")
	if err != nil {
		t.Fatal(err)
	}
	want := SSHTarget{User: AgentUserName("Bob"), Port: 31055, KeyPath: l.AgentKey, KnownHosts: l.KnownHosts}
	if ci.Target != want || ci.FromSandbox {
		t.Fatalf("got %+v, want %+v host-side", ci, want)
	}
}

// Inside a session without --vm the state dir is unreadable; the error
// should say how to get a working connection.
func TestConnFromStateUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	l := NewLayout(root)
	if err := SaveState(l.StatePath, &State{SSHPort: 31055}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	_, err := ConnFromState(l, "bob")
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "start it with --vm") {
		t.Fatalf("err = %v, want permission error with --vm hint", err)
	}
}

func TestConnFromStateMissing(t *testing.T) {
	l := NewLayout(t.TempDir())
	if _, err := ResolveConn(envMap(nil), l, "bob"); !errors.Is(err, ErrNotCreated) {
		t.Fatalf("err = %v, want ErrNotCreated", err)
	}
}

func TestResolveConnPrefersEnv(t *testing.T) {
	l := NewLayout(t.TempDir())
	if err := SaveState(l.StatePath, &State{SSHPort: 31055}); err != nil {
		t.Fatal(err)
	}
	getenv := envMap(map[string]string{EnvVMSSHPort: "31022", EnvVMSSHKey: "/c/agent_key", EnvVMUser: "alice"})
	ci, err := ResolveConn(getenv, l, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !ci.FromSandbox || ci.Target.Port != 31022 {
		t.Fatalf("got %+v, want env values", ci)
	}
	// Incomplete env is an error, never a silent fallback to the state dir.
	if _, err := ResolveConn(envMap(map[string]string{EnvVMSSHPort: "31022"}), l, "bob"); err == nil {
		t.Fatal("incomplete env: want error")
	}
}
