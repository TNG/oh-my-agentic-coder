// Package sandbox expands the argv template in a SandboxProfile into a
// concrete argv ready for exec.
//
// Placeholders (see oh-my-agentic-coder.md §14.2):
//
//	{{socket}}              absolute socket path
//	{{socket_dir}}          directory containing the socket
//	{{workdir}}             absolute workdir
//	{{skills_csv}}          comma-separated list of registered skill mounts
//	{{inner_cmd}}           first element of inner argv
//	{{inner_args}}          remaining inner argv (splats in place)
//	{{per_skill_env_flags}} --env OMAC_<SKILL>_BASE=... flags (splats)
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

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// Inputs captures everything needed to expand a sandbox profile.
type Inputs struct {
	Workdir  string
	Socket   string   // bridge.sock path (Unix transport)
	TCPPort  int      // bound 127.0.0.1 port (TCP transport); 0 disables {{tcp_port}}
	Mounts   []string // skill mount names
	InnerCmd []string // [cmd, args...] — InnerCmd[0] is {{inner_cmd}}; rest is {{inner_args}}
}

// Expand applies the profile template to Inputs and returns the resulting argv.
func Expand(profile config.SandboxProfile, in Inputs) ([]string, error) {
	if len(profile.Command) == 0 {
		return nil, fmt.Errorf("sandbox profile has no command")
	}
	innerCmd := ""
	var innerArgs []string
	switch {
	case len(in.InnerCmd) == 0 && len(profile.InnerCmd) == 0:
		return nil, fmt.Errorf("no inner_cmd provided")
	case len(in.InnerCmd) == 0:
		innerCmd = profile.InnerCmd[0]
		innerArgs = profile.InnerCmd[1:]
	default:
		innerCmd = in.InnerCmd[0]
		innerArgs = in.InnerCmd[1:]
	}
	skillsCSV := strings.Join(in.Mounts, ",")

	perSkillFlags := make([]string, 0, 2*len(in.Mounts))
	for _, m := range in.Mounts {
		perSkillFlags = append(perSkillFlags, "--env", perSkillEnv(m, in.Socket))
	}

	scalar := map[string]string{
		"socket":     in.Socket,
		"socket_dir": filepath.Dir(in.Socket),
		"workdir":    in.Workdir,
		"skills_csv": skillsCSV,
		"inner_cmd":  innerCmd,
		"tcp_port":   fmt.Sprintf("%d", in.TCPPort),
	}
	list := map[string][]string{
		"inner_args":          innerArgs,
		"per_skill_env_flags": perSkillFlags,
	}

	out := make([]string, 0, len(profile.Command))
	for _, token := range profile.Command {
		if name, ok := splatToken(token); ok {
			if v, isList := list[name]; isList {
				out = append(out, v...)
				continue
			}
			if v, isScalar := scalar[name]; isScalar {
				out = append(out, v)
				continue
			}
			return nil, fmt.Errorf("unknown placeholder {{%s}}", name)
		}
		// Embedded scalar placeholders (partial substitution).
		expanded, err := substituteScalars(token, scalar, list)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded)
	}
	return out, nil
}

// splatToken returns the inner name if token is exactly "{{name}}".
func splatToken(tok string) (string, bool) {
	if len(tok) < 5 || !strings.HasPrefix(tok, "{{") || !strings.HasSuffix(tok, "}}") {
		return "", false
	}
	name := tok[2 : len(tok)-2]
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	return name, true
}

// substituteScalars replaces {{name}} occurrences inside a larger string.
// List placeholders are not allowed in this mode.
func substituteScalars(tok string, scalar map[string]string, list map[string][]string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(tok); {
		if i+1 < len(tok) && tok[i] == '{' && tok[i+1] == '{' {
			end := strings.Index(tok[i+2:], "}}")
			if end >= 0 {
				name := strings.TrimSpace(tok[i+2 : i+2+end])
				if _, isList := list[name]; isList {
					return "", fmt.Errorf("list placeholder {{%s}} must stand alone", name)
				}
				v, ok := scalar[name]
				if !ok {
					return "", fmt.Errorf("unknown placeholder {{%s}}", name)
				}
				b.WriteString(v)
				i += 2 + end + 2
				continue
			}
		}
		b.WriteByte(tok[i])
		i++
	}
	return b.String(), nil
}

// perSkillEnv returns "OMAC_<SKILL>_BASE=http+unix://.../<mount>".
func perSkillEnv(mount, socket string) string {
	return OmacEnvName(mount) + "=" + OmacEnvValue(mount, socket)
}

// OmacEnvName maps a mount like "himalaya-email" to "OMAC_HIMALAYA_EMAIL_BASE".
// This is the flat (single-workdir / start-mode) form.
func OmacEnvName(mount string) string {
	return "OMAC_" + envIdent(mount) + "_BASE"
}

// OmacDirEnvName maps a (dir-token, mount) to the serve-mode workdir-local
// form "OMAC_D_<TOKEN>_<MOUNT>_BASE" (docs/MULTI_DIR_DESKTOP.md §4.1).
func OmacDirEnvName(dirToken, mount string) string {
	return "OMAC_D_" + envIdent(dirToken) + "_" + envIdent(mount) + "_BASE"
}

// OmacGlobalEnvName maps a mount to the serve-mode global (shared) form
// "OMAC_G_<MOUNT>_BASE" (docs/MULTI_DIR_DESKTOP.md §4.5).
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
// is the default (because TCP is the transport that works under
// nono proxy mode on macOS).
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
// This is the form sandboxed clients should use when nono proxy mode
// is active (or any other environment that blocks AF_UNIX connect).
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
// terminal-handling contract of Exec (docs/MULTI_DIR_DESKTOP.md §5.3).
//
// onReady must not block indefinitely on its own; it should spin up its
// goroutines and return. Any teardown it needs to do on child exit should
// be wired via the caller's own defers (the caller still owns the facade
// and supervisor).
func ExecWithReady(argv []string, extraEnv map[string]string, onReady func()) (int, error) {
	if len(argv) == 0 {
		return 1, fmt.Errorf("empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Inherit host env, then overlay extras.
	env := os.Environ()
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	// Place the child in its own process group. We will (a) forward signals
	// we receive to that group, and (b) when stdin is a tty, hand the
	// terminal foreground over to it so Ctrl-C is delivered directly.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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
	// Kick off the caller's concurrent work (serve's control plane).
	if onReady != nil {
		go onReady()
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
