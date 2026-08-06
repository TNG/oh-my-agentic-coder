package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildcontrol"
	"github.com/tngtech/oh-my-agentic-coder/internal/procidentity"
)

// TestReconcileDaemonOwnership_NoCacheRootNoop asserts the helper is a
// no-op when no cache scope was prepared (empty cacheRoot → no
// host-only build-control root → nothing to reconcile). The parent
// does not abort; no warning is printed.
func TestReconcileDaemonOwnership_NoCacheRootNoop(t *testing.T) {
	var stderr bytes.Buffer
	reconcileDaemonOwnership("", &stderr)
	if stderr.Len() != 0 {
		t.Errorf("empty cacheRoot produced stderr output (must be a silent no-op): %q", stderr.String())
	}
}

// TestReconcileDaemonOwnership_MissingDaemonsDirNoop asserts a missing
// daemons/ directory (fresh install) is a silent no-op — Reconcile
// returns nil and the helper prints nothing.
func TestReconcileDaemonOwnership_MissingDaemonsDirNoop(t *testing.T) {
	cacheRoot := t.TempDir()
	var stderr bytes.Buffer
	reconcileDaemonOwnership(cacheRoot, &stderr)
	if stderr.Len() != 0 {
		t.Errorf("missing daemons/ dir produced stderr output (must be a silent no-op): %q", stderr.String())
	}
}

// TestReconcileDaemonOwnership_PendingRecordRetired asserts the
// parent-startup reconciliation closes the PID-reuse window (ticket 07
// checklist item #6): a pending record (the parent crashed between
// wrapper launch and daemon registration) is retired (deleted) at
// startup. The pending record has no PID to verify; the unguessable
// marker makes blind deletion safe (buildcontrol/reconcile.go). The
// next build on the leaf re-arms a fresh pending record.
func TestReconcileDaemonOwnership_PendingRecordRetired(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/cache/gradle/leaf-pending"
	if err := buildcontrol.WritePendingDaemonRecord(cacheRoot, leaf, buildcontrol.DaemonRecord{
		State:         buildcontrol.DaemonStatePending,
		Marker:        "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		LeafDigest:    buildcontrol.HashLeaf(leaf),
		JDKExecutable: "/path/to/java",
		RequestID:      "req-pending-1234",
	}); err != nil {
		t.Fatalf("WritePendingDaemonRecord: %v", err)
	}

	var stderr bytes.Buffer
	reconcileDaemonOwnership(cacheRoot, &stderr)
	if stderr.Len() != 0 {
		t.Errorf("pending reconcile produced stderr (should be silent on success): %q", stderr.String())
	}
	// The pending record must be gone (retired = deleted).
	if _, err := buildcontrol.LoadDaemonRecord(cacheRoot, leaf); !errors.Is(err, buildcontrol.ErrNoDaemonRecord) {
		t.Errorf("after reconcile: LoadDaemonRecord err = %v, want ErrNoDaemonRecord (pending retired)", err)
	}
}

// TestReconcileDaemonOwnership_ActiveDeadPIDRetired asserts the
// PID-reuse window is closed for an active record whose process is
// conclusively dead: reconciliation retires (deletes) the record. The
// production verifier (procidentity.Verify) returns ErrNoSuchProcess
// for a PID that does not exist — a guaranteed-dead PID (a large
// unused value) exercises this arm without spawning a real process.
// This is the core "parent-startup reconciliation closes the
// PID-reuse window" assertion (ticket 07 checklist item #6): a parent
// that crashed after promoting the record but before retiring it leaves
// an active record pointing at a process that has since been reaped;
// the next startup must retire it so a PID-reused process is never
// signalled as if it were the OMAC-owned daemon.
func TestReconcileDaemonOwnership_ActiveDeadPIDRetired(t *testing.T) {
	// This test exercises the PRODUCTION procidentity.Verify path
	// (buildcontrol.defaultDaemonVerifier). A guaranteed-dead PID
	// (999999 — well above any realistic pid_max) returns
	// ErrNoSuchProcess on both Linux and macOS, so reconcile retires
	// the record. Skip on platforms where procidentity is unsupported
	// (the verifier returns ErrUnsupportedOS → the record is LEFT in
	// place, fail-closed — not this test's arm).
	if _, _, err := procidentity.Verify(999999, "/path/to/java", ""); err != nil && err == procidentity.ErrUnsupportedOS {
		t.Skipf("procidentity unsupported on this platform (ErrUnsupportedOS) — active-dead arm not exercisable")
	}

	cacheRoot := t.TempDir()
	leaf := "/cache/gradle/leaf-active-dead"
	if err := buildcontrol.WritePendingDaemonRecord(cacheRoot, leaf, buildcontrol.DaemonRecord{
		State:         buildcontrol.DaemonStatePending,
		Marker:        "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
		LeafDigest:    buildcontrol.HashLeaf(leaf),
		JDKExecutable: "/path/to/java",
		RequestID:      "req-active-dead-1234",
	}); err != nil {
		t.Fatalf("WritePendingDaemonRecord: %v", err)
	}
	if err := buildcontrol.PromoteDaemonRecord(cacheRoot, leaf, 999999, "start-dead"); err != nil {
		t.Fatalf("PromoteDaemonRecord: %v", err)
	}

	var stderr bytes.Buffer
	reconcileDaemonOwnership(cacheRoot, &stderr)
	if stderr.Len() != 0 {
		t.Errorf("active-dead reconcile produced stderr (should be silent on success): %q", stderr.String())
	}
	if _, err := buildcontrol.LoadDaemonRecord(cacheRoot, leaf); !errors.Is(err, buildcontrol.ErrNoDaemonRecord) {
		t.Errorf("after reconcile: LoadDaemonRecord err = %v, want ErrNoDaemonRecord (active-dead retired)", err)
	}
}

// TestReconcileDaemonOwnership_BadDaemonsDirFailsSoft asserts the
// fail-soft behavior: if ReconcileDaemonRecords returns an error (e.g.
// the daemons/ directory exists but is not readable — a setup bug), the
// helper logs a warning to stderr but does NOT panic or abort. The
// parent must still accept builds; the build-time handshake and the
// next startup will catch any stale records.
func TestReconcileDaemonOwnership_BadDaemonsDirFailsSoft(t *testing.T) {
	cacheRoot := t.TempDir()
	// Plant a daemons/ directory then make it unreadable (chmod 0).
	// The os.ReadDir inside ReconcileDaemonRecords fails with a
	// permission error → Reconcile returns an error → the helper logs
	// a warning. Create with a normal mode first (MkdirAll with 0o000
	// fails to create the parent chain), then chmod.
	daemonsDir := filepath.Join(buildcontrol.Root(cacheRoot), "daemons")
	if err := os.MkdirAll(daemonsDir, 0o700); err != nil {
		t.Fatalf("mkdir daemons: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(daemonsDir, 0o700) })
	if err := os.Chmod(daemonsDir, 0o000); err != nil {
		t.Fatalf("chmod daemons 0o000: %v", err)
	}

	var stderr bytes.Buffer
	reconcileDaemonOwnership(cacheRoot, &stderr)
	out := stderr.String()
	if !strings.Contains(out, "warning: daemon ownership reconciliation failed") {
		t.Errorf("bad daemons/ dir: stderr = %q, want a warning containing 'daemon ownership reconciliation failed'", out)
	}
	// The helper must NOT have panicked or aborted: it returned.
	// (Reaching this assertion means it did not call t.Fatal / panic.)
}
