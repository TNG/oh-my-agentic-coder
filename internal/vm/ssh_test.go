package vm

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSSHArgs(t *testing.T) {
	args := SSHArgs(SSHTarget{User: "agent", Port: 42022, KeyPath: "/k", KnownHosts: "/kh"})
	pairs := [][2]string{
		{"-p", "42022"},
		{"-i", "/k"},
		{"-o", "UserKnownHostsFile=/kh"},
		{"-o", "StrictHostKeyChecking=accept-new"},
		{"-o", "BatchMode=yes"},
		{"-o", "IdentitiesOnly=yes"},
		{"-o", "LogLevel=ERROR"},
	}
	for _, p := range pairs {
		found := false
		for i := 0; i+1 < len(args); i++ {
			if args[i] == p[0] && args[i+1] == p[1] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SSHArgs %v lacks %s %s", args, p[0], p[1])
		}
	}
	if slices.Contains(args, "-t") {
		t.Errorf("SSHArgs without TTY = %v, want no -t", args)
	}
	if args := SSHArgs(SSHTarget{User: "agent", Port: 42022, KeyPath: "/k", KnownHosts: "/kh", TTY: true}); !slices.Contains(args, "-t") {
		t.Errorf("SSHArgs with TTY = %v, want -t", args)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"it's": `'it'\''s'`,
		"":     `''`,
		"a b":  `'a b'`,
		"$x":   `'$x'`,
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestShellJoin(t *testing.T) {
	if got, want := ShellJoin([]string{"a b", "c"}), `'a b' c`; got != want {
		t.Errorf("ShellJoin = %s, want %s", got, want)
	}
}

func TestDefaultSSHBuildsCommand(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OMAC_TEST_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(t.TempDir(), "args")
	t.Setenv("PATH", dir)
	t.Setenv("OMAC_TEST_ARGS", argsPath)

	target := SSHTarget{User: "agent", Port: 42022, KeyPath: "/k", KnownHosts: "/kh"}
	if err := DefaultSSH(context.Background(), target, nil, nil, nil, "docker ps"); err != nil {
		t.Fatalf("DefaultSSH: %v", err)
	}
	raw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(got) < 3 {
		t.Fatalf("args too short: %q", got)
	}
	tail := got[len(got)-3:]
	if want := []string{"agent@127.0.0.1", "--", "docker ps"}; !slices.Equal(tail, want) {
		t.Errorf("trailing args = %q, want %q", tail, want)
	}
	if head := got[:len(got)-3]; !slices.Equal(head, SSHArgs(target)) {
		t.Errorf("leading args = %q, want SSHArgs %q", head, SSHArgs(target))
	}
}
