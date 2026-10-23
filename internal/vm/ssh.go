package vm

import (
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// SSHTarget is a VM user reachable through the forwarded port on 127.0.0.1.
type SSHTarget struct {
	User       string
	Port       int
	KeyPath    string
	KnownHosts string
	// TTY requests a terminal in the VM (ssh -t), so that Ctrl-C reaches
	// the remote command.
	TTY bool
}

// SSHRunner runs remote, a shell command line, as t.User inside the VM.
type SSHRunner func(ctx context.Context, t SSHTarget, stdin io.Reader, stdout, stderr io.Writer, remote string) error

// DefaultSSH runs the ssh binary found on $PATH.
func DefaultSSH(ctx context.Context, t SSHTarget, stdin io.Reader, stdout, stderr io.Writer, remote string) error {
	args := append(SSHArgs(t), t.User+"@127.0.0.1", "--", remote)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// SSHArgs are the ssh options shared by every connection: only the given
// key, a dedicated known_hosts file, and no interactive prompts.
func SSHArgs(t SSHTarget) []string {
	args := []string{
		"-i", t.KeyPath,
		"-p", strconv.Itoa(t.Port),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + t.KnownHosts,
		"-o", "LogLevel=ERROR",
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
	}
	if t.TTY {
		args = append(args, "-t")
	}
	return args
}

// ShellQuote wraps s in POSIX single quotes; each embedded single quote
// closes the quoting, emits an escaped quote and reopens it.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellJoin quotes each argument that needs it and joins them with spaces.
func ShellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "" && strings.IndexFunc(a, needsQuote) < 0 {
			out[i] = a
		} else {
			out[i] = ShellQuote(a)
		}
	}
	return strings.Join(out, " ")
}

// needsQuote reports runes outside the set a POSIX shell passes through
// unchanged in a bare word.
func needsQuote(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("-_./=:,+@%", r)
}
