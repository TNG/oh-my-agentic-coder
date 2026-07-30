package buildrun

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// GradleLeaf returns the GRADLE_USER_HOME leaf path under the resolved
// OMAC cache scope: <cacheDir>/gradle (per spec §Gradle State). The CLI
// uses this instead of re-deriving the leaf name (P7: the leaf-name
// constant belongs to buildrun, not cli), so `omac build stop` and the
// forced-cancel daemon recycle resolve the same leaf GrantsFor does.
func GradleLeaf(cacheDir string) string {
	return filepath.Join(cacheDir, GradleLeafName)
}

// StopDaemonOptions configures StopGradleDaemon. It reuses the SAME
// isolation the build executor gets (S6: spec.md:125-132 — stop must
// not inherit host env / host ~/.gradle / host creds): the isolated
// ChildEnv (no HOME, GRADLE_USER_HOME=<leaf>, JDK-resolved PATH/JAVA_HOME)
// is the critical part of the executor boundary.
type StopDaemonOptions struct {
	// Wrapper is the canonical repo-owned gradlew path (from Resolve).
	Wrapper string
	// ProjectDir is the build's working directory (the resolved root).
	ProjectDir string
	// Leaf is the GRADLE_USER_HOME leaf (GradleLeaf(cacheDir)).
	Leaf string
	// Grants supplies the isolated ChildEnv. nil falls back to a
	// minimal env built from Leaf (no HOME, no host creds) — but the
	// preferred path is a real Grants so the JDK is resolved the same
	// way as the build.
	Grants *BuildGrants
	// Stdout/Stderr receive the wrapper's output.
	Stdout io.Writer
	Stderr io.Writer
	// ForceKillAfter bounds how long StopGradleDaemon waits after the
	// cooperative `gradlew --stop` before force-killing lingering
	// daemons for this leaf (S7: spec.md:146). Zero uses
	// DefaultStopForceKillAfter.
	ForceKillAfter time.Duration
	// Cmdline, when non-nil, overrides the platform command-line probe
	// (ps on macOS, /proc on Linux) used to confirm a registry-listed
	// pid is associated with this leaf before SIGKILLing it. Tests
	// inject a fake (the in-sandbox environment blocks `ps`, so the
	// production probe returns "" and an unidentifiable pid is
	// conservatively NOT killed); production leaves it nil so the real
	// platform probe runs on the host.
	Cmdline func(pid int) string
}

// DefaultStopForceKillAfter is the cooperative->force deadline for
// StopGradleDaemon. After `gradlew --stop` returns, StopGradleDaemon
// waits this long for daemons to exit, then SIGKILLs any still-registered
// daemon for the leaf (a wedged daemon that ignores --stop).
const DefaultStopForceKillAfter = 10 * time.Second

// StopGradleDaemon runs `gradlew --stop` under the SAME restricted env as
// the build (S6: isolated ChildEnv, NOT os.Environ — no host HOME, no
// host ~/.gradle, no host creds; GRADLE_USER_HOME=<leaf>; JDK-resolved
// PATH/JAVA_HOME), so the cooperative stop targets THIS worktree's leaf
// daemons only. After the cooperative stop, it force-kills any lingering
// daemon for the leaf (S7: a wedged daemon that ignores --stop).
//
// Returns nil if the cooperative stop succeeded and no daemon required a
// force-kill. A non-zero `gradlew --stop` exit code is returned as-is
// (the caller maps it through). A force-kill failure is best-effort:
// logged to Stderr but not returned (the cooperative stop already ran).
//
// Applying the full kernel sandbox to `gradlew --stop` is risky: --stop
// signals a daemon across the process boundary, which a deny-default
// profile may block. The ENV is always isolated (the critical part for
// the spec boundary); the kernel sandbox is NOT applied to the stop
// process. This tradeoff is documented in docs/build-command.md.
func StopGradleDaemon(opts StopDaemonOptions) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	cmd := exec.Command(opts.Wrapper, "--stop")
	cmd.Dir = opts.ProjectDir
	cmd.Env = stopEnv(opts)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// No sandbox: --stop signals a daemon across the process boundary,
	// which a deny-default profile may block. ENV isolation (the spec
	// boundary: no host HOME, no host ~/.gradle, no host creds) is the
	// critical part and IS applied via stopEnv. Documented in
	// docs/build-command.md.
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee
		}
		return fmt.Errorf("gradle --stop: %w", err)
	}

	// S7: cooperative stop done; force-kill lingering daemons for this
	// leaf. Best-effort — a failure here is logged but not returned.
	wait := opts.ForceKillAfter
	if wait <= 0 {
		wait = DefaultStopForceKillAfter
	}
	forceKillLingeringDaemons(opts.Leaf, wait, stderr, opts.Cmdline)
	return nil
}

// stopEnv builds the isolated environment for `gradlew --stop`: the
// SAME restricted env the build executor gets (S6). When a Grants is
// supplied, ChildEnv is reused verbatim (no HOME, GRADLE_USER_HOME=leaf,
// JDK-resolved PATH/JAVA_HOME, proxy GRADLE_OPTS if configured). Without
// a Grants, a minimal env is built from the leaf (still no HOME, still
// no host creds) so a standalone stop stays within the executor boundary.
func stopEnv(opts StopDaemonOptions) []string {
	if opts.Grants != nil {
		return ChildEnv(opts.Grants)
	}
	// Minimal fallback: passthrough allowlist WITHOUT HOME, plus the
	// leaf as GRADLE_USER_HOME and the parent PATH/JAVA_HOME (best-effort
	// — without a Grants the JDK is not resolved, but HOME is still
	// absent, which is the spec-critical part).
	env := []string{"GRADLE_USER_HOME=" + opts.Leaf}
	if v := os.Getenv("PATH"); v != "" {
		env = append(env, "PATH="+v)
	}
	if v := os.Getenv("JAVA_HOME"); v != "" {
		env = append(env, "JAVA_HOME="+v)
	}
	for _, name := range envPassThrough {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// forceKillLingeringDaemons is the S7 two-stage teardown fallback: after
// the cooperative `gradlew --stop`, scan the leaf's daemon registry for
// daemons still marked active and SIGKILL them by pid. A wedged daemon
// that ignores --stop would otherwise linger with potentially-corrupt
// state. Best-effort and platform-aware (the registry layout is the same
// on darwin/linux; process enumeration is avoided in favor of the
// registry, which is the authoritative source of daemon pids for the
// leaf). Errors are logged to stderr, not returned.
//
// This is the bounded, registry-based approach the finding allows ("if
// robust daemon detection is too much, at minimum: run a bounded wait and
// if the daemon registry still shows active daemons for the leaf,
// SIGKILL by pid from the registry"). Full process enumeration by
// scanning /proc or `ps` is a later hardening item.
func forceKillLingeringDaemons(leaf string, wait time.Duration, stderr io.Writer, cmdlineProbe func(int) string) {
	// Give the cooperative stop time to land before scanning.
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		pids := activeDaemonPIDs(leaf, cmdlineProbe)
		if len(pids) == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	pids := activeDaemonPIDs(leaf, cmdlineProbe)
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			fmt.Fprintf(stderr, "omac build stop: warning: could not SIGKILL lingering daemon pid %d: %v\n", pid, err)
		}
	}
}

// daemonRegistryDir is the Gradle daemon registry location under a leaf.
// Each daemon version has its own subdir under <leaf>/.gradle/daemon/;
// the registry.bin file (and its .lock) live there. We read registry.bin
// to extract active daemon pids. The format is opaque (binary), so we
// scan for pid integers heuristically — robust daemon detection by
// parsing the binary registry is a later hardening item; the heuristic
// catches the common case where the pid appears as a readable integer in
// the registry's text-ish header.
func daemonRegistryDir(leaf string) string {
	return filepath.Join(leaf, ".gradle", "daemon")
}

// activeDaemonPIDs scans the leaf's daemon registry for daemon pids that
// still have a running process. Returns pids whose /proc/<pid> or
// kill(pid,0) succeeds AND whose command line / args reference the leaf
// (so we never SIGKILL an unrelated java process that recycled a pid
// Gradle listed). Best-effort; returns nil if the registry is absent or
// no listed pid is both alive and associated with this leaf.
func activeDaemonPIDs(leaf string, cmdlineProbe func(int) string) []int {
	regDir := daemonRegistryDir(leaf)
	entries, err := os.ReadDir(regDir)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bin := filepath.Join(regDir, e.Name(), "registry.bin")
		data, err := os.ReadFile(bin)
		if err != nil {
			continue
		}
		for _, pid := range extractPIDs(string(data)) {
			if pid <= 0 {
				continue
			}
			if !processAliveAndAssociated(pid, leaf, cmdlineProbe) {
				continue
			}
			out = append(out, pid)
		}
	}
	return out
}

// extractPIDs pulls decimal integers out of the (opaque) registry.bin
// content as candidate daemon pids. The Gradle registry is binary but
// embeds pids as readable integers in its header; a full parser is a
// later hardening item. Dedupes and bounds to plausible pid ranges.
func extractPIDs(s string) []int {
	seen := map[int]bool{}
	var out []int
	var num strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			num.WriteRune(r)
			continue
		}
		if num.Len() > 0 {
			if pid, err := strconv.Atoi(num.String()); err == nil && pid > 1 && !seen[pid] {
				seen[pid] = true
				out = append(out, pid)
			}
			num.Reset()
		}
	}
	if num.Len() > 0 {
		if pid, err := strconv.Atoi(num.String()); err == nil && pid > 1 && !seen[pid] {
			out = append(out, pid)
		}
	}
	return out
}

// processAliveAndAssociated reports whether pid is alive AND its command
// line / args reference the leaf path (so a recycled pid pointing at an
// unrelated java process is never SIGKILLed). Platform-aware: on Linux
// /proc/<pid>/cmdline is read; on macOS `ps` is used. Best-effort: on
// any error the pid is treated as not-associated (skipped, not killed).
func processAliveAndAssociated(pid int, leaf string, cmdlineProbe func(int) string) bool {
	// Alive check first (kill 0).
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	var cmdline string
	if cmdlineProbe != nil {
		cmdline = cmdlineProbe(pid)
	} else {
		cmdline = processCmdline(pid)
	}
	if cmdline == "" {
		// Cannot read cmdline: be conservative and do NOT kill an
		// unidentifiable process (e.g. inside a sandbox that blocks ps).
		return false
	}
	return strings.Contains(cmdline, leaf)
}

// processCmdline returns the command line of pid, best-effort. Linux:
// /proc/<pid>/cmdline. macOS: `ps -o args= -p <pid>`. Empty on error.
func processCmdline(pid int) string {
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return ""
		}
		// /proc/<pid>/cmdline is null-separated; normalize to spaces.
		return strings.ReplaceAll(string(b), "\x00", " ")
	case "darwin":
		out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return ""
		}
		return string(out)
	default:
		return ""
	}
}
