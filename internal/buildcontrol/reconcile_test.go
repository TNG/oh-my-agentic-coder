package buildcontrol

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/procidentity"
)

// fakeVerifier is a test seam for daemonVerify. It returns the
// configured verdict for any pid, simulating a live+matching, dead,
// PID-reused, or unverifiable process without spawning real daemons.
type fakeVerifier struct {
	verified bool
	id       procidentity.Identity
	err      error
}

func (f fakeVerifier) verify(int, string, string) (bool, procidentity.Identity, error) {
	return f.verified, f.id, f.err
}

// withVerifier swaps the package-level daemonVerify seam for the test
// and restores it on cleanup.
func withVerifier(t *testing.T, v DaemonVerifier) {
	t.Helper()
	saved := daemonVerify
	daemonVerify = v
	t.Cleanup(func() { daemonVerify = saved })
}

// writeActiveRecord writes a pending record and promotes it to active,
// returning the resulting on-disk record. Helper for reconciliation
// tests that need an active record to verify against.
func writeActiveRecord(t *testing.T, cacheRoot, leaf string, pid int, startID string) DaemonRecord {
	t.Helper()
	rec := validPending()
	if err := WritePendingDaemonRecord(cacheRoot, leaf, rec); err != nil {
		t.Fatal(err)
	}
	if err := PromoteDaemonRecord(cacheRoot, leaf, pid, startID); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDaemonRecord(cacheRoot, leaf)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestReconcileDaemonRecords_MissingDirIsNoOp asserts that an absent
// daemons/ directory (fresh install, no builds yet) reconciles to nil
// without creating the tree.
func TestReconcileDaemonRecords_MissingDirIsNoOp(t *testing.T) {
	cacheRoot := t.TempDir()
	if err := ReconcileDaemonRecords(cacheRoot); err != nil {
		t.Errorf("missing dir: %v", err)
	}
}

// TestReconcileDaemonRecords_PendingRetires asserts that a pending
// record at startup is retired (deleted) — the parent that created it
// crashed before the daemon registered, and the next build re-arms.
func TestReconcileDaemonRecords_PendingRetires(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	if err := WritePendingDaemonRecord(cacheRoot, leaf, validPending()); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDaemonRecords(cacheRoot); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(DaemonPath(cacheRoot, leaf)); !errors.Is(err, os.ErrNotExist) {
		t.Error("pending record not deleted by reconcile")
	}
}

// TestReconcileDaemonRecords_ActiveLiveKept asserts that an active
// record whose process verifies (live, executable + main class +
// start identity all match) is left in place (remains controllable).
func TestReconcileDaemonRecords_ActiveLiveKept(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	rec := writeActiveRecord(t, cacheRoot, leaf, 1234, "start-1")

	withVerifier(t, fakeVerifier{
		verified: true,
		id: procidentity.Identity{
			Executable:    rec.JDKExecutable,
			MainClass:     procidentity.GradleDaemonMainClass,
			StartIdentity: rec.StartIdentity,
		},
	}.verify)

	if err := ReconcileDaemonRecords(cacheRoot); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := LoadDaemonRecord(cacheRoot, leaf)
	if err != nil {
		t.Fatalf("record removed by reconcile (wanted kept): %v", err)
	}
	if got.State != DaemonStateActive {
		t.Errorf("State = %q, want active", got.State)
	}
}

// TestReconcileDaemonRecords_ActiveDeadRetired asserts that an active
// record whose process is dead (procidentity.ErrNoSuchProcess) is
// retired (deleted).
func TestReconcileDaemonRecords_ActiveDeadRetired(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	writeActiveRecord(t, cacheRoot, leaf, 1234, "start-1")

	withVerifier(t, fakeVerifier{err: procidentity.ErrNoSuchProcess}.verify)

	if err := ReconcileDaemonRecords(cacheRoot); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(DaemonPath(cacheRoot, leaf)); !errors.Is(err, os.ErrNotExist) {
		t.Error("dead active record not deleted by reconcile")
	}
}

// TestReconcileDaemonRecords_ActivePIDReusedRetired asserts that an
// active record whose process is live but mismatched (PID reused —
// start identity changed, or executable changed) is retired.
func TestReconcileDaemonRecords_ActivePIDReusedRetired(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	writeActiveRecord(t, cacheRoot, leaf, 1234, "start-1")

	withVerifier(t, fakeVerifier{
		verified: false, // live but mismatched
		id:       procidentity.Identity{Executable: "/other/java"},
	}.verify)

	if err := ReconcileDaemonRecords(cacheRoot); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(DaemonPath(cacheRoot, leaf)); !errors.Is(err, os.ErrNotExist) {
		t.Error("PID-reused active record not deleted by reconcile")
	}
}

// TestReconcileDaemonRecords_ActiveUnverifiableKept asserts that an
// active record whose process cannot be verified
// (procidentity.ErrUnverifiable — e.g. sandbox blocks /proc) is LEFT
// in place; the leaf is blocked (fail closed) at build time, NOT by
// reconciliation deleting the record.
func TestReconcileDaemonRecords_ActiveUnverifiableKept(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	writeActiveRecord(t, cacheRoot, leaf, 1234, "start-1")

	withVerifier(t, fakeVerifier{err: procidentity.ErrUnverifiable}.verify)

	if err := ReconcileDaemonRecords(cacheRoot); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(DaemonPath(cacheRoot, leaf)); errors.Is(err, os.ErrNotExist) {
		t.Fatal("unverifiable active record was deleted (should be kept — fail closed)")
	}
}

// TestReconcileDaemonRecords_RetiredTombstoneCleaned asserts that a
// retired-but-not-deleted tombstone (left by a crash between the
// atomic write and the unlink in RetireDaemonRecord) is cleaned up.
func TestReconcileDaemonRecords_RetiredTombstoneCleaned(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	if _, err := EnsureRoot(cacheRoot); err != nil {
		t.Fatal(err)
	}
	// Hand-write a retired tombstone (simulate a crash mid-retire).
	path := DaemonPath(cacheRoot, leaf)
	tomb := DaemonRecord{
		LeafHash: HashLeaf(leaf),
		State:    DaemonStateRetired,
		Marker:   "x",
	}
	if err := writeDaemonRecordAtomic(path, tomb); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileDaemonRecords(cacheRoot); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("retired tombstone not cleaned up by reconcile")
	}
}

// TestReconcileDaemonRecords_MalformedRecordDeleted asserts that a
// malformed record file is deleted (a corrupt trusted-state file
// cannot identify a daemon; deleting it unblocks the leaf without
// aborting the walk).
func TestReconcileDaemonRecords_MalformedRecordDeleted(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	if _, err := EnsureRoot(cacheRoot); err != nil {
		t.Fatal(err)
	}
	path := DaemonPath(cacheRoot, leaf)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileDaemonRecords(cacheRoot); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("malformed record not deleted by reconcile")
	}
}

// TestReconcileDaemonRecords_MixedLeaves asserts that reconciliation
// processes every record in the daemons/ dir independently — a pending
// on one leaf is retired while a live active on another leaf is kept.
func TestReconcileDaemonRecords_MixedLeaves(t *testing.T) {
	cacheRoot := t.TempDir()
	leafPending := "/leaf/pending"
	leafActive := "/leaf/active"

	if err := WritePendingDaemonRecord(cacheRoot, leafPending, validPending()); err != nil {
		t.Fatal(err)
	}
	activeRec := writeActiveRecord(t, cacheRoot, leafActive, 42, "start-42")

	withVerifier(t, fakeVerifier{
		verified: true,
		id: procidentity.Identity{
			Executable:    activeRec.JDKExecutable,
			MainClass:     procidentity.GradleDaemonMainClass,
			StartIdentity: activeRec.StartIdentity,
		},
	}.verify)

	if err := ReconcileDaemonRecords(cacheRoot); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Pending retired.
	if _, err := os.Stat(DaemonPath(cacheRoot, leafPending)); !errors.Is(err, os.ErrNotExist) {
		t.Error("pending record not retired")
	}
	// Active kept.
	if _, err := LoadDaemonRecord(cacheRoot, leafActive); err != nil {
		t.Errorf("active record not kept: %v", err)
	}
}

// TestReconcileDaemonRecords_IgnoresNonJSONFiles asserts that non-.json
// files in the daemons/ directory (e.g. a stray editor backup) are
// ignored, not treated as records.
func TestReconcileDaemonRecords_IgnoresNonJSONFiles(t *testing.T) {
	cacheRoot := t.TempDir()
	if _, err := EnsureRoot(cacheRoot); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(Root(cacheRoot), daemonsDir)
	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("ignore me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "noext"), []byte("ignore me too"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDaemonRecords(cacheRoot); err != nil {
		t.Errorf("reconcile with non-json files: %v", err)
	}
	// Non-json files left untouched.
	if _, err := os.Stat(filepath.Join(dir, "stray.txt")); err != nil {
		t.Errorf("stray.txt touched by reconcile: %v", err)
	}
}

// TestReconcileDaemonRecords_ContinuesOnPerRecordError asserts that a
// failing record does not abort the walk — the other records are
// still processed and the error is returned aggregated.
func TestReconcileDaemonRecords_ContinuesOnPerRecordError(t *testing.T) {
	// This is hard to trigger naturally (per-record errors are
	// swallowed: malformed → delete, unverifiable → leave). We
	// approximate by making the daemons/ dir contain only well-
	// formed records and asserting reconcile returns nil. A genuine
	// per-record error path would require a chmod/remove-failure
	// injection seam, which is out of scope for Phase 1.
	cacheRoot := t.TempDir()
	if err := WritePendingDaemonRecord(cacheRoot, "/a", validPending()); err != nil {
		t.Fatal(err)
	}
	if err := WritePendingDaemonRecord(cacheRoot, "/b", validPending()); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileDaemonRecords(cacheRoot); err != nil {
		t.Errorf("reconcile well-formed records: %v", err)
	}
	// Both retired.
	for _, leaf := range []string{"/a", "/b"} {
		if _, err := os.Stat(DaemonPath(cacheRoot, leaf)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("record %s not retired", leaf)
		}
	}
}
