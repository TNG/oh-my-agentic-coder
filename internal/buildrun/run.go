package buildrun

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
)

// defaultLaunch adapts sandboxrun.BuildChildArgv to the RunOptions.Launcher
// field: the seam between "everything except the kernel sandbox
// application" and the platform sandbox itself. Tests replace it with
// NoSandboxLauncher so every behavior except kernel enforcement runs
// without applying a Seatbelt/bwrap profile.
func defaultLaunch(g *BuildGrants, innerArgv []string) ([]string, error) {
	return sandboxrun.BuildChildArgv(g.Grants, innerArgv)
}

// NoSandboxLauncher is the unsandboxed launch adapter: it runs the inner
// argv directly. Unit and integration tests inject it via
// RunOptions.Launcher so everything except kernel enforcement executes
// without a Seatbelt/bwrap profile (which nested sandboxes cannot apply).
func NoSandboxLauncher(g *BuildGrants, innerArgv []string) ([]string, error) {
	return innerArgv, nil
}

// RunOptions bundles the inputs for RunBuild.
type RunOptions struct {
	Resolved Resolved
	Grants   *BuildGrants
	// Stdout/Stderr receive the build's output incrementally (direct pipe
	// through, never buffered to completion).
	Stdout io.Writer
	Stderr io.Writer
	// Launcher, nil selects the platform sandbox via sandboxrun.
	Launcher func(g *BuildGrants, innerArgv []string) ([]string, error)
	// Auditor receives the build lifecycle events; nil → audit.Nop().
	Auditor audit.Auditor
	// Cancel, when non-nil and closed, cancels the build: SIGTERM to the
	// child's process group, then SIGKILL after KillAfter.
	Cancel <-chan struct{}
	// KillAfter bounds the graceful window before SIGKILL. Zero uses the
	// documented default (5s).
	KillAfter time.Duration
	// GroupSignal delivers a signal to the child's process group
	// (negative pid semantics). Nil uses groupSignal (syscall.Kill);
	// tests inject a recorder to assert the staged graceful-then-kill
	// sequence without signalling real process groups.
	GroupSignal func(pid int, sig syscall.Signal) error
}

// DefaultKillAfter is the documented graceful-cancellation deadline.
const DefaultKillAfter = 5 * time.Second

// RunBuild runs one restricted executor process for the build request.
// stdout/stderr stream straight through (the child writes to the caller's
// writers directly); exit-code and cancellation mapping follows
// ExitCode()'s contract — policy denials never reach this function (they
// fail earlier in ParseArgs/Resolve), so RunBuild returns the mapped code
// for build-success/build-failure/cancellation/service-failure only.
func RunBuild(opts RunOptions) (int, error) {
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	launch := opts.Launcher
	if launch == nil {
		launch = defaultLaunch
	}
	auditor := opts.Auditor
	if auditor == nil {
		auditor = audit.Nop()
	}
	killAfter := opts.KillAfter
	if killAfter <= 0 {
		killAfter = DefaultKillAfter
	}
	sigGroup := opts.GroupSignal
	if sigGroup == nil {
		sigGroup = groupSignal
	}

	innerArgv := append([]string{opts.Resolved.Wrapper}, opts.Resolved.Args...)

	auditor.Emit(audit.InnerExec(innerArgv, "build-gradle", true))
	started := time.Now()

	argv, err := launch(opts.Grants, innerArgv)
	if err != nil {
		emitExit(auditor, ExitServiceFailure, started)
		return ExitServiceFailure, fmt.Errorf("build executor launch: %w", err)
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = opts.Resolved.ProjectDir
	cmd.Env = ChildEnv(opts.Grants)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// No Stdin: builds must never read caller input (no interactive
	// prompts; a daemon prompt would hang a harness-driven request).
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		emitExit(auditor, ExitServiceFailure, started)
		return ExitServiceFailure, fmt.Errorf("start build executor: %w", err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	// childReaped flips to true exactly when Wait returns; the hard-stage
	// goroutine reads it before resorting to SIGKILL.
	childReaped := make(chan struct{})

	cancelled := false
	childDone := false
	var childErr error
	takeResult := func(err error) (int, error) {
		code := mapWaitErr(err)
		if cancelled {
			code = ExitCancelled
			// Marker BEFORE returning 4: a raw `gradle exit 4` never
			// prints the omac-prefixed marker, so callers can
			// disambiguate the reserved code from a build-tool
			// coincidence by stderr contents.
			fmt.Fprintln(stderr, CancelledMarker)
		}
		emitExit(auditor, code, started)
		return code, nil
	}
	for {
		if opts.Cancel == nil {
			err := <-waitErr
			code := mapWaitErr(err)
			emitExit(auditor, code, started)
			return code, nil
		}
		if childDone {
			return takeResult(childErr)
		}
		select {
		case err := <-waitErr:
			childDone = true
			childErr = err
			close(childReaped)
		case <-opts.Cancel:
			if cancelled {
				continue
			}
			cancelled = true
			auditor.Emit(audit.ControlMutation("build.cancel", opts.Resolved.Worktree, "sigterm"))
			// Graceful stage: SIGTERM the whole group...
			_ = sigGroup(-pgid, syscall.SIGTERM)
			// ...hard stage after the deadline — but only while the
			// child is unreaped. Once Wait has returned the child pid is
			// back in the pool, so kill(-pgid, SIGKILL) could hit an
			// unrelated process group that recycled the pgid; skipping
			// it is also correct because a reaped child needs no kill.
			go func() {
				timer := time.NewTimer(killAfter)
				select {
				case <-childReaped:
					timer.Stop()
				case <-timer.C:
					_ = sigGroup(-pgid, syscall.SIGKILL)
				}
			}()
		}
	}
}

// groupSignal is the production GroupSignal: POSIX process-group delivery
// (negative pid) via the raw syscall so Setpgid children are signalled as
// one unit.
func groupSignal(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

// mapWaitErr renders a *exec.ExitError (or nil) into the shell-convention
// exit code: 0..255 for exits, 128+signum for signal kills.
func mapWaitErr(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Exited() {
				return ws.ExitStatus()
			}
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
		}
		return ee.ExitCode()
	}
	return ExitServiceFailure
}

func emitExit(a audit.Auditor, code int, started time.Time) {
	a.Emit(audit.ProcessExit("build", "", code, time.Since(started).Milliseconds()))
}

// --- signal-driven cancellation -----------------------------------------

// SignalContext returns a cancel channel closed on the FIRST SIGINT or
// SIGTERM delivered to this process, a drill-through channel that tests
// use to inject signals without touching the real disposition, and a
// release func restoring the default disposition. The CLI wires the cancel
// channel to RunBuild so a harness interrupting omac cancels the build
// through the staged graceful-then-kill path rather than orphaning the
// executor.
//
// A second received signal is FATAL to the process, but NOT via a raw
// os.Exit: os.Exit skips deferred functions, so the previous
// implementation leaked the build's private temp (+ the whole CLI defer
// chain: audit close, cache-scope lock release). Instead the second
// signal is only recorded; the caller collapses the graceful window to
// KillAfter=0 itself, letting RunBuild's normal return path (and every
// deferred cleanup above it) run to completion before returning
// ExitCancelled.
func SignalContext() (cancel <-chan struct{}, second chan<- os.Signal, release func()) {
	cancelCh := make(chan struct{})
	// Drill channel: writes delivered to the same goroutine signal.Notify
	// feeds. Buffered so a test can inject two signals without blocking
	// before the watcher starts.
	drill := make(chan os.Signal, 2)
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for {
			select {
			case <-sigCh:
			case <-drill:
			}
			select {
			case <-cancelCh:
			default:
				close(cancelCh)
			}
			// Second signal: do NOT os.Exit — unwind through the normal
			// cancel path so deferred cleanup (CleanupTmp, audit close)
			// still runs.
			select {
			case <-sigCh:
			case <-drill:
			}
			return
		}
	}()
	return cancelCh, drill, func() { signal.Stop(sigCh) }
}
