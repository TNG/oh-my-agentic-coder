package buildengine

import (
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildcontrol"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
	"github.com/tngtech/oh-my-agentic-coder/internal/procidentity"
)

// StopBrokeredOptions bundles the engine inputs for one brokered
// `omac build stop` invocation. It is a DISTINCT engine operation from
// the direct-host Stop (which runs the repo wrapper): the brokered stop
// does NOT execute the repository wrapper, does NOT apply a
// speculative relaxed profile, and does NOT remove the lockfile
// (spec.md §240). It uses the host-only ownership records
// (buildcontrol.DaemonRecord) + procidentity-verified process control
// to identify leaf-associated Gradle daemons and request termination.
//
// The shape mirrors StopOptions (the direct-host op) for the fields
// both share (Workdir, RawArgs, Stdout, Stderr, CacheDir, CacheRoot,
// CloseScope, Auditor); the brokered path adds Cancel (the broker's
// graceful signal) and omits the wrapper-execution-specific fields.
// ForceCancel is intentionally absent — the brokered stop is already
// bounded (DefaultStopForceKillAfter bounds the SIGTERM→SIGKILL
// escalation); the broker has no force-kill escalation for the stop
// op itself (a force signal cancels the stop the same as a graceful
// one — there is no separate escalation stage for a manual stop).
type StopBrokeredOptions struct {
	// Workdir is the canonical worktree root.
	Workdir string
	// RawArgs are the arguments AFTER `omac build stop` (typically
	// `--root <rel>` or empty). The broker strips the leading "stop"
	// token before passing them here (the broker receives args after
	// `omac build`, so `args[0]=="stop"` is the stop subcommand).
	RawArgs []string
	// Stdout/Stderr receive the stop's output. The brokered stop
	// streams nothing to stdout (no build output); a short diagnostic
	// is written to Stderr on a service_failure.
	Stdout io.Writer
	// Stderr must be non-nil.
	Stderr io.Writer
	// CacheDir is the resolved OMAC cache scope dir.
	CacheDir string
	// CacheRoot is the shared cache root (parent of cache-scope dirs)
	// under which the host-only build-control root lives. The
	// ownership record is read at
	// buildcontrol.DaemonPath(cacheRoot, canonicalLeaf). Empty falls
	// back to the legacy in-leaf lock (behavior-preserving; the
	// ownership records live under the build-control root so an empty
	// CacheRoot means no ownership state → idempotent success).
	CacheRoot string
	// CloseScope releases the cache-scope lock; the engine defers it.
	CloseScope func()
	// Auditor receives the build.stop event; nil → audit.Nop().
	Auditor audit.Auditor
	// Cancel is the broker's graceful cancellation signal. The
	// brokered stop honors it by aborting the bounded SIGTERM wait
	// (and the leaf-lock acquire). There is no force signal for the
	// stop op itself.
	Cancel <-chan struct{}
}

// DefaultStopBrokeredForceKillAfter bounds the SIGTERM→SIGKILL
// escalation for the brokered stop. After requesting SIGTERM, the
// engine waits this long for the daemon to exit, then SIGKILLs a
// STILL-VERIFIED identity (re-verified immediately before the
// SIGKILL so a PID-reused process is never signalled). The value
// matches buildrun.DefaultStopForceKillAfter (10s) for consistency
// with the legacy cooperative-stop→force-kill deadline.
//
// stopBrokeredForceKillAfter is the package-level var the engine
// reads; tests swap it to shorten the bound (a 10s wait in a unit
// test is too long). Production leaves it at the default.
const DefaultStopBrokeredForceKillAfter = buildrun.DefaultStopForceKillAfter

// stopBrokeredForceKillAfter is the bound StopBrokered uses for the
// SIGTERM→SIGKILL escalation. It is a var (not the const) so tests
// can override it; production code does not change it.
var stopBrokeredForceKillAfter = DefaultStopBrokeredForceKillAfter

// stopBrokeredVerify is the procidentity.Verify seam used by
// StopBrokered. Package-level var (not an unexported function) so
// tests swap it without spawning real processes. Production wires
// procidentity.Verify (the same seam the handshake and reconciliation
// use). The signature mirrors procidentity.Verify exactly so the
// production wiring is a direct assignment.
var stopBrokeredVerify = procidentity.Verify

// stopBrokeredKill is the syscall.Kill seam used by StopBrokered.
// Package-level var so tests can inject a recorder and assert which
// signal (SIGTERM/SIGKILL) was delivered to which PID without sending
// real signals. Production wires syscall.Kill.
var stopBrokeredKill = syscall.Kill

// StopBrokered executes one brokered `omac build stop` invocation
// (ticket 07, spec.md §240). It is a DISTINCT engine operation from
// the direct-host Stop: it does NOT execute the repository wrapper,
// does NOT apply a speculative relaxed profile, and does NOT remove
// the lockfile. It uses the host-only ownership records
// (buildcontrol.DaemonRecord) + procidentity-verified process control
// to identify leaf-associated Gradle daemons, request termination
// (SIGTERM), wait a bounded interval, and force (SIGKILL) ONLY
// still-verified process identities.
//
// Brokered-stop state machine (spec.md §240):
//
//   - no ownership record AND no live daemon → success (exit 0),
//     idempotent. The lockfile is not touched; nothing is signalled.
//   - pending record → service_failure (exit 10), signal nothing. A
//     pending record means "the leaf indicates a possible daemon" (a
//     build is in flight or crashed mid-handshake) but no process can
//     be verified (a pending record carries no PID). The record is
//     LEFT in place — Phase 5's parent-startup reconciliation retires
//     stale pending records at the next startup; a concurrent in-flight
//     build owns the pending record and must not see it vanish from
//     under it. Sanitized diagnostic to stderr.
//   - active record + verified (live, executable matches, main class
//     matches, start identity unchanged) → request SIGTERM, bounded
//     wait, SIGKILL if still-verified after the bound, retire the
//     record on confirmed exit → success (exit 0).
//   - active record + alive but unverifiable (executable changed / main
//     class missing / start identity changed = PID reused) →
//     service_failure (exit 10), signal NOTHING (a reused PID is an
//     unrelated process). Retire the record (it is stale). Sanitized
//     diagnostic to stderr.
//   - active record + dead (ErrNoSuchProcess) → retire the record,
//     success (exit 0) — idempotent, the daemon is already gone.
//   - active record + ErrUnverifiable → service_failure (exit 10),
//     signal nothing. Leave the record (it will be reconciled at the
//     next parent startup, or block the leaf — fail closed).
//
// The lockfile is NEVER removed (spec.md §231: "omac build stop
// therefore no longer removes the lockfile"). The leaf lock is
// acquired (spec.md §240: "It acquires the same leaf lock") and
// released on return; the persistent lockfile is reused by the next
// Acquire.
//
// The brokered stop does NOT execute the repo wrapper, so a malformed
// worktree (no gradlew, bad --root) is a policy_denial surfaced by
// the parseStopArgs / Resolve step — same as the direct-host Stop.
// A worktree-authorization denial is handled by the broker BEFORE the
// invoker runs, so it never reaches StopBrokered.
func StopBrokered(opts StopBrokeredOptions) Result {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	deny := func(err error) Result {
		return Result{Class: ClassPolicyDenial, Exit: 3, Err: err}
	}
	failService := func(format string, args ...any) Result {
		return Result{Class: ClassServiceFailure, Exit: 10, Err: fmt.Errorf(format, args...)}
	}

	// Parse --root from the args after `omac build stop` (the broker
	// stripped the leading "stop"). Same grammar as the direct-host
	// Stop: `omac build stop [--root <rel>]`.
	root, perr := parseStopArgs(opts.RawArgs)
	if perr != nil {
		return deny(perr)
	}

	// Resolve the worktree + leaf. The brokered stop needs the
	// canonical leaf to key the ownership record lookup; it does NOT
	// run the wrapper, but Resolve validates the --root + worktree
	// shape (a malformed --root or a worktree with no gradlew is a
	// policy denial, same as the direct path).
	stopArgs := []string{"--root", root, "--", "gradle", "--stop"}
	req, err := buildrun.ParseArgs(stopArgs)
	if err != nil {
		return deny(err)
	}
	resolved, err := buildrun.Resolve(opts.Workdir, req)
	if err != nil {
		return deny(err)
	}

	if opts.CloseScope != nil {
		defer opts.CloseScope()
	}

	leaf := buildrun.GradleLeaf(opts.CacheDir)
	auditor := opts.Auditor
	if auditor == nil {
		auditor = audit.Nop()
	}
	auditor.Emit(audit.ControlMutation("build.stop", resolved.Worktree, "brokered verified stop"))

	// Acquire the SAME leaf lock the build acquires (spec.md §240:
	// "It acquires the same leaf lock"). The lock prevents a
	// concurrent build from re-arming a pending record while the stop
	// reads + retires the active record. Cancel is the broker's
	// graceful signal; a lock-acquire cancelled by the broker returns
	// ClassCancelled.
	lock, err := acquireLeafLock(opts.CacheRoot, leaf, opts.Cancel)
	if err != nil {
		if errors.Is(err, buildcontrol.ErrLockCancelled) || errors.Is(err, buildrun.ErrLockCancelled) {
			fmt.Fprintln(stderr, buildrun.CancelledMarker)
			return Result{Class: ClassCancelled, Exit: 4}
		}
		return failService("acquire leaf lock: %v", err)
	}
	defer lock.Release()

	// No build-control root → no ownership state → idempotent success
	// (spec.md §240: "if neither ownership state nor a live daemon is
	// present, stop succeeds idempotently"). The legacy in-leaf path
	// has no ownership records; the brokered stop is a no-op there.
	if opts.CacheRoot == "" {
		fmt.Fprintln(stdout, "omac build stop: no daemon ownership state (idempotent)")
		return Result{Class: ClassSuccess, Exit: 0}
	}

	// Load the ownership record for this leaf. ErrNoDaemonRecord →
	// idempotent success. A malformed record → service_failure (the
	// host's trusted state was corrupted; the caller surfaces a
	// sanitized diagnostic — the broker redacts /build-control/ paths).
	rec, err := buildcontrol.LoadDaemonRecord(opts.CacheRoot, leaf)
	if err != nil {
		if errors.Is(err, buildcontrol.ErrNoDaemonRecord) {
			fmt.Fprintln(stdout, "omac build stop: no daemon record (idempotent)")
			return Result{Class: ClassSuccess, Exit: 0}
		}
		return failService("load daemon record: %v", err)
	}

	switch rec.State {
	case buildcontrol.DaemonStatePending:
		// A pending record means "the leaf indicates a possible
		// daemon" (a build is in flight or crashed mid-handshake) but
		// no process can be verified (a pending record carries no
		// PID). Per spec.md §240: "If the leaf indicates a possible
		// daemon but no process can be verified, stop returns a
		// sanitized service failure and signals nothing." Leave the
		// record in place — a concurrent in-flight build owns the
		// pending record and must not see it vanish; Phase 5's
		// parent-startup reconciliation retires stale pending records
		// at the next startup.
		fmt.Fprintln(stderr, "omac build stop: daemon ownership pending (no verifiable process)")
		return Result{Class: ClassServiceFailure, Exit: 10, Err: errors.New("daemon ownership pending; no process can be verified")}

	case buildcontrol.DaemonStateRetired:
		// A retired-but-not-yet-deleted tombstone (crash between the
		// atomic write and the unlink in a previous retire). Treat as
		// no-owner: idempotent success. The next reconciliation
		// cleans up the tombstone.
		fmt.Fprintln(stdout, "omac build stop: daemon record retired (idempotent)")
		return Result{Class: ClassSuccess, Exit: 0}

	case buildcontrol.DaemonStateActive:
		return stopBrokeredActive(opts.CacheRoot, leaf, rec, stderr, stdout)

	default:
		// Unknown state: fail closed (service_failure) and retire the
		// malformed record so the leaf unblocks.
		_ = buildcontrol.RetireDaemonRecord(opts.CacheRoot, leaf)
		return failService("unknown daemon record state %q", rec.State)
	}
}

// stopBrokeredActive handles the active-record branch of the brokered
// stop. It re-verifies the recorded PID via procidentity.Verify using
// the recorded StartIdentity (the re-verify path, distinct from the
// handshake's expectedStart=""), then:
//
//   - verified → SIGTERM, bounded wait, SIGKILL if still-verified,
//     retire on confirmed exit → success.
//   - alive but unverifiable (PID reused / executable changed / start
//     identity changed) → service_failure, signal nothing, retire the
//     stale record.
//   - dead (ErrNoSuchProcess) → retire, success (idempotent).
//   - ErrUnverifiable → service_failure, signal nothing, leave the
//     record (fail closed — reconcile at next startup).
func stopBrokeredActive(cacheRoot, leaf string, rec buildcontrol.DaemonRecord, stderr, stdout io.Writer) Result {
	// Re-verify the recorded process using the recorded StartIdentity.
	// This is the re-verify path (procidentity.Verify with a non-empty
	// expectedStart), distinct from the handshake's expectedStart=""
	// (the handshake captures the start identity; the stop compares
	// against it to detect PID reuse).
	verified, _, err := stopBrokeredVerify(rec.PID, rec.JDKExecutable, rec.StartIdentity)
	if err != nil {
		if errors.Is(err, procidentity.ErrNoSuchProcess) {
			// Dead: retire the record, idempotent success.
			if rerr := buildcontrol.RetireDaemonRecord(cacheRoot, leaf); rerr != nil {
				fmt.Fprintf(stderr, "omac build stop: warning: retire dead daemon record: %v\n", rerr)
			}
			fmt.Fprintln(stdout, "omac build stop: daemon already gone (idempotent)")
			return Result{Class: ClassSuccess, Exit: 0}
		}
		if errors.Is(err, procidentity.ErrUnverifiable) {
			// Unverifiable: fail closed, signal nothing, leave the
			// record. Reconciliation at next startup decides.
			fmt.Fprintln(stderr, "omac build stop: daemon identity unverifiable (signal nothing, fail closed)")
			return Result{Class: ClassServiceFailure, Exit: 10, Err: errors.New("daemon identity unverifiable; stop signals nothing")}
		}
		// Other verify error: service_failure, signal nothing.
		fmt.Fprintf(stderr, "omac build stop: verify daemon: %v\n", err)
		return Result{Class: ClassServiceFailure, Exit: 10, Err: fmt.Errorf("verify daemon: %v", err)}
	}
	if !verified {
		// Alive but does not match (PID reused / executable changed /
		// start identity changed). Signal NOTHING — a reused PID is
		// an unrelated process. Retire the stale record so the leaf
		// unblocks at the next build.
		if rerr := buildcontrol.RetireDaemonRecord(cacheRoot, leaf); rerr != nil {
			fmt.Fprintf(stderr, "omac build stop: warning: retire stale daemon record: %v\n", rerr)
		}
		fmt.Fprintln(stderr, "omac build stop: daemon identity could not be verified (PID reused or executable changed)")
		return Result{Class: ClassServiceFailure, Exit: 10, Err: errors.New("daemon identity could not be verified; stop signals nothing")}
	}

	// Verified: request SIGTERM, bounded wait, SIGKILL if
	// still-verified after the bound, retire on confirmed exit.
	if err := stopBrokeredKill(rec.PID, syscall.SIGTERM); err != nil {
		// The verify just succeeded, so a SIGTERM failure is most
		// likely a race (process exited between verify and kill).
		// Re-check: if dead, retire + idempotent success; otherwise
		// service_failure.
		if _, _, e2 := stopBrokeredVerify(rec.PID, rec.JDKExecutable, rec.StartIdentity); errors.Is(e2, procidentity.ErrNoSuchProcess) {
			_ = buildcontrol.RetireDaemonRecord(cacheRoot, leaf)
			fmt.Fprintln(stdout, "omac build stop: daemon already gone (idempotent)")
			return Result{Class: ClassSuccess, Exit: 0}
		}
		return Result{Class: ClassServiceFailure, Exit: 10, Err: fmt.Errorf("signal daemon: %v", err)}
	}

	if exited := waitVerifiedExit(rec.PID, rec.JDKExecutable, rec.StartIdentity, stopBrokeredForceKillAfter, nil); exited {
		if rerr := buildcontrol.RetireDaemonRecord(cacheRoot, leaf); rerr != nil {
			fmt.Fprintf(stderr, "omac build stop: warning: retire daemon record: %v\n", rerr)
		}
		fmt.Fprintln(stdout, "omac build stop: stopped Gradle daemon")
		return Result{Class: ClassSuccess, Exit: 0}
	}

	// Still alive after the SIGTERM bound. Re-verify before SIGKILL so
	// a PID-reused process (which would have a different start
	// identity) is NEVER signalled. If the re-verify fails, retire
	// the stale record and return service_failure (signal nothing for
	// the reused PID). Only a STILL-VERIFIED identity is SIGKILLed.
	verifiedKill, _, kerr := stopBrokeredVerify(rec.PID, rec.JDKExecutable, rec.StartIdentity)
	if kerr != nil {
		if errors.Is(kerr, procidentity.ErrNoSuchProcess) {
			_ = buildcontrol.RetireDaemonRecord(cacheRoot, leaf)
			fmt.Fprintln(stdout, "omac build stop: daemon exited during SIGTERM wait (idempotent)")
			return Result{Class: ClassSuccess, Exit: 0}
		}
		// Unverifiable or other: fail closed, signal nothing.
		fmt.Fprintln(stderr, "omac build stop: daemon identity unverifiable before SIGKILL (signal nothing)")
		return Result{Class: ClassServiceFailure, Exit: 10, Err: errors.New("daemon identity unverifiable before SIGKILL")}
	}
	if !verifiedKill {
		// PID reused between the SIGTERM and the re-verify. Do NOT
		// SIGKILL the reused PID. Retire the stale record, return
		// service_failure.
		_ = buildcontrol.RetireDaemonRecord(cacheRoot, leaf)
		fmt.Fprintln(stderr, "omac build stop: daemon identity changed during SIGTERM wait (PID reused, no SIGKILL)")
		return Result{Class: ClassServiceFailure, Exit: 10, Err: errors.New("daemon identity changed during SIGTERM wait; no SIGKILL signalled")}
	}
	if err := stopBrokeredKill(rec.PID, syscall.SIGKILL); err != nil {
		// Best-effort: the SIGKILL failed (process may have just
		// exited). Re-check; if dead, success; otherwise service_failure.
		if _, _, e2 := stopBrokeredVerify(rec.PID, rec.JDKExecutable, rec.StartIdentity); errors.Is(e2, procidentity.ErrNoSuchProcess) {
			_ = buildcontrol.RetireDaemonRecord(cacheRoot, leaf)
			fmt.Fprintln(stdout, "omac build stop: daemon exited (idempotent)")
			return Result{Class: ClassSuccess, Exit: 0}
		}
		return Result{Class: ClassServiceFailure, Exit: 10, Err: fmt.Errorf("SIGKILL daemon: %v", err)}
	}
	// Wait for the SIGKILL'd process to be reaped (re-verify until
	// ErrNoSuchProcess). Bounded by the same deadline; a wedged kernel
	// reap is a service_failure.
	if exited := waitVerifiedExit(rec.PID, rec.JDKExecutable, rec.StartIdentity, stopBrokeredForceKillAfter, nil); exited {
		if rerr := buildcontrol.RetireDaemonRecord(cacheRoot, leaf); rerr != nil {
			fmt.Fprintf(stderr, "omac build stop: warning: retire daemon record: %v\n", rerr)
		}
		fmt.Fprintln(stdout, "omac build stop: stopped Gradle daemon (SIGKILL)")
		return Result{Class: ClassSuccess, Exit: 0}
	}
	return Result{Class: ClassServiceFailure, Exit: 10, Err: errors.New("daemon did not exit after SIGKILL")}
}

// waitVerifiedExit polls procidentity.Verify for the process to exit
// (ErrNoSuchProcess) up to the bound. Returns true if the process
// exited within the bound, false on timeout. An optional cancel
// channel aborts the wait early (the broker's graceful signal). A
// poll interval of 100ms balances responsiveness against syscall load
// (the cooperative stop is normally sub-second).
func waitVerifiedExit(pid int, expectedJDK, expectedStart string, bound time.Duration, cancel <-chan struct{}) bool {
	deadline := time.Now().Add(bound)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_, _, err := stopBrokeredVerify(pid, expectedJDK, expectedStart)
			if errors.Is(err, procidentity.ErrNoSuchProcess) {
				return true
			}
			// ErrUnverifiable or other: keep waiting (the process is
			// likely still exiting; a transient /proc race is
			// possible on Linux). The final post-SIGKILL re-check
			// decides.
			if time.Now().After(deadline) {
				return false
			}
		case <-cancel:
			return false
		}
	}
}
