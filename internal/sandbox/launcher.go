// Package sandbox assembles the command that launches omac's built-in OS
// sandbox (BuildBuiltinArgv), execs the inner command inside it
// (Exec/ExecWithReady), and derives the OMAC_<SKILL> environment variables
// that skills are reached through.
package sandbox

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Inputs captures everything BuildBuiltinArgv needs to assemble the sandbox
// launch command.
type Inputs struct {
	Socket   string   // bridge.sock path (Unix transport)
	TCPPort  int      // bound 127.0.0.1 port (TCP transport)
	InnerCmd []string // [cmd, args...] run inside the sandbox
	// TmpDir is a host directory that omac grants the sandbox read+write
	// access to and exports as TMPDIR for the inner command. Bun-built
	// harnesses (opencode) extract an embedded runtime into TMPDIR at
	// startup; without a writable, sandbox-granted temp dir the extraction
	// fails. Empty omits the grant.
	TmpDir string
}

// BuildBuiltinArgv builds the argv that launches the builtin sandbox: it
// re-execs the running omac binary as `omac sandbox run` wrapping the inner
// command.
func BuildBuiltinArgv(in Inputs) ([]string, error) {
	if len(in.InnerCmd) == 0 {
		return nil, fmt.Errorf("no inner_cmd provided")
	}
	self, err := os.Executable()
	if err != nil {
		self = "omac" // PATH fallback; better than failing the launch
	}
	argv := []string{
		self, "sandbox", "run",
		"--allow-file", in.Socket,
		"--read", filepath.Dir(in.Socket),
	}
	if in.TmpDir != "" {
		argv = append(argv, "--read", in.TmpDir, "--write", in.TmpDir)
	}
	argv = append(argv, "--open-port", fmt.Sprintf("%d", in.TCPPort), "--")
	argv = append(argv, in.InnerCmd...)
	return argv, nil
}

// OmacEnvName maps a mount like "himalaya-email" to "OMAC_HIMALAYA_EMAIL_BASE".
// This is the flat (single-workdir / start-mode) form.
func OmacEnvName(mount string) string {
	return "OMAC_" + envIdent(mount) + "_BASE"
}

// OmacDirEnvName maps a (dir-token, mount) to the serve-mode workdir-local
// form "OMAC_D_<TOKEN>_<MOUNT>_BASE" (docs/contributing/serve-spec.md).
func OmacDirEnvName(dirToken, mount string) string {
	return "OMAC_D_" + envIdent(dirToken) + "_" + envIdent(mount) + "_BASE"
}

// OmacGlobalEnvName maps a mount to the serve-mode global (shared) form
// "OMAC_G_<MOUNT>_BASE" (docs/contributing/serve-spec.md).
func OmacGlobalEnvName(mount string) string {
	return "OMAC_G_" + envIdent(mount) + "_BASE"
}

// envIdent upper-cases an identifier and replaces every non-alphanumeric
// rune with '_', matching the historical OmacEnvName behaviour.
func envIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteByte(byte(r) - 32)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// OmacSocketEnvName maps a mount like "himalaya-email" to
// "OMAC_HIMALAYA_EMAIL_SOCKET_BASE" — the env var carrying the
// http+unix:// URL form. The TCP form lives under OmacEnvName, which
// is the default (because TCP is the transport that works under every
// sandbox backend, including those that block AF_UNIX connect).
func OmacSocketEnvName(mount string) string {
	// Strip the trailing "_BASE" we'd get from OmacEnvName and append
	// "_SOCKET_BASE" instead, so the two forms have parallel suffixes.
	return strings.TrimSuffix(OmacEnvName(mount), "_BASE") + "_SOCKET_BASE"
}

// OmacEnvValue returns the http+unix URL for the given mount.
func OmacEnvValue(mount, socket string) string {
	return "http+unix://" + url.PathEscape(socket) + "/" + mount
}

// OmacTCPEnvValue returns the http://127.0.0.1:<port>/<mount> URL.
// This is the form sandboxed clients should use in any environment that
// blocks AF_UNIX connect.
func OmacTCPEnvValue(mount string, port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/%s", port, mount)
}

// OmacTCPEnvValueNS returns the http://127.0.0.1:<port>/<namespace>/<mount>
// URL for serve-mode namespaced routes (dir token or "__global__"). An
// empty namespace falls back to the flat form.
func OmacTCPEnvValueNS(namespace, mount string, port int) string {
	if namespace == "" {
		return OmacTCPEnvValue(mount, port)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/%s/%s", port, namespace, mount)
}

// OmacEnvValueNS returns the http+unix URL for a serve-mode namespaced
// route. An empty namespace falls back to the flat form.
func OmacEnvValueNS(namespace, mount, socket string) string {
	if namespace == "" {
		return OmacEnvValue(mount, socket)
	}
	return "http+unix://" + url.PathEscape(socket) + "/" + namespace + "/" + mount
}

// Exec runs the argv as a child process and waits for it, forwarding stdio
// and signals.
//
// Signal handling:
//   - When omac runs attached to a terminal, the child is placed in its own
//     process group AND that group is made the terminal's foreground group
//     (tcsetpgrp), so Ctrl-C from the keyboard is delivered by the kernel
//     directly to the child instead of to omac. omac itself temporarily
//     ignores SIGTTIN/SIGTTOU during this dance and SIGINT/SIGTERM during
//     the lifetime of the child (it forwards them explicitly; see below).
//   - When omac is signalled directly (e.g. `kill -INT <omac-pid>`, or in a
//     non-tty / CI context), the installed handler forwards the signal to
//     the child's process group so the entire sandbox tree exits cleanly.
//   - On clean child exit, omac restores the original foreground pgid and
//     uninstalls its signal handlers.
//
// Returns the child's exit code in 0..255. A signal-killed child maps to
// 128+signum, matching shell convention.
func Exec(argv []string, extraEnv map[string]string) (int, error) {
	return ExecWithReady(argv, extraEnv, nil)
}

// ExecWithReady is like Exec but invokes onReady (if non-nil) on a
// background goroutine immediately after the child has been started (and
// the terminal handed over), then blocks waiting for the child exactly as
// Exec does. This lets serve mode run its control plane / activation loop
// concurrently with `opencode serve` while preserving the signal- and
// terminal-handling contract of Exec (docs/contributing/serve-spec.md).
//
// onReady must not block indefinitely on its own; it should spin up its
// goroutines and return. Any teardown it needs to do on child exit should
// be wired via the caller's own defers (the caller still owns the facade
// and supervisor).
func ExecWithReady(argv []string, extraEnv map[string]string, onReady func()) (int, error) {
	// Inherit host env, then overlay extras.
	env := os.Environ()
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	var hook func(int)
	if onReady != nil {
		hook = func(int) { onReady() }
	}
	return ExecWithEnv(argv, env, "", hook)
}

// newChildCmd builds the child command with its stdio, environment and
// working directory wired. workdir sets the child's cwd; when empty the
// caller's cwd is inherited (ExecWithReady's behavior). On Linux bwrap
// re-chdirs inside the namespace via --chdir, but Seatbelt has no such
// flag, so this is the only place the macOS child's cwd is established —
// without it a relative command like ./gradlew from --workdir won't resolve.
func newChildCmd(argv []string, env []string, workdir string) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env
	if workdir != "" {
		cmd.Dir = workdir
	}
	// Place the child in its own process group. We will (a) forward signals
	// we receive to that group, and (b) when stdin is a tty, hand the
	// terminal foreground over to it so Ctrl-C is delivered directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// ExecWithEnv is like ExecWithReady but takes the child environment
// verbatim (no host-env inheritance), sets the child's working directory
// to workdir (empty inherits the caller cwd), and passes the child pid to
// the onReady hook (the learn-mode recorder samples the child's process
// group). Used by `omac sandbox run`, which builds the child env from
// scratch (env_clear semantics with blocklist/allowlist filtering).
func ExecWithEnv(argv []string, env []string, workdir string, onReady func(pid int)) (int, error) {
	if len(argv) == 0 {
		return 1, fmt.Errorf("empty argv")
	}
	cmd := newChildCmd(argv, env, workdir)

	// CRITICAL: install our own signal handlers BEFORE we fork+exec the
	// child. POSIX execve(2) preserves SIG_IGN through exec, but converts
	// any explicitly-installed handler to SIG_DFL. So if the parent
	// shell launched omac with SIGINT ignored (e.g. omac in a background
	// job, or any non-interactive bash which masks SIGINT for async
	// children), and we did NOT pre-install a handler, the inherited
	// SIG_IGN would survive the fork+exec into bash/opencode/etc., and
	// our pgroup-wide kill(-pgid, SIGINT) would be silently ignored.
	//
	// signal.Notify here installs a Go-runtime handler (sa_handler, not
	// SIG_IGN). After cmd.Start fork+execs, the child resets to SIG_DFL,
	// which is what we want: SIGINT terminates by default.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh,
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigCh)

	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("exec %s: %w", argv[0], err)
	}

	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		// Setpgid races with cmd.Start sometimes; on darwin/linux this
		// almost always succeeds, but if it doesn't we fall back to using
		// the pid as the pgid (kill(2) accepts that as a single-process
		// target, which is degraded but not broken).
		pgid = pid
	}

	// If we have a controlling terminal, give it to the child's pgid so the
	// kernel's terminal-driver-driven SIGINT goes there. Ignore SIGTTOU
	// during tcsetpgrp itself; otherwise we get suspended on the syscall
	// because we are no longer the foreground group.
	tty, restoreTTY := claimTerminalFor(pgid)
	defer restoreTTY()
	_ = tty
	done := make(chan struct{})
	go func() {
		// Escalation policy when omac itself receives a termination signal:
		//   1. Forward the original signal to -pgid.
		//   2. If the child is still alive after 2s, send SIGTERM.
		//   3. If still alive after another 3s, send SIGKILL.
		// This makes us robust to children that inherited SIG_IGN for the
		// signal we forwarded (which can happen when omac was launched
		// from a non-interactive parent that masked SIGINT for async
		// children — POSIX execve preserves SIG_IGN).
		var first bool
		for {
			select {
			case <-done:
				return
			case s := <-sigCh:
				if ss, ok := s.(syscall.Signal); ok {
					_ = syscall.Kill(-pgid, ss)
					if !first {
						first = true
						go func() {
							select {
							case <-done:
								return
							case <-time.After(2 * time.Second):
							}
							_ = syscall.Kill(-pgid, syscall.SIGTERM)
							select {
							case <-done:
								return
							case <-time.After(3 * time.Second):
							}
							_ = syscall.Kill(-pgid, syscall.SIGKILL)
						}()
					}
				}
			}
		}
	}()

	// The child is up and (when interactive) owns the terminal foreground.
	// Kick off the caller's concurrent work (serve's control plane,
	// learn-mode recorder).
	if onReady != nil {
		go onReady(pid)
	}

	waitErr := cmd.Wait()
	close(done)

	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
				if ws.Exited() {
					return ws.ExitStatus(), nil
				}
				if ws.Signaled() {
					return 128 + int(ws.Signal()), nil
				}
			}
			return ee.ExitCode(), nil
		}
		return 1, waitErr
	}
	return 0, nil
}
