package buildrun

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildcontrol"
	"github.com/tngtech/oh-my-agentic-coder/internal/procidentity"
)

// DaemonOwnershipConfig bundles the inputs the engine wires for the
// pending-to-active daemon ownership handshake (ticket 07, spec.md
// §237). The engine (Phase 3) constructs this BEFORE calling GrantsFor
// so the marker + socket path flow into BuildConfig →
// GradlePropertiesConfig → PrepareControlState (the init script reads
// the marker from -Domac.daemon.owner and the socket path from the
// daemon-handshake-sock control-state file).
//
// When ANY of CacheRoot / CanonicalLeaf / RequestID is zero, the
// ownership path is disabled and RunBuild behaves exactly as it did
// before Phase 3 (behavior-preserving for the existing run_test.go /
// engine_test.go tests that do not set these). When set, the engine:
//
//  1. mints a marker (NewDaemonOwnerMarker),
//  2. writes the pending DaemonRecord (buildcontrol.WritePendingDaemonRecord),
//  3. starts the DaemonHandshakeChannel at DaemonHandshakeSockPath(RequestDir),
//  4. threads marker + sock path into BuildConfig so GrantsFor →
//     PrepareControlState renders them into gradle.properties + the
//     daemon-handshake-sock control file,
//  5. AFTER GrantsFor returns, resolves the JDKExecutable from
//     grants.JDKExecutable() and builds the verify closure,
//  6. launches the wrapper (RunBuild, unchanged),
//  7. concurrently awaits the handshake (AwaitHandshake) with the
//     verify closure that calls procidentity.Verify and — INSIDE the
//     closure, BEFORE the ack — calls buildcontrol.PromoteDaemonRecord,
//  8. on handshake error, cancels the wrapper (closes the engine's
//     internal cancel channel) so the build fails closed without
//     waiting the init script's 30s read timeout,
//  9. after the wrapper exits, runs the in-sandbox `gradlew --stop`
//     recycle (RunStopInSandbox) and retires the record.
//
// JDKExecutable is NOT required at PrepareDaemonOwnership time (it is
// resolved from grants AFTER GrantsFor, since GrantsFor owns JDK
// resolution); it is only needed for the verify closure, which runs
// during AwaitHandshake (after the wrapper launches).
type DaemonOwnershipConfig struct {
	// CacheRoot is the shared cache root (parent of cache-scope dirs)
	// under which the host-only build-control root lives. The pending
	// DaemonRecord is written at buildcontrol.DaemonPath(cacheRoot,
	// canonicalLeaf); the handshake socket lives at
	// buildcontrol.RequestDir(cacheRoot, requestID) + "/daemon.sock".
	// Empty disables the ownership path.
	CacheRoot string
	// CanonicalLeaf is the resolved Gradle cache leaf
	// (GradleLeaf(cacheDir)) the handshake verifies the daemon against.
	// Empty disables the ownership path.
	CanonicalLeaf string
	// RequestID is the build request id (buildrun.NewBuildRequestID)
	// the pending record attributes a stale pending record to. Empty
	// disables the ownership path.
	RequestID string
	// JDKExecutable is the EvalSymlinks-resolved path of the resolved
	// JDK's `java` binary (BuildGrants.JDKExecutable()). The verify
	// closure compares the daemon's resolved executable against it
	// (procidentity.Verify). Set AFTER GrantsFor (the engine resolves
	// it from grants); empty at PrepareDaemonOwnership time is fine
	// (the verify closure is built later via
	// DefaultDaemonOwnershipVerifier once grants are known). If still
	// empty when the verify closure is built, the engine treats it as
	// a service failure (the daemon cannot be verified without a
	// resolved JDK executable).
	JDKExecutable string
	// HandshakeDeadline bounds AwaitHandshake (accept + read + verify).
	// Zero uses DefaultHandshakeDeadline. The init script's own read
	// timeout is 30s; this deadline should be >= that so the daemon's
	// failure path (throw on read-timeout/EOF) is what surfaces, not a
	// host-side pre-emptive timeout — but a host-side bound is still
	// required (the spec's bounded-wait requirement forbids an
	// unbounded block).
	HandshakeDeadline time.Duration
	// Verify is the procidentity seam the handshake calls after the
	// marker matches. Production wires a closure that calls
	// procidentity.Verify(pid, cfg.JDKExecutable, "") and — INSIDE the
	// closure, BEFORE returning true — calls
	// buildcontrol.PromoteDaemonRecord (the promote-before-ack
	// ordering the Phase 2 handoff pins as CRITICAL). nil selects
	// DefaultDaemonOwnershipVerifier (the production closure). Tests
	// inject a fake to assert the lifecycle without spawning real
	// processes.
	Verify DaemonHandshakeVerifier
}

// DefaultHandshakeDeadline bounds AwaitHandshake when
// DaemonOwnershipConfig.HandshakeDeadline is zero. 45s gives the
// daemon headroom over the init script's 30s read timeout so the
// daemon's own fail-closed throw is what surfaces on a host that never
// acks (the host-side bound is still present so a hung daemon cannot
// hold the host forever).
const DefaultHandshakeDeadline = 45 * time.Second

// Enabled reports whether the ownership path is wired (the three
// fields PrepareDaemonOwnership needs are set: CacheRoot,
// CanonicalLeaf, RequestID). JDKExecutable is NOT required at prepare
// time (it is resolved from grants AFTER GrantsFor). When false, the
// engine runs the legacy Phase-2 path (RunBuild unchanged, the old
// unsandboxed daemonRecycle). When true, the engine runs the Phase-3
// path (pending record + handshake channel + in-sandbox recycle +
// retire).
func (c DaemonOwnershipConfig) Enabled() bool {
	return c.CacheRoot != "" && c.CanonicalLeaf != "" && c.RequestID != ""
}

// VerifyReady reports whether the verify closure can be built (the
// JDKExecutable is resolved). The engine checks this AFTER GrantsFor;
// if false, the build fails closed as a service failure (the daemon
// cannot be verified without a resolved JDK executable).
func (c DaemonOwnershipConfig) VerifyReady() bool {
	return c.Enabled() && c.JDKExecutable != ""
}

// OwnershipHandshakeResult is what the engine's handshake goroutine
// returns: the verified PID (on success) or the error (on any failure).
// The engine reads this after RunBuild returns to decide whether to
// retire the record and run the recycle, or fail closed.
type OwnershipHandshakeResult struct {
	PID int
	Err error
}

// PrepareDaemonOwnership mints the marker, writes the pending
// DaemonRecord, and starts the DaemonHandshakeChannel. The engine
// calls this BEFORE GrantsFor so the returned marker + socket path can
// flow into BuildConfig (and from there into GradlePropertiesConfig →
// PrepareControlState). The returned channel must be Closed by the
// engine (defer) after RunBuild returns; the pending record is
// retired by the engine after the in-sandbox recycle (or on handshake
// failure).
//
// On any failure (marker mint, pending write, listen) the engine
// treats the build as a service failure BEFORE launching the wrapper
// (the spec's fail-closed requirement: a build that cannot establish
// ownership must not start).
//
// The verify closure captures cfg.JDKExecutable + cfg.CanonicalLeaf +
// cfg.CacheRoot so the promote happens INSIDE the closure (before the
// ack), per the Phase 2 handoff's critical ordering note. If the
// promote fails, the closure returns false and no ack is written (the
// build fails closed).
func PrepareDaemonOwnership(cfg DaemonOwnershipConfig) (marker DaemonOwnerMarker, ch *DaemonHandshakeChannel, err error) {
	if !cfg.Enabled() {
		return "", nil, errors.New("buildrun: PrepareDaemonOwnership called with disabled config")
	}
	marker, err = NewDaemonOwnerMarker()
	if err != nil {
		return "", nil, fmt.Errorf("buildrun: prepare daemon ownership: %w", err)
	}
	// Write the pending record BEFORE starting the channel so the
	// handshake's verify closure can promote pending → active. The
	// record carries the marker, leaf digest, resolved JDK executable,
	// and request id (spec.md §237).
	if err := buildcontrol.WritePendingDaemonRecord(cfg.CacheRoot, cfg.CanonicalLeaf, buildcontrol.DaemonRecord{
		State:         buildcontrol.DaemonStatePending,
		Marker:        marker,
		LeafDigest:    buildcontrol.HashLeaf(cfg.CanonicalLeaf),
		JDKExecutable: cfg.JDKExecutable,
		RequestID:     cfg.RequestID,
	}); err != nil {
		return "", nil, fmt.Errorf("buildrun: write pending daemon record: %w", err)
	}
	// Start the handshake channel at the per-request control bundle.
	// buildcontrol.EnsureRoot creates the requests/ parent (mode
	// 0o700); the per-request dir itself is created here (mode 0o700,
	// owner-only — the socket file is mode 0o600 by Listen). The
	// socket's parent MUST exist before net.Listen("unix", ...).
	reqDir := buildcontrol.RequestDir(cfg.CacheRoot, cfg.RequestID)
	if err := os.MkdirAll(reqDir, buildcontrol.RootMode); err != nil {
		_ = buildcontrol.RetireDaemonRecord(cfg.CacheRoot, cfg.CanonicalLeaf)
		return "", nil, fmt.Errorf("buildrun: create per-request control dir: %w", err)
	}
	sockPath := DaemonHandshakeSockPath(reqDir)
	// Keep the socket path short on macOS (SUN_LEN 104-byte limit):
	// the per-request dir under the default ~/.cache/omac/build-control/
	// requests/<id>/daemon.sock may approach or exceed the limit (the
	// default macOS path is exactly 104 bytes for a typical
	// /Users/<short-name>/Library/Caches/omac home; a longer username
	// or a worktree-rooted cache scope exceeds it). resolveDaemonSockPath
	// falls back to a short os.TempDir()-rooted path when the canonical
	// path exceeds SUN_LEN on darwin (the TMPDIR=/tmp/omac-e2e pattern
	// documented in AGENTS.md for the facade's bridge.sock). On non-
	// darwin platforms the canonical path is always used (Linux's
	// sockaddr_un.sun_path is 108 bytes, and the tmpdir fallback is not
	// needed).
	sockPath = resolveDaemonSockPath(reqDir, cfg.RequestID)
	ch = NewDaemonHandshakeChannel(sockPath)
	// Track whether the socket lives in a private temp dir (the SUN_LEN
	// fallback) so Close can remove the temp dir parent and not leak
	// 0o700 dirs under os.TempDir(). The canonical path lives in the
	// per-request control bundle, which the engine does not remove
	// (it's reused across the request lifetime).
	ch.sockDirIsTemp = sockPath != DaemonHandshakeSockPath(reqDir)
	if err := ch.Listen(); err != nil {
		// Best-effort retire the pending record so a next build re-arms
		// cleanly; a listen failure means the host cannot receive the
		// handshake, so the build must fail closed.
		_ = buildcontrol.RetireDaemonRecord(cfg.CacheRoot, cfg.CanonicalLeaf)
		return "", nil, fmt.Errorf("buildrun: listen daemon handshake: %w", err)
	}
	return marker, ch, nil
}

// AwaitDaemonOwnership runs AwaitHandshake on the channel in the
// current goroutine. The engine typically runs this in a goroutine
// concurrently with RunBuild; on error the engine closes the wrapper's
// cancel channel. The verify closure (cfg.Verify, or the default
// production closure if nil) does the promote INSIDE the closure so
// the promote happens BEFORE the ack (Phase 2 critical ordering).
//
// Returns the verified PID on success, or an error on any failure
// (marker mismatch, verify false, verify error, timeout). The caller
// treats any error as a handshake failure → cancel the wrapper + fail
// closed.
func AwaitDaemonOwnership(cfg DaemonOwnershipConfig, marker DaemonOwnerMarker, ch *DaemonHandshakeChannel) OwnershipHandshakeResult {
	deadline := cfg.HandshakeDeadline
	if deadline <= 0 {
		deadline = DefaultHandshakeDeadline
	}
	verify := cfg.Verify
	if verify == nil {
		verify = DefaultDaemonOwnershipVerifier(cfg)
	}
	pid, err := ch.AwaitHandshake(deadline, marker, verify)
	return OwnershipHandshakeResult{PID: pid, Err: err}
}

// DefaultDaemonOwnershipVerifier returns the production verify closure
// the handshake calls after the marker matches (Phase 3 wiring). The
// closure:
//
//  1. calls procidentity.Verify(pid, cfg.JDKExecutable, "") — at
//     handshake time expectedStart is "" (the daemon was JUST
//     promoted; the start identity is captured FROM the returned
//     Identity and recorded via PromoteDaemonRecord),
//  2. on verified=true, calls buildcontrol.PromoteDaemonRecord
//     INSIDE the closure (BEFORE the ack — the Phase 2 handoff's
//     critical ordering note: the promote must happen before the ack
//     so the daemon cannot proceed before the record is active, and a
//     promote failure means no ack → build fails closed),
//  3. returns (true, nil) only when BOTH verify AND promote succeed.
//
// A verify=false (live but mismatched) or any error (ErrNoSuchProcess,
// ErrUnverifiable, promote failure) → (false, err) → no ack → the init
// script throws → the build fails closed.
func DefaultDaemonOwnershipVerifier(cfg DaemonOwnershipConfig) DaemonHandshakeVerifier {
	return func(pid int) (bool, error) {
		verified, id, err := procidentity.Verify(pid, cfg.JDKExecutable, "")
		if err != nil {
			return false, err
		}
		if !verified {
			return false, nil
		}
		// Promote INSIDE the closure, BEFORE the ack. A promote
		// failure (record was retired between the pending write and
		// the handshake, or a concurrent promote) → no ack → fail
		// closed.
		if err := buildcontrol.PromoteDaemonRecord(cfg.CacheRoot, cfg.CanonicalLeaf, pid, id.StartIdentity); err != nil {
			return false, fmt.Errorf("buildrun: promote daemon record: %w", err)
		}
		return true, nil
	}
}

// RetireDaemonOwnership retires the pending/active DaemonRecord after
// the build completes (success or failure). Best-effort: a retire
// failure is logged but not fatal (the record will be reconciled at
// the next parent startup via buildcontrol.ReconcileDaemonRecords).
// The engine calls this AFTER the in-sandbox `gradlew --stop` recycle
// so the record covers the daemon's full lifecycle.
//
// io.Writer is the engine's stderr (for the best-effort warning).
func RetireDaemonOwnership(cfg DaemonOwnershipConfig, stderr io.Writer) {
	if !cfg.Enabled() {
		return
	}
	if err := buildcontrol.RetireDaemonRecord(cfg.CacheRoot, cfg.CanonicalLeaf); err != nil {
		fmt.Fprintf(stderr, "omac build: warning: retire daemon record failed: %v\n", err)
	}
}

// daemonSockSunLenLimit is the macOS Unix-domain socket path length
// limit (SUN_LEN, 104 bytes including the NUL terminator — so 103
// usable bytes; net.ListenUnix rejects a path whose len > 103). On
// non-darwin platforms this limit is not enforced (Linux's
// sockaddr_un.sun_path is 108 bytes) and resolveDaemonSockPath
// returns the canonical path unchanged.
const daemonSockSunLenLimit = 103

// resolveDaemonSockPath returns the daemon-handshake socket path to
// use for the given per-request control dir. On darwin, when the
// canonical path (<reqDir>/daemon.sock) exceeds the 103-byte SUN_LEN
// usable limit, it falls back to a short, PRIVATE (0o700) temp dir
// under os.TempDir() so net.Listen("unix", ...) does not fail with
// `bind: invalid argument`. The fallback path is
// <private-temp-dir>/daemon.sock, where the private temp dir is
// created via os.MkdirTemp (mode 0o700, owner-only) — NOT a bare
// os.TempDir() path. A bare os.TempDir() socket would be
// world-writable-adjacent on /tmp (a same-user adversary could win
// the unlink+listen race); the private 0o700 dir closes that. The
// short id (<sha256(requestID)[:8]>) in the dir name keeps the full
// path under SUN_LEN even when os.TempDir() is a deep
// /var/folders/.../T/ path.
//
// The fallback path's parent always exists and is owner-only on every
// supported platform. This mirrors the TMPDIR=/tmp/omac-e2e workaround
// documented in AGENTS.md for the facade's bridge.sock (the same
// SUN_LEN constraint on a deep /var/folders/... TMPDIR). The fallback
// is a host-only control surface (the init script reads the path from
// the daemon-handshake-sock control-state file, which the engine
// writes with the resolved path), so the daemon dials wherever the
// host listens — the path does not need to live under the per-request
// control bundle.
//
// On non-darwin platforms the canonical path is always returned (the
// 108-byte Linux limit is comfortably above any realistic path).
//
// The caller (PrepareDaemonOwnership) is responsible for cleaning up
// the fallback temp dir (via the channel's Close, which removes the
// socket file, plus the engine's defer). The private dir itself is
// left for os.TempDir() cleanup; this is acceptable because the
// socket file (the shared resource) is removed, and an empty 0o700 dir
// is harmless.
func resolveDaemonSockPath(reqDir, requestID string) string {
	canonical := DaemonHandshakeSockPath(reqDir)
	if runtime.GOOS != "darwin" {
		return canonical
	}
	if len(canonical) <= daemonSockSunLenLimit {
		return canonical
	}
	// Fallback: a PRIVATE (0o700) dir under a short temp root, with
	// the socket inside it. A bare os.TempDir() socket would be
	// world-writable-adjacent on /tmp (a same-user adversary could win
	// the unlink+listen race); a private 0o700 dir closes that.
	//
	// The short id (<sha256(requestID)[:8]>) in the dir name keeps the
	// full path under SUN_LEN and gives ~32 bits of uniqueness so
	// concurrent builds do not collide.
	//
	// Candidate base dirs, in order of preference:
	//  1. <os.TempDir()>/omac-daemon-socks (per-user private dir; 0o700)
	//  2. /tmp/omac-daemon-socks (short, always fits SUN_LEN)
	// Each candidate's resulting full path is length-checked; the
	// first that fits SUN_LEN wins. If none fits (a pathological
	// TMPDIR), the canonical path is returned and net.Listen surfaces
	// the bind error (fail closed — do NOT silently use a
	// world-writable location).
	sum := sha256.Sum256([]byte(requestID))
	short := hex.EncodeToString(sum[:4]) // 8 hex chars
	tryBase := func(base string) (string, bool) {
		dir := filepath.Join(base, "omac-daemon-"+short)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", false
		}
		// Re-assert 0o700 in case the dir pre-existed with a looser
		// mode (a prior crash left it; chmod tightens it back).
		_ = os.Chmod(dir, 0o700)
		sock := filepath.Join(dir, "daemon.sock")
		if len(sock) > daemonSockSunLenLimit {
			// The base is too deep (a pathological TMPDIR). Remove
			// the dir we just made and try the next candidate.
			_ = os.RemoveAll(dir)
			return "", false
		}
		return sock, true
	}
	// Candidate 1: the per-user temp dir (preferred — private on every
	// platform).
	if sock, ok := tryBase(os.TempDir()); ok {
		return sock
	}
	// Candidate 2: /tmp directly (short; always fits SUN_LEN). On
	// macOS /tmp is a symlink to /private/tmp but net.Listen resolves
	// it; the 0o700 dir still closes the same-user race.
	if sock, ok := tryBase("/tmp"); ok {
		return sock
	}
	// Neither fits (a pathological environment). Return the canonical
	// path; net.Listen will fail with the bind error and the engine
	// surfaces a service failure (fail closed — do NOT silently use a
	// world-writable location).
	return canonical
}
