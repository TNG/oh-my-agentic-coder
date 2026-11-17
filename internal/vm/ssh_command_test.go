package vm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestShellJoinRoundTripsThroughSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not installed")
	}
	cases := [][]string{
		{"echo", "plain"},
		{"echo", "a b"},
		{"echo", ""},
		{"echo", "it's"},
		{"echo", "$HOME", "`x`", "${y}"},
		{"echo", "a;b", "a|b", "a&b", "a>b"},
		{"echo", "*", "?", "[a]", "~", "#", "!"},
		{"echo", "line\nbreak", "tab\there", `\`},
		{"echo", "-n", "--help"},
	}
	for _, args := range cases {
		// "set --" splits the joined words the way a command line would; it
		// does not execute anything, so no helper binary is needed.
		script := "set -- " + ShellJoin(args) + `; printf '%s\000' "$@"`
		out, err := exec.Command(sh, "-c", script).Output()
		if err != nil {
			t.Errorf("ShellJoin(%q) produced %q, sh failed: %v", args, script, err)
			continue
		}
		got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		if !slices.Equal(got, args) {
			t.Errorf("shell parsed %q into %q, want %q", ShellJoin(args), got, args)
		}
	}
}

func TestDashDashAfterDestinationEndsOptionParsing(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh not installed")
	}
	target := SSHTarget{User: "agent", Port: 42022, KeyPath: "/nonexistent/agent_key", KnownHosts: "/nonexistent/known_hosts"}
	config := func(tail ...string) string {
		args := append([]string{"-G"}, SSHArgs(target)...)
		args = append(append(args, target.User+"@127.0.0.1"), tail...)
		out, err := exec.Command(sshBin, args...).Output()
		if err != nil {
			t.Fatalf("ssh -G %v: %v", tail, err)
		}
		return string(out)
	}

	// Control: without "--", ssh does parse an option after the destination.
	if control := config("-J", "jump.example"); !strings.Contains(control, "proxyjump jump.example") {
		t.Fatal("this ssh does not parse options after the destination; the destination/-- layout needs rework")
	}
	// With "--", the same option has to reach the remote command untouched.
	if got := config("--", "-J", "jump.example"); strings.Contains(got, "proxyjump jump.example") {
		t.Fatal("-- after the destination did not end option parsing; move -- before the destination")
	}
}

func TestSSHArgsIgnoreAmbientConfig(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh not installed")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "agent_key")
	if err := os.WriteFile(key, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	target := SSHTarget{User: "agent", Port: 42022, KeyPath: key, KnownHosts: filepath.Join(dir, "known_hosts")}
	args := append([]string{"-G", "-v"}, SSHArgs(target)...)
	args = append(args, target.User+"@127.0.0.1")
	cmd := exec.Command(sshBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ssh config check: %v: %s", err, &stderr)
	}
	readNull := false
	for line := range strings.Lines(stderr.String()) {
		if _, config, ok := strings.Cut(strings.TrimSpace(line), "Reading configuration data "); ok {
			if config != "/dev/null" {
				t.Errorf("ssh loaded ambient config %q", config)
			}
			readNull = readNull || config == "/dev/null"
		}
	}
	if !readNull {
		t.Errorf("ssh did not read the isolated config: %s", &stderr)
	}
	for _, want := range []string{
		"user agent", "hostname 127.0.0.1", "port 42022",
		"identityfile " + key, "userknownhostsfile " + target.KnownHosts,
		"stricthostkeychecking accept-new", "batchmode yes", "identitiesonly yes",
	} {
		if !slices.Contains(strings.Split(string(out), "\n"), want) {
			t.Errorf("ssh effective config lacks %q", want)
		}
	}
}
