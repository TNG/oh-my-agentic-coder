package buildengine

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildcontrol"
	"github.com/tngtech/oh-my-agentic-coder/internal/procidentity"
)

// stopBrokeredTestEnv builds a worktree + cache dir + short cacheRoot
// for a brokered stop test. The worktree has a stub gradlew (the
// brokered stop does NOT run it, but Resolve validates its presence).
// Returns (worktree, cacheDir, closeScope, cacheRoot, leaf).
func stopBrokeredTestEnv(t *testing.T) (worktree, cacheDir string, closeScope func(), cacheRoot, leaf string) {
	t.Helper()
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "gradlew"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cd, cs, err := prepareTestCacheScope(wt)
	if err != nil {
		t.Fatalf("prepare cache scope: %v", err)
	}
	leafDir := filepath.Join(cd, "gradle")
	if err := os.MkdirAll(leafDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(leafDir, "init.d"), 0o755) })
	root, err := os.MkdirTemp("/tmp", "omac-eng-stop-brokered")
	if err != nil {
		t.Fatalf("create short cache root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return wt, cd, cs, root, leafDir
}

// withStopBrokeredSeams swaps the package-level procidentity.Verify and
// syscall.Kill seams to the supplied fakes for the duration of the
// test and restores them on cleanup. Returns the kill recorder so the
// test can assert which signal was delivered to which PID.
func withStopBrokeredSeams(t *testing.T, verify func(pid int, exe, start string) (bool, procidentity.Identity, error), killRecorder *stopKillRecorder) {
	t.Helper()
	prevVerify := stopBrokeredVerify
	prevKill := stopBrokeredKill
	stopBrokeredVerify = verify
	stopBrokeredKill = killRecorder.kill
	t.Cleanup(func() {
		stopBrokeredVerify = prevVerify
		stopBrokeredKill = prevKill
	})
}

// stopKillRecorder records every signal delivered via the kill seam.
type stopKillRecorder struct {
	mu      sync.Mutex
	signals []stopKillRecord
}

type stopKillRecord struct {
	PID int
	Sig syscall.Signal
}

func (r *stopKillRecorder) kill(pid int, sig syscall.Signal) error {
	r.mu.Lock()
	r.signals = append(r.signals, stopKillRecord{PID: pid, Sig: sig})
	r.mu.Unlock()
	// Pretend the signal was delivered. The verify seam controls
	// whether the process is "alive" for the subsequent poll.
	return nil
}

func (r *stopKillRecorder) signalsOf() []stopKillRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]stopKillRecord, len(r.signals))
	copy(out, r.signals)
	return out
}

// makeVerifyFake builds a procidentity.Verify seam that:
//   - returns ErrNoSuchProcess once deadAfter SIGTERM/SIGKILL calls
//     have been made (simulating the process exiting after a signal);
//   - returns (true, Identity{StartIdentity: start}, nil) while the
//     process is "alive" and the caller is in the verified branch;
//   - returns (false, Identity{}, nil) for the alive-unverified
//     branch (PID reused / executable changed);
//   - returns ErrUnverifiable for the unverifiable branch.
type verifyFakeMode int

const (
	verifyFakeVerified verifyFakeMode = iota
	verifyFakeAliveUnverified
	verifyFakeUnverifiable
	verifyFakeDeadImmediately
)

// makeVerifyFake returns a verify seam that, when called, returns the
// configured mode. For the verified mode, it counts SIGTERM/SIGKILL
// deliveries via the kill recorder and switches to ErrNoSuchProcess
// once the requested signal has been delivered (simulating cooperative
// exit after SIGTERM, or kernel reaping after SIGKILL).
func makeVerifyFake(mode verifyFakeMode, killRec *stopKillRecorder, exitAfterSig syscall.Signal) func(pid int, exe, start string) (bool, procidentity.Identity, error) {
	return func(pid int, exe, start string) (bool, procidentity.Identity, error) {
		switch mode {
		case verifyFakeDeadImmediately:
			return false, procidentity.Identity{}, procidentity.ErrNoSuchProcess
		case verifyFakeUnverifiable:
			return false, procidentity.Identity{}, procidentity.ErrUnverifiable
		case verifyFakeAliveUnverified:
			return false, procidentity.Identity{Executable: exe, MainClass: "other"}, nil
		case verifyFakeVerified:
			// If the requested signal has been delivered, the process
			// has exited.
			for _, s := range killRec.signalsOf() {
				if s.PID == pid && s.Sig == exitAfterSig {
					return false, procidentity.Identity{}, procidentity.ErrNoSuchProcess
				}
			}
			return true, procidentity.Identity{Executable: exe, MainClass: procidentity.GradleDaemonMainClass, StartIdentity: start}, nil
		}
		return false, procidentity.Identity{}, procidentity.ErrNoSuchProcess
	}
}

// writeActiveRecord writes an active DaemonRecord for the leaf under
// cacheRoot with the given PID + JDKExecutable + StartIdentity.
func writeActiveRecord(t *testing.T, cacheRoot, leaf string, pid int, jdkExe, startID string) {
	t.Helper()
	if err := buildcontrol.WritePendingDaemonRecord(cacheRoot, leaf, buildcontrol.DaemonRecord{
		State:         buildcontrol.DaemonStatePending,
		Marker:        "test-marker",
		LeafDigest:    buildcontrol.HashLeaf(leaf),
		JDKExecutable: jdkExe,
		RequestID:     "test-req",
	}); err != nil {
		t.Fatalf("write pending: %v", err)
	}
	if err := buildcontrol.PromoteDaemonRecord(cacheRoot, leaf, pid, startID); err != nil {
		t.Fatalf("promote: %v", err)
	}
}

// TestStopBrokered_NoRecord_IdempotentSuccess asserts that when no
// ownership record exists, the brokered stop succeeds idempotently
// (exit 0) without signalling anything.
func TestStopBrokered_NoRecord_IdempotentSuccess(t *testing.T) {
	wt, cd, cs, cr, leaf := stopBrokeredTestEnv(t)
	defer cs()
	killRec := &stopKillRecorder{}
	withStopBrokeredSeams(t, func(pid int, exe, start string) (bool, procidentity.Identity, error) {
		t.Errorf("verify must not be called when no record exists")
		return false, procidentity.Identity{}, procidentity.ErrNoSuchProcess
	}, killRec)

	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   nil,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassSuccess {
		t.Errorf("class = %q, want %q (idempotent success)", res.Class, ClassSuccess)
	}
	if res.ExitCode() != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode())
	}
	if len(killRec.signalsOf()) != 0 {
		t.Errorf("no signal expected when no record exists; got %v", killRec.signalsOf())
	}
	// The record file must NOT have been created.
	if _, err := buildcontrol.LoadDaemonRecord(cr, leaf); !errors.Is(err, buildcontrol.ErrNoDaemonRecord) {
		t.Errorf("record file should not exist after idempotent stop; err=%v", err)
	}
}

// TestStopBrokered_NoCacheRoot_IdempotentSuccess asserts that an empty
// CacheRoot (the legacy in-leaf path with no ownership state) is an
// idempotent success without signalling anything.
func TestStopBrokered_NoCacheRoot_IdempotentSuccess(t *testing.T) {
	wt, cd, cs, _, _ := stopBrokeredTestEnv(t)
	defer cs()
	killRec := &stopKillRecorder{}
	withStopBrokeredSeams(t, func(pid int, exe, start string) (bool, procidentity.Identity, error) {
		t.Errorf("verify must not be called when CacheRoot is empty")
		return false, procidentity.Identity{}, procidentity.ErrNoSuchProcess
	}, killRec)

	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   nil,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: "",
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassSuccess {
		t.Errorf("class = %q, want %q (idempotent success)", res.Class, ClassSuccess)
	}
	if len(killRec.signalsOf()) != 0 {
		t.Errorf("no signal expected; got %v", killRec.signalsOf())
	}
}

// TestStopBrokered_Pending_ServiceFailureSignalNothing asserts a
// pending record (a build in flight or crashed mid-handshake) returns
// a sanitized service_failure and signals NOTHING. The record is LEFT
// in place (a concurrent in-flight build owns it; Phase 5 reconciles
// stale pending records at startup).
func TestStopBrokered_Pending_ServiceFailureSignalNothing(t *testing.T) {
	wt, cd, cs, cr, leaf := stopBrokeredTestEnv(t)
	defer cs()
	if err := buildcontrol.WritePendingDaemonRecord(cr, leaf, buildcontrol.DaemonRecord{
		State:         buildcontrol.DaemonStatePending,
		Marker:        "test-marker",
		LeafDigest:    buildcontrol.HashLeaf(leaf),
		JDKExecutable: "/path/to/java",
		RequestID:     "test-req",
	}); err != nil {
		t.Fatalf("write pending: %v", err)
	}
	killRec := &stopKillRecorder{}
	withStopBrokeredSeams(t, func(pid int, exe, start string) (bool, procidentity.Identity, error) {
		t.Errorf("verify must not be called for a pending record (no PID)")
		return false, procidentity.Identity{}, procidentity.ErrNoSuchProcess
	}, killRec)

	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   nil,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassServiceFailure {
		t.Errorf("class = %q, want %q", res.Class, ClassServiceFailure)
	}
	if res.ExitCode() != 10 {
		t.Errorf("ExitCode = %d, want 10", res.ExitCode())
	}
	if len(killRec.signalsOf()) != 0 {
		t.Errorf("no signal expected for pending record; got %v", killRec.signalsOf())
	}
	// The pending record must STILL exist (left in place for
	// reconciliation / the concurrent in-flight build).
	rec, err := buildcontrol.LoadDaemonRecord(cr, leaf)
	if err != nil {
		t.Fatalf("record vanished: %v", err)
	}
	if rec.State != buildcontrol.DaemonStatePending {
		t.Errorf("record state = %q, want %q (must be left in place)", rec.State, buildcontrol.DaemonStatePending)
	}
}

// TestStopBrokered_ActiveVerified_SIGTERMThenRetire_Success asserts the
// happy path: an active record with a verified process is SIGTERM'd,
// the process exits, the record is retired (deleted), and the result
// is success.
func TestStopBrokered_ActiveVerified_SIGTERMThenRetire_Success(t *testing.T) {
	wt, cd, cs, cr, leaf := stopBrokeredTestEnv(t)
	defer cs()
	const pid = 4242
	const jdkExe = "/path/to/java"
	const startID = "start-id-123"
	writeActiveRecord(t, cr, leaf, pid, jdkExe, startID)

	killRec := &stopKillRecorder{}
	// The fake reports the process as verified; once a SIGTERM is
	// delivered, subsequent verify calls return ErrNoSuchProcess
	// (simulating cooperative exit after SIGTERM).
	withStopBrokeredSeams(t, makeVerifyFake(verifyFakeVerified, killRec, syscall.SIGTERM), killRec)

	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   nil,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassSuccess {
		t.Errorf("class = %q, want %q", res.Class, ClassSuccess)
	}
	if res.ExitCode() != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode())
	}
	sigs := killRec.signalsOf()
	if len(sigs) != 1 {
		t.Fatalf("expected exactly 1 signal (SIGTERM), got %v", sigs)
	}
	if sigs[0].PID != pid || sigs[0].Sig != syscall.SIGTERM {
		t.Errorf("signal = {%d, %v}, want {%d, SIGTERM}", sigs[0].PID, sigs[0].Sig, pid)
	}
	// The record must be retired (deleted) on confirmed exit.
	if _, err := buildcontrol.LoadDaemonRecord(cr, leaf); !errors.Is(err, buildcontrol.ErrNoDaemonRecord) {
		t.Errorf("record should be retired (deleted); err = %v", err)
	}
}

// TestStopBrokered_ActiveVerified_Wedged_SIGKILLAfterBound_Success
// asserts that when SIGTERM does not cause exit within the bound, the
// engine re-verifies and SIGKILLs the still-verified process. Uses the
// stopBrokeredForceKillAfter test seam to shorten the bound.
func TestStopBrokered_ActiveVerified_Wedged_SIGKILLAfterBound_Success(t *testing.T) {
	wt, cd, cs, cr, leaf := stopBrokeredTestEnv(t)
	defer cs()
	const pid = 4244
	const jdkExe = "/path/to/java"
	const startID = "start-id-789"
	writeActiveRecord(t, cr, leaf, pid, jdkExe, startID)

	killRec := &stopKillRecorder{}
	// The fake exits only on SIGKILL (SIGTERM ignored — wedged daemon).
	withStopBrokeredSeams(t, makeVerifyFake(verifyFakeVerified, killRec, syscall.SIGKILL), killRec)
	prevBound := stopBrokeredForceKillAfter
	stopBrokeredForceKillAfter = 300 * time.Millisecond
	t.Cleanup(func() { stopBrokeredForceKillAfter = prevBound })

	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   nil,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassSuccess {
		t.Errorf("class = %q, want %q", res.Class, ClassSuccess)
	}
	if res.ExitCode() != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode())
	}
	sigs := killRec.signalsOf()
	if len(sigs) != 2 {
		t.Fatalf("expected 2 signals (SIGTERM then SIGKILL), got %v", sigs)
	}
	if sigs[0].Sig != syscall.SIGTERM || sigs[1].Sig != syscall.SIGKILL {
		t.Errorf("signal order = %v, want [SIGTERM, SIGKILL]", sigs)
	}
	if _, err := buildcontrol.LoadDaemonRecord(cr, leaf); !errors.Is(err, buildcontrol.ErrNoDaemonRecord) {
		t.Errorf("record should be retired (deleted); err = %v", err)
	}
}

// TestStopBrokered_ActiveAliveUnverified_ServiceFailureSignalNothing
// asserts that an active record whose process is alive but does NOT
// verify (PID reused / executable changed / start identity changed)
// returns a sanitized service_failure and signals NOTHING. The stale
// record is retired so the leaf unblocks.
func TestStopBrokered_ActiveAliveUnverified_ServiceFailureSignalNothing(t *testing.T) {
	wt, cd, cs, cr, leaf := stopBrokeredTestEnv(t)
	defer cs()
	const pid = 4245
	const jdkExe = "/path/to/java"
	const startID = "start-id-reused"
	writeActiveRecord(t, cr, leaf, pid, jdkExe, startID)

	killRec := &stopKillRecorder{}
	withStopBrokeredSeams(t, makeVerifyFake(verifyFakeAliveUnverified, killRec, 0), killRec)

	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   nil,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassServiceFailure {
		t.Errorf("class = %q, want %q", res.Class, ClassServiceFailure)
	}
	if res.ExitCode() != 10 {
		t.Errorf("ExitCode = %d, want 10", res.ExitCode())
	}
	if len(killRec.signalsOf()) != 0 {
		t.Errorf("no signal expected for reused PID; got %v", killRec.signalsOf())
	}
	// The stale record must be retired (deleted).
	if _, err := buildcontrol.LoadDaemonRecord(cr, leaf); !errors.Is(err, buildcontrol.ErrNoDaemonRecord) {
		t.Errorf("stale record should be retired; err = %v", err)
	}
}

// TestStopBrokered_ActiveDead_RetireSuccess asserts that an active
// record whose process is already dead (ErrNoSuchProcess) is retired
// and the stop succeeds idempotently.
func TestStopBrokered_ActiveDead_RetireSuccess(t *testing.T) {
	wt, cd, cs, cr, leaf := stopBrokeredTestEnv(t)
	defer cs()
	const pid = 4246
	const jdkExe = "/path/to/java"
	const startID = "start-id-dead"
	writeActiveRecord(t, cr, leaf, pid, jdkExe, startID)

	killRec := &stopKillRecorder{}
	withStopBrokeredSeams(t, makeVerifyFake(verifyFakeDeadImmediately, killRec, 0), killRec)

	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   nil,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassSuccess {
		t.Errorf("class = %q, want %q (idempotent success)", res.Class, ClassSuccess)
	}
	if res.ExitCode() != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode())
	}
	if len(killRec.signalsOf()) != 0 {
		t.Errorf("no signal expected for already-dead daemon; got %v", killRec.signalsOf())
	}
	if _, err := buildcontrol.LoadDaemonRecord(cr, leaf); !errors.Is(err, buildcontrol.ErrNoDaemonRecord) {
		t.Errorf("dead record should be retired; err = %v", err)
	}
}

// TestStopBrokered_ActiveUnverifiable_ServiceFailureSignalNothing
// asserts that an active record whose process cannot be verified
// (ErrUnverifiable — the platform cannot extract identity, e.g. a
// sandbox blocks /proc) returns a sanitized service_failure, signals
// NOTHING, and LEAVES the record (fail closed — reconciliation at
// next startup decides).
func TestStopBrokered_ActiveUnverifiable_ServiceFailureSignalNothing(t *testing.T) {
	wt, cd, cs, cr, leaf := stopBrokeredTestEnv(t)
	defer cs()
	const pid = 4247
	const jdkExe = "/path/to/java"
	const startID = "start-id-unver"
	writeActiveRecord(t, cr, leaf, pid, jdkExe, startID)

	killRec := &stopKillRecorder{}
	withStopBrokeredSeams(t, makeVerifyFake(verifyFakeUnverifiable, killRec, 0), killRec)

	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   nil,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassServiceFailure {
		t.Errorf("class = %q, want %q", res.Class, ClassServiceFailure)
	}
	if res.ExitCode() != 10 {
		t.Errorf("ExitCode = %d, want 10", res.ExitCode())
	}
	if len(killRec.signalsOf()) != 0 {
		t.Errorf("no signal expected for unverifiable process; got %v", killRec.signalsOf())
	}
	// The record must STILL exist (left in place — fail closed; Phase
	// 5 reconciliation decides at next startup).
	rec, err := buildcontrol.LoadDaemonRecord(cr, leaf)
	if err != nil {
		t.Fatalf("record vanished: %v", err)
	}
	if rec.State != buildcontrol.DaemonStateActive {
		t.Errorf("record state = %q, want %q (must be left in place)", rec.State, buildcontrol.DaemonStateActive)
	}
}

// TestStopBrokered_PolicyDenialOnBadRoot asserts a malformed --root is
// a policy_denial (exit 3), mirroring the direct-host Stop grammar.
func TestStopBrokered_PolicyDenialOnBadRoot(t *testing.T) {
	wt, cd, cs, cr, _ := stopBrokeredTestEnv(t)
	defer cs()
	killRec := &stopKillRecorder{}
	withStopBrokeredSeams(t, func(pid int, exe, start string) (bool, procidentity.Identity, error) {
		t.Errorf("verify must not be called on a policy denial")
		return false, procidentity.Identity{}, procidentity.ErrNoSuchProcess
	}, killRec)

	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   []string{"--bogus"},
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassPolicyDenial {
		t.Errorf("class = %q, want %q", res.Class, ClassPolicyDenial)
	}
	if res.ExitCode() != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode())
	}
}

// TestStopBrokered_SubdirWrapperBareStopSucceeds asserts bare `omac
// build stop` works in a repo whose Gradle wrapper lives in a
// subdirectory (yarp3's `backend/gradlew`). The brokered stop keys on
// the leaf + ownership record and never executes the repo wrapper, so
// a wrapper at `<worktree>/gradlew` is not required — the ownership
// record is the sole source of truth.
func TestStopBrokered_SubdirWrapperBareStopSucceeds(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	// Wrapper lives ONLY under backend/ — none at the worktree root.
	backend := filepath.Join(wt, "backend")
	if err := os.MkdirAll(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backend, "gradlew"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cd, cs, err := prepareTestCacheScope(wt)
	if err != nil {
		t.Fatalf("prepare cache scope: %v", err)
	}
	defer cs()
	leafDir := filepath.Join(cd, "gradle")
	if err := os.MkdirAll(leafDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(leafDir, "init.d"), 0o755) })
	cr, err := os.MkdirTemp("/tmp", "omac-eng-stop-subdir")
	if err != nil {
		t.Fatalf("create short cache root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(cr) })

	const pid = 4249
	writeActiveRecord(t, cr, leafDir, pid, "/path/to/java", "start-id-subdir")
	killRec := &stopKillRecorder{}
	withStopBrokeredSeams(t, makeVerifyFake(verifyFakeVerified, killRec, syscall.SIGTERM), killRec)

	// Bare stop: RawArgs=nil (no --root). No wrapper at the worktree
	// root — the brokered stop must still succeed (it keys on the
	// cache-scope leaf, which is the same regardless of the wrapper
	// location).
	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   nil,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassSuccess {
		t.Errorf("class = %q, want %q (bare stop must not require a worktree-root wrapper)", res.Class, ClassSuccess)
	}
	if _, err := buildcontrol.LoadDaemonRecord(cr, leafDir); !errors.Is(err, buildcontrol.ErrNoDaemonRecord) {
		t.Errorf("record should be retired; err = %v", err)
	}
}

// TestStopBrokered_HonorsRootFlag asserts `--root backend` resolves
// the leaf for the backend/ worktree (the ownership record is keyed
// by the resolved leaf, not the worktree root). The leaf is keyed by
// the CACHE SCOPE, not the worktree subdir, so --root is a no-op for
// the brokered stop — accepted (to match the CLI grammar) but unused
// beyond worktree containment checks by the broker.
func TestStopBrokered_HonorsRootFlag(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	wt := t.TempDir()
	backend := filepath.Join(wt, "backend")
	if err := os.MkdirAll(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backend, "gradlew"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cd, cs, err := prepareTestCacheScope(wt)
	if err != nil {
		t.Fatalf("prepare cache scope: %v", err)
	}
	defer cs()
	leafDir := filepath.Join(cd, "gradle")
	if err := os.MkdirAll(leafDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(leafDir, "init.d"), 0o755) })
	cr, err := os.MkdirTemp("/tmp", "omac-eng-stop-root")
	if err != nil {
		t.Fatalf("create short cache root: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(cr) })

	// Write an active record for the leaf the --root backend path
	// resolves to. The leaf is GradleLeaf(cacheDir) — the SAME for
	// every --root under this cache scope (the leaf is keyed by the
	// cache scope, not the worktree subdir). So we write the record
	// for the cache-scope leaf and assert the stop retires it.
	const pid = 4248
	writeActiveRecord(t, cr, leafDir, pid, "/path/to/java", "start-id-root")
	killRec := &stopKillRecorder{}
	withStopBrokeredSeams(t, makeVerifyFake(verifyFakeVerified, killRec, syscall.SIGTERM), killRec)

	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   []string{"--root", "backend"},
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassSuccess {
		t.Errorf("class = %q, want %q", res.Class, ClassSuccess)
	}
	if _, err := buildcontrol.LoadDaemonRecord(cr, leafDir); !errors.Is(err, buildcontrol.ErrNoDaemonRecord) {
		t.Errorf("record should be retired; err = %v", err)
	}
}

// TestStopBrokered_DoesNotRemoveLockfile asserts the persistent leaf
// lockfile is NOT removed by the brokered stop (spec.md §231).
func TestStopBrokered_DoesNotRemoveLockfile(t *testing.T) {
	wt, cd, cs, cr, leaf := stopBrokeredTestEnv(t)
	defer cs()
	// Pre-create the leaf lockfile under the legacy in-leaf location
	// (CacheRoot is set, so the build-control lock is used; but the
	// legacy in-leaf lockfile path is what the spec's "lockfile is
	// never unlinked" requirement addresses. The brokered stop uses
	// the build-control lock; the legacy lockfile is left alone).
	lockPath := filepath.Join(leaf, ".omac-build.lock")
	if err := os.WriteFile(lockPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	killRec := &stopKillRecorder{}
	withStopBrokeredSeams(t, func(pid int, exe, start string) (bool, procidentity.Identity, error) {
		return false, procidentity.Identity{}, procidentity.ErrNoSuchProcess
	}, killRec)

	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   nil,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassSuccess {
		t.Errorf("class = %q, want %q", res.Class, ClassSuccess)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("legacy lockfile must NOT be removed by brokered stop: %v", err)
	}
}

// TestStopBrokered_DiagnosticSanitized asserts the service_failure
// diagnostic on stderr does not leak the host-only build-control path.
// (The broker redacts /build-control/ paths in its sanitizeMessage;
// the engine's own diagnostic should not include them either.)
func TestStopBrokered_DiagnosticSanitized(t *testing.T) {
	wt, cd, cs, cr, leaf := stopBrokeredTestEnv(t)
	defer cs()
	if err := buildcontrol.WritePendingDaemonRecord(cr, leaf, buildcontrol.DaemonRecord{
		State:         buildcontrol.DaemonStatePending,
		Marker:        "m",
		LeafDigest:    buildcontrol.HashLeaf(leaf),
		JDKExecutable: "/java",
		RequestID:     "r",
	}); err != nil {
		t.Fatal(err)
	}
	killRec := &stopKillRecorder{}
	withStopBrokeredSeams(t, func(pid int, exe, start string) (bool, procidentity.Identity, error) {
		return false, procidentity.Identity{}, procidentity.ErrNoSuchProcess
	}, killRec)

	var stderrBuf bytes.Buffer
	res := StopBrokered(StopBrokeredOptions{
		Workdir:   wt,
		RawArgs:   nil,
		Stdout:    io.Discard,
		Stderr:    &stderrBuf,
		CacheDir:  cd,
		CacheRoot: cr,
		Auditor:   audit.Nop(),
	})
	if res.Class != ClassServiceFailure {
		t.Fatalf("class = %q, want service_failure", res.Class)
	}
	if strings.Contains(stderrBuf.String(), cr) {
		t.Errorf("stderr leaked host-only cache root %q: %q", cr, stderrBuf.String())
	}
}

// TestStopBrokered_CancelAbortsLockAcquire asserts that a cancelled
// brokered stop (Cancel closed during the leaf-lock acquire) returns
// ClassCancelled with the cancelled marker on stderr. We force the
// lock to be contended so the cancel fires during acquire.
func TestStopBrokered_CancelAbortsLockAcquire(t *testing.T) {
	wt, cd, cs, cr, leaf := stopBrokeredTestEnv(t)
	defer cs()
	// Acquire the build-control leaf lock from another goroutine and
	// hold it so StopBrokered's acquire blocks. We then close the
	// Cancel channel and expect ClassCancelled.
	held, err := buildcontrol.Acquire(cr, leaf, buildcontrol.DefaultQueueTimeout, nil)
	if err != nil {
		t.Fatalf("holder Acquire: %v", err)
	}
	defer held.Release()

	cancel := make(chan struct{})
	done := make(chan Result, 1)
	go func() {
		done <- StopBrokered(StopBrokeredOptions{
			Workdir:   wt,
			RawArgs:   nil,
			Stdout:    io.Discard,
			Stderr:    io.Discard,
			CacheDir:  cd,
			CacheRoot: cr,
			Auditor:   audit.Nop(),
			Cancel:    cancel,
		})
	}()
	// Give the acquire time to block, then cancel.
	time.Sleep(50 * time.Millisecond)
	close(cancel)
	res := <-done
	if res.Class != ClassCancelled {
		t.Errorf("class = %q, want %q", res.Class, ClassCancelled)
	}
	if res.ExitCode() != 4 {
		t.Errorf("ExitCode = %d, want 4", res.ExitCode())
	}
}
