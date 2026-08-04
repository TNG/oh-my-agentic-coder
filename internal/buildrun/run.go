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
	// ForceCancel, when non-nil and closed, collapses the graceful
	// KillAfter window to ~0: a second SIGINT/SIGTERM tears down
	// descendants immediately rather than waiting the full graceful
	// window. The first signal preserves the warm Gradle daemon (graceful
	// SIGTERM lets it finish in-flight work and idle-stop on its own); a
	// forced cancellation SIGKILLs the process group to recycle unsafe
	// state. Wired from SignalContext's second-signal channel in the CLI.
	ForceCancel <-chan struct{}
	// KillAfter bounds the graceful window before SIGKILL. Zero uses the
	// documented default (5s). A closed ForceCancel collapses this to
	// forcedKillAfter for the remainder of the cancellation.
	KillAfter time.Duration
	// MaxDuration bounds the total build wall-clock; when it elapses the
	// build is cancelled as if the caller signalled (graceful first, then
	// the staged kill). Zero disables the duration ceiling. This is the
	// resource ceiling for build duration (issues/04:15).
	MaxDuration time.Duration
	// GroupSignal delivers a signal to the child's process group
	// (negative pid semantics). Nil uses groupSignal (syscall.Kill);
	// tests inject a recorder to assert the staged graceful-then-kill
	// sequence without signalling real process groups.
	GroupSignal func(pid int, sig syscall.Signal) error
	// OnForcedCancel, when non-nil, is invoked AFTER a forced
	// cancellation (ForceCancel fired, or a forced teardown from
	// MaxDuration) has SIGKILLed the gradlew process group. It recycles
	// the (potentially corrupt) Gradle daemon for the leaf — a forced
	// kill leaves the daemon (a separate process outside the group)
	// running with state the killed build may have corrupted, so spec
	// §144 requires recycling it rather than reusing it. Best-effort:
	// the error (if any) is logged to Stderr but does not fail the
	// forced-cancel path. Graceful cancellation (first signal) does NOT
	// invoke this — the warm daemon is preserved per spec.
	OnForcedCancel func(stderr io.Writer) error
}

// DefaultKillAfter is the documented graceful-cancellation deadline.
const DefaultKillAfter = 5 * time.Second

// forcedKillAfter is the collapsed graceful window when the caller forces
// cancellation (second signal): ~0 so descendants are SIGKILLed without
// waiting, recycling unsafe state. Non-zero only so the hard-stage
// goroutine still arms a timer (0 would block on the timer path).
const forcedKillAfter = 50 * time.Millisecond

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
		// The default launch applies the platform kernel sandbox via
		// sandboxrun.BuildChildArgv: the seam between "everything except
		// the kernel sandbox application" and the sandbox itself. Tests
		// replace it with NoSandboxLauncher so every behavior except
		// kernel enforcement runs without applying a Seatbelt/bwrap
		// profile.
		launch = func(g *BuildGrants, innerArgv []string) ([]string, error) {
			return sandboxrun.BuildChildArgv(g.Grants, innerArgv)
		}
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

	// maxDurationCh fires when the build-duration ceiling elapses; nil
	// disables the ceiling. Treating it as a caller-style cancel keeps
	// the exit-code + marker contract identical to an explicit SIGINT.
	var maxDurationCh <-chan time.Time
	if opts.MaxDuration > 0 {
		maxDurationCh = time.After(opts.MaxDuration)
	}

	// forceCh collapses the graceful KillAfter window to ~0 when closed.
	// Wired from SignalContext's second-signal channel: the FIRST cancel
	// preserves the warm Gradle daemon (graceful SIGTERM); a FORCED
	// cancel SIGKILLs the process group to recycle unsafe state.
	forceCh := opts.ForceCancel

	cancelled := false
	childDone := false
	forced := false // set when a forced kill (forceCh) actually fired
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
	// stageKillCh closes when the staged-kill goroutine actually delivers
	// a FORCED SIGKILL (forceCh fired). RunBuild consults it after the
	// child is reaped to decide whether to recycle the daemon (S3).
	var stageKillCh <-chan struct{}
	// dispatchCancel handles both cancel triggers identically (caller
	// signal OR the build-duration ceiling): once-only, audit the trigger,
	// then SIGTERM the group and stage the hard kill. The staged kill
	// honors forceCh: a forced cancel during the teardown collapses the
	// window and reports via stageKillCh so RunBuild recycles the daemon
	// (S3, P1). trigger is the audit reason ("sigterm" / "max-duration").
	dispatchCancel := func(trigger string) {
		if cancelled {
			return
		}
		cancelled = true
		auditor.Emit(audit.ControlMutation("build.cancel", opts.Resolved.Worktree, trigger))
		stageKillCh = stageKill(pgid, killAfter, forceCh, sigGroup, childReaped)
	}
	for {
		if opts.Cancel == nil {
			err := <-waitErr
			code := mapWaitErr(err)
			emitExit(auditor, code, started)
			return code, nil
		}
		if childDone {
			// S3: a forced cancel recycled the gradlew group; also
			// recycle the (potentially corrupt) daemon. Best-effort:
			// a --stop failure is logged but does not fail the
			// forced-cancel path. Graceful cancel preserves the daemon.
			if forced && opts.OnForcedCancel != nil {
				if err := opts.OnForcedCancel(stderr); err != nil {
					fmt.Fprintf(stderr, "omac build: warning: daemon recycle after forced cancel failed: %v\n", err)
				}
			}
			return takeResult(childErr)
		}
		select {
		case err := <-waitErr:
			childDone = true
			childErr = err
			close(childReaped)
		case <-opts.Cancel:
			dispatchCancel("sigterm")
		case <-maxDurationCh:
			// Build-duration ceiling elapsed: cancel as if the caller
			// signalled. maxDurationCh is nil unless MaxDuration > 0.
			dispatchCancel("max-duration")
		case <-stageKillCh:
			// The staged-kill goroutine delivered a FORCED SIGKILL
			// (forceCh fired). Mark forced so the daemon is recycled
			// after the child is reaped (S3).
			forced = true
		}
	}
}

// stageKill delivers SIGTERM to the process group, then stages a hard
// SIGKILL after killAfter — but only while the child is unreaped. Once
// Wait has returned the child pid is back in the pool, so kill(-pgid,
// SIGKILL) could hit an unrelated process group that recycled the pgid;
// skipping it is also correct because a reaped child needs no kill.
//
// It honors forceCh: a closed forceCh collapses the graceful window to
// forcedKillAfter so a second signal tears down descendants immediately,
// recycling unsafe state (S3). It returns a channel that closes when the
// FORCED SIGKILL is delivered (forceCh fired), so RunBuild can recycle
// the daemon afterwards; the channel never closes for a graceful
// (timer-driven) kill.
//
// Extracted (P1) so the Cancel and MaxDuration arms share identical
// staging instead of duplicating the goroutine.
func stageKill(pgid int, killAfter time.Duration, forceCh <-chan struct{}, sigGroup func(int, syscall.Signal) error, childReaped <-chan struct{}) <-chan struct{} {
	forcedCh := make(chan struct{})
	_ = sigGroup(-pgid, syscall.SIGTERM)
	go func() {
		timer := time.NewTimer(killAfter)
		defer timer.Stop()
		select {
		case <-childReaped:
			return
		case <-timer.C:
			// Graceful deadline elapsed: SIGKILL the group. NOT a forced
			// cancel, so forcedCh stays open (daemon preserved per spec).
			_ = sigGroup(-pgid, syscall.SIGKILL)
		case <-forceCh:
			// Forced cancel: collapse the graceful window and SIGKILL
			// immediately. Signal RunBuild to recycle the daemon (S3).
			_ = sigGroup(-pgid, syscall.SIGKILL)
			close(forcedCh)
		}
	}()
	return forcedCh
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
// SIGTERM delivered to this process, a force channel closed on the SECOND
// signal (collapsing RunBuild's graceful KillAfter window to ~0 so
// descendants are SIGKILLed immediately — forced cancellation tears down
// and recycles unsafe state, while the first signal preserves the warm
// Gradle daemon), a drill-through channel that tests use to inject
// signals without touching the real disposition, and a release func
// restoring the default disposition. The CLI wires cancel + force to
// RunBuild so a harness interrupting omac cancels the build through the
// staged graceful-then-kill path rather than orphaning the executor.
//
// A second received signal is FATAL to the process, but NOT via a raw
// os.Exit: os.Exit skips deferred functions, so the previous
// implementation leaked the build's private temp (+ the whole CLI defer
// chain: audit close, cache-scope lock release). Instead the second
// signal closes the force channel; RunBuild collapses the graceful window
// to forcedKillAfter itself, letting its normal return path (and every
// deferred cleanup above it) run to completion before returning
// ExitCancelled.
func SignalContext() (cancel <-chan struct{}, force <-chan struct{}, second chan<- os.Signal, release func()) {
	cancelCh := make(chan struct{})
	forceCh := make(chan struct{})
	// Drill channel: writes delivered to the same goroutine signal.Notify
	// feeds. Buffered so a test can inject two signals without blocking
	// before the watcher starts.
	drill := make(chan os.Signal, 2)
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
		case <-drill:
		}
		close(cancelCh)
		// Second signal: do NOT os.Exit — close the force channel so
		// RunBuild collapses the graceful window, then unwind
		// through the normal cancel path so deferred cleanup
		// (CleanupTmp, audit close) still runs.
		select {
		case <-sigCh:
		case <-drill:
		}
		close(forceCh)
	}()
	return cancelCh, forceCh, drill, func() { signal.Stop(sigCh) }
}
