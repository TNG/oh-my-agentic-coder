package buildrun

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
)

// RunStopInSandboxOptions configures RunStopInSandbox — the in-sandbox
// post-build `gradlew --stop` recycle (ticket 07, spec.md §236). The
// recycle runs under the SAME sandboxrun.BuildChildArgv grants and
// ChildEnv as the build, in its own short-lived process group, so ADR
// 0001's cold-start-per-build behavior is preserved without an
// unsandboxed host wrapper invocation (the Phase-3 supervisor
// requirement).
//
// This replaces the engine's old `daemonRecycle` closure which ran
// `gradlew --stop` as a SEPARATE unsandboxed `exec.Command` after
// RunBuild returned (engine.go:565, Phase 2 baseline). The in-sandbox
// recycle is the Option-B supervisor shape: the supervisor is a
// host-side goroutine (the engine) that, after the wrapper exits,
// launches a SECOND in-sandbox process (`gradlew --stop` with the same
// grants, same Linux network namespace) for the recycle. Cancellation
// targeted only the wrapper's process group; this `--stop` invocation
// has its own short-lived process group, so a wrapper cancel does not
// tear it down.
type RunStopInSandboxOptions struct {
	// Resolved is the resolved build request (Wrapper + ProjectDir +
	// Args are used; Args is ignored, --stop is the sole wrapper arg).
	Resolved Resolved
	// Grants supplies the SAME sandbox grants + isolated ChildEnv the
	// build used. nil is a service failure (the recycle cannot run
	// unsandboxed — that is the Phase-2 behavior ticket 07 forbids).
	Grants *BuildGrants
	// Stdout/Stderr receive the wrapper's output.
	Stdout io.Writer
	Stderr io.Writer
	// Launcher, nil selects the platform sandbox via sandboxrun, the
	// SAME seam RunBuild uses so the recycle runs under the same kernel
	// sandbox (and the same Linux network namespace) as the build.
	// Tests inject NoSandboxLauncher.
	Launcher func(g *BuildGrants, innerArgv []string) ([]string, error)
	// Auditor receives the recycle lifecycle events; nil → audit.Nop().
	Auditor audit.Auditor
	// Timeout bounds the whole `gradlew --stop` invocation. Zero uses
	// DefaultStopInSandboxTimeout. The recycle is bounded so a wedged
	// daemon cannot hold the leaf lock forever.
	Timeout time.Duration
	// GroupSignal delivers a signal to the child's process group (the
	// same seam RunOptions.GroupSignal documents). Nil uses groupSignal
	// (syscall.Kill); tests inject a recorder. Used only for the
	// timeout force-kill of the `--stop` process group.
	GroupSignal func(pid int, sig syscall.Signal) error
}

// DefaultStopInSandboxTimeout bounds the in-sandbox `gradlew --stop`
// recycle. After it elapses, RunStopInSandbox SIGKILLs the `--stop`
// process group and returns a timeout error (the caller — the engine —
// treats a recycle launch/timeout failure as a mandatory cleanup failure
// per spec §Mandatory cleanup failure, overriding the primary result
// with service_failure). 30s matches the codebase's
// buildcontrol.DefaultQueueTimeout for consistency; `gradlew --stop` is
// normally sub-second.
const DefaultStopInSandboxTimeout = 30 * time.Second

// ErrStopInSandboxTimeout is returned by RunStopInSandbox when the
// `gradlew --stop` invocation exceeds the Timeout. The process group is
// SIGKILLed before returning. The caller treats this as a mandatory
// cleanup failure.
var ErrStopInSandboxTimeout = errors.New("buildrun: in-sandbox gradlew --stop timed out")

// RunStopInSandbox runs `gradlew --stop` under the SAME restricted
// executor lifecycle as the build (ticket 07, spec.md §236): the same
// sandboxrun.BuildChildArgv grants, the same isolated ChildEnv (no HOME,
// no host ~/.gradle, no host creds, GRADLE_USER_HOME=<leaf>,
// JDK-resolved PATH/JAVA_HOME), and — on Linux — the same private
// loopback network namespace. This preserves ADR 0001's
// cold-start-per-build behavior without an unsandboxed host wrapper
// invocation.
//
// It is the Option-B supervisor's recycle step: after the wrapper exits,
// the engine (the host-side supervisor) calls this to recycle the
// daemon INSIDE the same sandbox lifecycle. The `--stop` process runs
// in its OWN process group (Setpgid: true) so it is NOT torn down by a
// wrapper cancellation (which targeted the wrapper's group only); the
// supervisor survives to complete the recycle.
//
// Returns nil on success (cooperative stop exited 0). A non-zero
// `gradlew --stop` exit code is returned as a *exec.ExitError (the
// caller maps it through, same as the legacy StopGradleDaemon). A
// launch failure, a timeout, or an exec/IO error is a non-nil error
// (the caller treats it as a mandatory cleanup failure →
// service_failure, per spec §Mandatory cleanup failure).
//
// The kernel sandbox IS applied to `gradlew --stop` here (unlike the
// legacy StopGradleDaemon, which deliberately did NOT apply it because
// --stop signals a daemon across the process boundary). Under ticket
// 07's ownership model the daemon is OMAC-owned and the recycle runs
// in the same sandbox where the daemon already lives; the
// sandbox-projected control state (the handshake socket path, the
// init scripts) is reachable, and `--stop` cooperatively asks the
// daemon to exit rather than signalling an unrelated process. A
// deny-default profile that blocks the daemon's IPC would surface as
// a non-zero `--stop` exit, which the caller maps to a cleanup
// failure — the fail-closed outcome the spec requires.
func RunStopInSandbox(opts RunStopInSandboxOptions) error {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	if opts.Grants == nil {
		return errors.New("buildrun: RunStopInSandbox requires Grants (the recycle cannot run unsandboxed)")
	}
	launch := opts.Launcher
	if launch == nil {
		launch = func(g *BuildGrants, innerArgv []string) ([]string, error) {
			return sandboxrun.BuildChildArgv(g.Grants, innerArgv)
		}
	}
	auditor := opts.Auditor
	if auditor == nil {
		auditor = audit.Nop()
	}
	sigGroup := opts.GroupSignal
	if sigGroup == nil {
		sigGroup = groupSignal
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultStopInSandboxTimeout
	}

	innerArgv := []string{opts.Resolved.Wrapper, "--stop"}
	auditor.Emit(audit.InnerExec(innerArgv, "build-gradle-stop", true))
	started := time.Now()

	argv, err := launch(opts.Grants, innerArgv)
	if err != nil {
		emitStopExit(auditor, ExitServiceFailure, started)
		return fmt.Errorf("buildrun: launch in-sandbox gradlew --stop: %w", err)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = opts.Resolved.ProjectDir
	cmd.Env = ChildEnv(opts.Grants)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = nil
	// Own process group so a wrapper cancellation (which targeted the
	// wrapper's group only) does not tear down the recycle. The
	// supervisor survives to complete the `--stop`.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		emitStopExit(auditor, ExitServiceFailure, started)
		return fmt.Errorf("buildrun: start in-sandbox gradlew --stop: %w", err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-waitErr:
		if err == nil {
			emitStopExit(auditor, 0, started)
			return nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			emitStopExit(auditor, ee.ExitCode(), started)
			return ee
		}
		emitStopExit(auditor, ExitServiceFailure, started)
		return fmt.Errorf("buildrun: in-sandbox gradlew --stop: %w", err)
	case <-timer.C:
		// Bounded recycle: SIGKILL the `--stop` process group and
		// return a timeout error. The caller (engine) treats this as
		// a mandatory cleanup failure → service_failure.
		_ = sigGroup(-pgid, syscall.SIGKILL)
		// Reap the child so it does not linger as a zombie.
		go func() { <-waitErr }()
		emitStopExit(auditor, ExitServiceFailure, started)
		return fmt.Errorf("buildrun: %w (after %s)", ErrStopInSandboxTimeout, timeout)
	}
}

// emitStopExit emits the audit ProcessExit event for the in-sandbox
// `gradlew --stop` recycle. Mirrors run.go's emitExit.
func emitStopExit(a audit.Auditor, code int, started time.Time) {
	a.Emit(audit.ProcessExit("build-stop", "", code, time.Since(started).Milliseconds()))
}
