package buildrun

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DaemonHandshakeChannel is the executor supervisor's private control
// channel the Gradle daemon writes its ownership handshake to (ticket
// 07, spec.md §237). It is a Unix-domain socket under the per-request
// control bundle:
//
//	<build-control-root>/requests/<request-id>/daemon.sock
//
// (buildcontrol.RequestDir(cacheRoot, requestID) + "daemon.sock").
// The daemon-owner-handshake init script (RenderDaemonOwnerHandshakeInitScript)
// reads the socket path from a control-state file
// (<leaf>/.omac-control/daemon-handshake-sock, written by
// PrepareControlState when GradlePropertiesConfig.DaemonHandshakeSock
// is set), connects to the socket at daemon startup BEFORE project
// configuration proceeds, and sends a single JSON line
// `{"pid":<pid>,"marker":"<marker>"}`. AwaitHandshake accepts one
// connection, reads the JSON, verifies the marker matches the
// expected value (constant-time — the marker is not secret but a
// mismatch is a denial; subtle.ConstantTimeCompare is used for
// hygiene), calls the caller-supplied verify seam (procidentity-based:
// the caller passes procidentity.Verify or a test fake) to check the
// process is the leaf's Gradle daemon, and on success writes a
// one-byte ack ("1") and returns the verified PID. On marker mismatch,
// verify failure, or timeout → the channel closes WITHOUT acking, so
// the init script's read returns -1/EOF, the script throws a
// GradleException, and the build fails closed (the wrapper cannot
// proceed unverified).
//
// Phase 2 exposes the channel + path; Phase 3 (engine wiring)
// generates the marker, writes the pending DaemonRecord, starts the
// channel, threads the socket path into PrepareControlState, runs the
// wrapper, awaits the handshake, promotes pending→active, and acks.
type DaemonHandshakeChannel struct {
	// sockPath is the absolute path of the Unix socket file. The
	// daemon connects to it via java.net.UnixDomainSocketAddress.
	sockPath string
	// ln is the Unix socket listener. nil after Close.
	ln net.Listener
	// sockDirIsTemp is true when sockPath lives inside a private
	// (0o700) temp dir created by resolveDaemonSockPath as a SUN_LEN
	// fallback. Close removes the temp dir parent in that case so the
	// fallback does not leak 0o700 dirs under os.TempDir(). False for
	// the canonical path (the per-request control bundle owns that
	// dir).
	sockDirIsTemp bool
	// cancelMu guards cancel + cancelClosed so Cancel is idempotent
	// and safe to call concurrently with AwaitHandshake and Close.
	cancelMu       sync.Mutex
	cancelClosed   bool
	cancelNotifyCh chan struct{}
}

// Cancel interrupts a blocked AwaitHandshake. It closes the listener
// (so a blocked Accept returns immediately with a "use of closed
// network connection" error) and signals AwaitHandshake to return
// ErrHandshakeCancelled. Safe to call before AwaitHandshake, after it
// returns, or concurrently with it; idempotent. The engine calls this
// when RunBuild returns (success or failure) so a wrapper that exits
// before the daemon dials does NOT hang for the full handshake deadline
// (DefaultHandshakeDeadline, 45s) — without this, every fast-failing
// brokered build would pay a 45s penalty waiting for a handshake that
// never arrives. Closing the listener is the cancellation primitive:
// Go's net.Listener.Accept returns a net.OpError wrapping
// net.ErrClosed, which AwaitHandshake maps to ErrHandshakeCancelled.
//
// Cancel does NOT remove the socket file (Close does that). The two
// are distinct: Cancel is the interrupt signal; Close is the resource
// release. The engine defers Close (resource release) AND calls Cancel
// on RunBuild return (interrupt). Calling Close first would also
// unblock Accept, but Close also removes the socket file; Cancel
// keeps the listener state intact for diagnostics until the deferred
// Close runs.
func (c *DaemonHandshakeChannel) Cancel() {
	if c == nil {
		return
	}
	c.cancelMu.Lock()
	defer c.cancelMu.Unlock()
	if c.cancelClosed {
		return
	}
	c.cancelClosed = true
	if c.cancelNotifyCh != nil {
		close(c.cancelNotifyCh)
	}
	// Close the listener to unblock a pending Accept. A nil listener
	// (Listen failed or already closed) is a no-op. The error is
	// ignored — Accept will surface its own error to AwaitHandshake.
	if c.ln != nil {
		_ = c.ln.Close()
		c.ln = nil
	}
}

// ErrHandshakeCancelled is returned by AwaitHandshake when Cancel
// interrupted a blocked Accept (the engine signalled the wrapper
// exited and the host is no longer waiting for the daemon). Distinct
// from ErrHandshakeTimeout (a deadline) so the engine can distinguish
// "wrapper exited, no daemon" (cancel — not a build failure on its
// own; the wrapper's own exit code is authoritative) from "daemon
// never registered in time" (timeout — a handshake failure).
var ErrHandshakeCancelled = errors.New("buildrun: daemon handshake cancelled (wrapper exited before the daemon registered)")

// DaemonHandshakePID is the JSON payload the Gradle daemon sends over
// the private control channel. The daemon's PID (extracted portably in
// the init script via ManagementFactory.getRuntimeMXBean().getName()
// split("@")[0], Java 8+) and the owner marker the host injected into
// org.gradle.jvmargs. The host compares the marker against the
// expected value before calling the verify seam.
type DaemonHandshakePID struct {
	PID    int    `json:"pid"`
	Marker string `json:"marker"`
}

// DaemonHandshakeVerifier is the procidentity seam AwaitHandshake
// calls after the marker matches. The contract mirrors
// procidentity.Verify:
//
//	Verify(pid, expectedJDKExecutable, expectedStart) (verified bool, id Identity, err error)
//
// At handshake time (pending → active) the caller passes
// expectedStart="" so Verify checks process liveness + executable +
// main class only and returns the Identity (whose StartIdentity the
// caller then records via buildcontrol.PromoteDaemonRecord). A
// verified=false (live but mismatched) or any error (ErrNoSuchProcess,
// ErrUnverifiable) → no ack, the build fails closed.
//
// Tests inject a fake; production wires procidentity.Verify.
type DaemonHandshakeVerifier func(pid int) (verified bool, err error)

// NewDaemonHandshakeChannel returns a DaemonHandshakeChannel value
// for sockPath without listening. Call Listen to bind the socket,
// AwaitHandshake to accept one connection and complete the handshake,
// and Close to release the listener + remove the socket file. The
// constructor is separate from Listen so the caller can defer Close
// unconditionally even on a Listen failure.
func NewDaemonHandshakeChannel(sockPath string) *DaemonHandshakeChannel {
	return &DaemonHandshakeChannel{sockPath: sockPath}
}

// SockPath returns the absolute path of the Unix socket file. The
// caller (engine wiring, Phase 3) passes this to
// GradlePropertiesConfig.DaemonHandshakeSock so PrepareControlState
// writes it to the daemon-handshake-sock control-state file the init
// script reads. Safe to call before Listen (returns the planned
// path).
func (c *DaemonHandshakeChannel) SockPath() string {
	if c == nil {
		return ""
	}
	return c.sockPath
}

// Listen binds the Unix socket listener at sockPath. The socket's
// parent directory must already exist with mode 0o700 (the engine
// creates buildcontrol.RequestDir's gradle-control/ subdir or the
// RequestDir itself with 0o700). Listen removes any stale socket file
// at sockPath before binding (a leftover from a crashed previous
// run); the unlink is best-effort. The socket file is created mode
// 0o600 by net.ListenUnix (owner-only — the per-request control
// bundle is host-only trusted state, never in executor grants).
//
// SUN_LEN note: macOS limits a Unix-domain socket path to 104 bytes
// (SUN_LEN). RequestDir under the default ~/.cache/omac/build-control/
// requests/<id>/daemon.sock may approach this; the e2e-local.sh
// TMPDIR=/tmp/omac-e2e workaround (AGENTS.md) is the documented
// pattern for the same constraint on the facade's bridge.sock. If
// sockPath exceeds SUN_LEN, Listen fails with a bind error; Phase 3
// (or Phase 5) wires the short-path TMPDIR workaround if needed. For
// Phase 2 only the listener + path are exposed, with this length
// concern documented here.
func (c *DaemonHandshakeChannel) Listen() error {
	if c == nil {
		return errors.New("buildrun: nil DaemonHandshakeChannel")
	}
	if c.sockPath == "" {
		return errors.New("buildrun: empty daemon handshake socket path")
	}
	// Best-effort unlink of a stale socket from a crashed previous run.
	// A missing file is not an error; a non-socket file at sockPath
	// would make bind fail with EADDRINUSE, which the unlink clears.
	if err := os.Remove(c.sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Surface a non-ENOENT unlink failure, but do not abort: a
		// permission error here means the parent dir is not writable
		// (a setup bug), which bind will also surface.
		return fmt.Errorf("buildrun: unlink stale daemon handshake socket %s: %w", c.sockPath, err)
	}
	ln, err := net.Listen("unix", c.sockPath)
	if err != nil {
		return fmt.Errorf("buildrun: listen daemon handshake socket %s: %w", c.sockPath, err)
	}
	// Enforce owner-only on the socket file (net.ListenUnix uses
	// umask, not an explicit mode). The per-request control bundle is
	// host-only; a group/world-readable socket would expose the
	// handshake (the marker is not secret, but the socket is a host
	// control surface).
	if err := os.Chmod(c.sockPath, 0o600); err != nil {
		ln.Close()
		_ = os.Remove(c.sockPath)
		return fmt.Errorf("buildrun: chmod daemon handshake socket %s: %w", c.sockPath, err)
	}
	c.ln = ln
	return nil
}

// ErrHandshakeTimeout is returned by AwaitHandshake when the deadline
// elapses before a complete, verified handshake arrives. The caller
// (engine wiring) treats this as a service failure: the daemon did
// not register in time, so the build cannot proceed safely.
var ErrHandshakeTimeout = errors.New("buildrun: daemon handshake timed out")

// ErrHandshakeMarkerMismatch is returned by AwaitHandshake when the
// daemon sends a marker that does not match the expected value. A
// mismatch means the daemon is not the one the host started (a stale
// or PID-recycled process spoofing the leaf). No ack is sent; the
// init script throws and the build fails closed.
var ErrHandshakeMarkerMismatch = errors.New("buildrun: daemon handshake marker mismatch")

// ErrHandshakeVerifyFailed is returned by AwaitHandshake when the
// verify seam reports the process is live but does not match (not the
// resolved JDK, not the Gradle daemon main class, or — at promote
// time with expectedStart empty — start identity not extractable).
// No ack is sent; the build fails closed.
var ErrHandshakeVerifyFailed = errors.New("buildrun: daemon handshake process verify failed")

// handshakeAckByte is the single byte the host writes to acknowledge
// the daemon. The init script reads exactly one byte and checks it
// equals '1' (ASCII 0x31). A single byte — not a line — so no line
// terminator convention is needed across the host/Java boundary.
const handshakeAckByte byte = '1'

// AwaitHandshake accepts one connection on the listener, reads the
// {"pid","marker"} JSON line, verifies the marker matches
// expectedMarker (constant-time), calls verify(pid) to check the
// process is the leaf's Gradle daemon, and on success writes the
// one-byte ack and returns the verified PID. On marker mismatch,
// verify failure, or timeout → the connection is closed WITHOUT
// acking, the listener is NOT closed (the caller may want to retry or
// inspect), and an error is returned. The caller (engine wiring)
// treats any error as a build failure (the init script's read returns
// -1/EOF, the script throws a GradleException, the build fails closed).
//
// deadline bounds how long AwaitHandshake waits for a complete,
// verified handshake. A zero or negative deadline returns
// ErrHandshakeTimeout immediately (the caller must supply a positive
// bound; the spec's bounded-wait requirement forbids an unbounded
// block). The deadline covers accept + read + verify; the verify seam
// is the caller's responsibility and must itself be bounded (procidentity.
// Verify reads /proc or libproc, which are fast).
//
// The expectedMarker is the value the host minted
// (NewDaemonOwnerMarker) and wrote into gradle.properties and the
// pending DaemonRecord. The daemon echoes it back; a mismatch is a
// denial. subtle.ConstantTimeCompare is used for hygiene (the marker
// is not secret, but a timing oracle on the mismatch is pointless
// and constant-time is the codebase style — see
// buildbroker token comparison).
//
// verify is the procidentity seam. At handshake time (pending →
// active) the caller passes a closure that calls procidentity.Verify
// (pid, expectedJDKExecutable, "") — expectedStart is empty because
// the daemon was JUST promoted and has no recorded start identity
// yet; Verify then checks liveness + executable + main class and
// returns the Identity whose StartIdentity the caller records via
// buildcontrol.PromoteDaemonRecord. A verified=false (live but
// mismatched) or any error (ErrNoSuchProcess, ErrUnverifiable) → no
// ack, ErrHandshakeVerifyFailed (or the wrapped verify error). Tests
// inject a fake that returns true/false/error without spawning real
// processes.
//
// Returns the verified PID on success (so the caller can promote the
// record with it) and nil on any failure.
func (c *DaemonHandshakeChannel) AwaitHandshake(deadline time.Duration, expectedMarker string, verify DaemonHandshakeVerifier) (int, error) {
	if c == nil || c.ln == nil {
		return 0, errors.New("buildrun: daemon handshake channel not listening")
	}
	if deadline <= 0 {
		return 0, ErrHandshakeTimeout
	}
	if expectedMarker == "" {
		return 0, errors.New("buildrun: empty expected marker")
	}
	if verify == nil {
		return 0, errors.New("buildrun: nil verify seam")
	}
	// Bound the whole handshake. Set a deadline on the listener; a
	// connection that does not arrive in time yields a net.OpError
	// wrapping a timeout, which we map to ErrHandshakeTimeout.
	if err := c.ln.(*net.UnixListener).SetDeadline(time.Now().Add(deadline)); err != nil {
		return 0, fmt.Errorf("buildrun: set daemon handshake listener deadline: %w", err)
	}
	conn, err := c.ln.Accept()
	if err != nil {
		if isTimeout(err) {
			return 0, ErrHandshakeTimeout
		}
		// A closed listener (Cancel closed it, or Close raced) maps
		// to ErrHandshakeCancelled so the engine distinguishes
		// "wrapper exited, no daemon" (cancel) from "daemon never
		// registered" (timeout). net.ErrClosed is the sentinel Go's
		// net package returns when a listener is closed under Accept.
		if errors.Is(err, net.ErrClosed) {
			return 0, ErrHandshakeCancelled
		}
		return 0, fmt.Errorf("buildrun: accept daemon handshake: %w", err)
	}
	// Defer closing the connection (NOT the listener): on any failure
	// the daemon's read returns -1/EOF, the init script throws, the
	// build fails closed. The listener stays open so the caller can
	// decide to retry or give up.
	defer conn.Close()
	// Bound the read too: a daemon that connects but never sends the
	// handshake line must not hold the host forever.
	if err := conn.SetReadDeadline(time.Now().Add(deadline)); err != nil {
		return 0, fmt.Errorf("buildrun: set daemon handshake read deadline: %w", err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		if isTimeout(err) {
			return 0, ErrHandshakeTimeout
		}
		return 0, fmt.Errorf("buildrun: read daemon handshake line: %w", err)
	}
	if len(line) == 0 {
		return 0, errors.New("buildrun: empty daemon handshake line")
	}
	var pid DaemonHandshakePID
	if err := json.Unmarshal([]byte(line), &pid); err != nil {
		return 0, fmt.Errorf("buildrun: parse daemon handshake line: %w", err)
	}
	// Constant-time marker compare (not secret, but hygiene + matches
	// the codebase token-compare style). A mismatch is a denial: no
	// ack, the build fails closed.
	if subtle.ConstantTimeCompare([]byte(pid.Marker), []byte(expectedMarker)) != 1 {
		return 0, ErrHandshakeMarkerMismatch
	}
	if pid.PID <= 0 {
		return 0, fmt.Errorf("buildrun: daemon handshake carried non-positive pid %d", pid.PID)
	}
	// procidentity verify: the caller's closure checks the process is
	// the leaf's Gradle daemon (executable + main class + liveness).
	// verified=false → no ack; any error → no ack.
	verified, verr := verify(pid.PID)
	if verr != nil {
		return 0, fmt.Errorf("buildrun: %w: %v", ErrHandshakeVerifyFailed, verr)
	}
	if !verified {
		return 0, ErrHandshakeVerifyFailed
	}
	// Acknowledge: write the single ack byte. The init script's read
	// returns this byte and the daemon proceeds with project
	// configuration. The write is bounded by the read deadline already
	// set on the conn (Go's net.Conn deadlines apply to both reads and
	// writes).
	if _, err := conn.Write([]byte{handshakeAckByte}); err != nil {
		return 0, fmt.Errorf("buildrun: write daemon handshake ack: %w", err)
	}
	return pid.PID, nil
}

// Close releases the listener and removes the socket file. Safe to
// call on a nil receiver or after a failed Listen (no-op then).
// Idempotent. The socket file removal is best-effort: a missing file
// (already removed, or never created) is not an error.
func (c *DaemonHandshakeChannel) Close() error {
	if c == nil {
		return nil
	}
	var lnErr error
	if c.ln != nil {
		lnErr = c.ln.Close()
		c.ln = nil
	}
	if c.sockPath != "" {
		if err := os.Remove(c.sockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			if lnErr != nil {
				return fmt.Errorf("buildrun: close listener: %v; remove socket %s: %w", lnErr, c.sockPath, err)
			}
			return fmt.Errorf("buildrun: remove daemon handshake socket %s: %w", c.sockPath, err)
		}
		// If the socket lived in a private temp dir created by the
		// SUN_LEN fallback (resolveDaemonSockPath), remove the now-empty
		// temp dir so the fallback does not leak 0o700 dirs under
		// os.TempDir() across builds. Best-effort: a non-empty dir (a
		// race) or a missing dir is not an error.
		if c.sockDirIsTemp {
			_ = os.Remove(filepath.Dir(c.sockPath))
		}
	}
	return lnErr
}

// DaemonHandshakeSockPath returns the absolute path of the daemon
// handshake Unix socket under the per-request control bundle:
// <RequestDir>/daemon.sock. The caller (engine wiring, Phase 3)
// passes cacheRoot + requestID; buildcontrol.RequestDir gives the
// per-request dir, and the socket sits directly under it. The parent
// directory must exist with mode 0o700 (buildcontrol.EnsureRoot
// creates the requests/ parent; the per-request dir is created by
// the engine when the request is accepted). Phase 2 exposes the
// path; Phase 3 wires it into GradlePropertiesConfig.DaemonHandshakeSock
// so PrepareControlState writes it to the daemon-handshake-sock
// control-state file.
func DaemonHandshakeSockPath(requestDir string) string {
	return filepath.Join(requestDir, "daemon.sock")
}

// isTimeout reports whether err is a net timeout (a net.OpError with
// Timeout() true, or a wrapped such error). Used to map listener /
// read deadlines to ErrHandshakeTimeout.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}
