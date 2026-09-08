package buildcontrol

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// validPending is a pending DaemonRecord with all required fields set
// and no PID/StartIdentity. Used as the starting point for the
// lifecycle tests.
func validPending() DaemonRecord {
	return DaemonRecord{
		State:         DaemonStatePending,
		Marker:        "unguessable-marker-abc123",
		LeafDigest:    "deadbeef",
		JDKExecutable: "/opt/jdk/bin/java",
		RequestID:     "req-1",
	}
}

// TestWritePendingDaemonRecord_RequiresPendingState asserts that a
// record whose State is not "pending" is rejected (the caller must
// promote/retire, not re-write pending).
func TestWritePendingDaemonRecord_RequiresPendingState(t *testing.T) {
	cacheRoot := t.TempDir()
	rec := validPending()
	rec.State = DaemonStateActive
	if err := WritePendingDaemonRecord(cacheRoot, "/leaf", rec); err == nil {
		t.Error("expected error for non-pending state, got nil")
	}
}

// TestWritePendingDaemonRecord_RequiresAllFields asserts the four
// required pending fields (marker, leaf_digest, jdk_executable,
// request_id) are all present.
func TestWritePendingDaemonRecord_RequiresAllFields(t *testing.T) {
	cacheRoot := t.TempDir()
	cases := []struct {
		name string
		mut  func(DaemonRecord) DaemonRecord
	}{
		{"missing marker", func(r DaemonRecord) DaemonRecord { r.Marker = ""; return r }},
		{"missing leaf digest", func(r DaemonRecord) DaemonRecord { r.LeafDigest = ""; return r }},
		{"missing jdk executable", func(r DaemonRecord) DaemonRecord { r.JDKExecutable = ""; return r }},
		{"missing request id", func(r DaemonRecord) DaemonRecord { r.RequestID = ""; return r }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := WritePendingDaemonRecord(cacheRoot, "/leaf", c.mut(validPending())); err == nil {
				t.Error("expected error for missing field, got nil")
			}
		})
	}
}

// TestWritePendingDaemonRecord_RejectsPIDAndStartIdentity asserts that
// a pending record must NOT carry PID or StartIdentity (those are set
// by PromoteDaemonRecord).
func TestWritePendingDaemonRecord_RejectsPIDAndStartIdentity(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Run("with pid", func(t *testing.T) {
		rec := validPending()
		rec.PID = 1234
		if err := WritePendingDaemonRecord(cacheRoot, "/leaf", rec); err == nil {
			t.Error("expected error for non-zero PID, got nil")
		}
	})
	t.Run("with start identity", func(t *testing.T) {
		rec := validPending()
		rec.StartIdentity = "99999"
		if err := WritePendingDaemonRecord(cacheRoot, "/leaf", rec); err == nil {
			t.Error("expected error for non-empty StartIdentity, got nil")
		}
	})
}

// TestWritePendingDaemonRecord_ThenLoad_ReadBackCorrectness asserts the
// round-trip: a written pending record loads back with the same
// fields, a fresh CreatedAt, nil PromotedAt/RetiredAt, and the correct
// LeafHash (derived from the canonical leaf).
func TestWritePendingDaemonRecord_ThenLoad_ReadBackCorrectness(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/cache/gradle/leaf"
	rec := validPending()
	if err := WritePendingDaemonRecord(cacheRoot, leaf, rec); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	got, err := LoadDaemonRecord(cacheRoot, leaf)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != DaemonStatePending {
		t.Errorf("State = %q, want %q", got.State, DaemonStatePending)
	}
	if got.Marker != rec.Marker {
		t.Errorf("Marker = %q, want %q", got.Marker, rec.Marker)
	}
	if got.LeafDigest != rec.LeafDigest {
		t.Errorf("LeafDigest = %q, want %q", got.LeafDigest, rec.LeafDigest)
	}
	if got.JDKExecutable != rec.JDKExecutable {
		t.Errorf("JDKExecutable = %q, want %q", got.JDKExecutable, rec.JDKExecutable)
	}
	if got.RequestID != rec.RequestID {
		t.Errorf("RequestID = %q, want %q", got.RequestID, rec.RequestID)
	}
	if got.PID != 0 {
		t.Errorf("PID = %d, want 0 (pending has no pid)", got.PID)
	}
	if got.StartIdentity != "" {
		t.Errorf("StartIdentity = %q, want empty (pending)", got.StartIdentity)
	}
	if got.LeafHash != HashLeaf(leaf) {
		t.Errorf("LeafHash = %q, want %q", got.LeafHash, HashLeaf(leaf))
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want set by WritePending")
	}
	if got.PromotedAt != nil {
		t.Error("PromotedAt non-nil on pending, want nil")
	}
	if got.RetiredAt != nil {
		t.Error("RetiredAt non-nil on pending, want nil")
	}
}

// TestWritePendingDaemonRecord_OverwritesStalePending asserts that a
// stale pending record (from a previous crashed build) is overwritten
// by a new pending write — the documented re-arm behaviour.
func TestWritePendingDaemonRecord_OverwritesStalePending(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	r1 := validPending()
	r1.RequestID = "req-old"
	if err := WritePendingDaemonRecord(cacheRoot, leaf, r1); err != nil {
		t.Fatal(err)
	}
	r2 := validPending()
	r2.RequestID = "req-new"
	if err := WritePendingDaemonRecord(cacheRoot, leaf, r2); err != nil {
		t.Fatalf("overwrite stale pending: %v", err)
	}
	got, err := LoadDaemonRecord(cacheRoot, leaf)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestID != "req-new" {
		t.Errorf("RequestID = %q, want req-new (overwrite)", got.RequestID)
	}
}

// TestWritePendingDaemonRecord_FailsOnActiveOwner asserts the fail-
// closed path: a pending write against a leaf that has an active
// record is rejected (the live owner must be retired first).
func TestWritePendingDaemonRecord_FailsOnActiveOwner(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	if err := WritePendingDaemonRecord(cacheRoot, leaf, validPending()); err != nil {
		t.Fatal(err)
	}
	if err := PromoteDaemonRecord(cacheRoot, leaf, 4242, "start-1"); err != nil {
		t.Fatal(err)
	}
	err := WritePendingDaemonRecord(cacheRoot, leaf, validPending())
	if err == nil {
		t.Fatal("expected active-owner error, got nil")
	}
	if !strings.Contains(err.Error(), "active") {
		t.Errorf("error %q does not mention active owner", err.Error())
	}
}

// TestPromoteDaemonRecord_PendingToActive asserts the promote sets
// PID, StartIdentity, PromotedAt, and State=active, and that the
// pre-existing pending fields are preserved.
func TestPromoteDaemonRecord_PendingToActive(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	rec := validPending()
	if err := WritePendingDaemonRecord(cacheRoot, leaf, rec); err != nil {
		t.Fatal(err)
	}
	if err := PromoteDaemonRecord(cacheRoot, leaf, 7777, "start-xyz"); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	got, err := LoadDaemonRecord(cacheRoot, leaf)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != DaemonStateActive {
		t.Errorf("State = %q, want %q", got.State, DaemonStateActive)
	}
	if got.PID != 7777 {
		t.Errorf("PID = %d, want 7777", got.PID)
	}
	if got.StartIdentity != "start-xyz" {
		t.Errorf("StartIdentity = %q, want start-xyz", got.StartIdentity)
	}
	if got.PromotedAt == nil || got.PromotedAt.IsZero() {
		t.Error("PromotedAt nil/zero, want set")
	}
	// Preserved pending fields.
	if got.Marker != rec.Marker {
		t.Errorf("Marker changed on promote: %q", got.Marker)
	}
	if got.RequestID != rec.RequestID {
		t.Errorf("RequestID changed on promote: %q", got.RequestID)
	}
}

// TestPromoteDaemonRecord_NotPendingFails asserts that promoting a
// record that is not pending (active, retired, or missing) is rejected.
func TestPromoteDaemonRecord_NotPendingFails(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"

	t.Run("missing record", func(t *testing.T) {
		err := PromoteDaemonRecord(cacheRoot, leaf, 1, "start")
		if !errors.Is(err, ErrNoDaemonRecord) {
			t.Errorf("err = %v, want ErrNoDaemonRecord", err)
		}
	})

	t.Run("already active", func(t *testing.T) {
		if err := WritePendingDaemonRecord(cacheRoot, leaf, validPending()); err != nil {
			t.Fatal(err)
		}
		if err := PromoteDaemonRecord(cacheRoot, leaf, 1, "start"); err != nil {
			t.Fatal(err)
		}
		err := PromoteDaemonRecord(cacheRoot, leaf, 2, "start2")
		if err == nil {
			t.Error("expected error promoting active record, got nil")
		}
	})
}

// TestRetireDaemonRecord_Idempotent asserts that retiring a missing or
// already-retired record is a no-op (the spec says stop succeeds
// idempotently when no owner is present).
func TestRetireDaemonRecord_Idempotent(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"

	t.Run("missing record is no-op", func(t *testing.T) {
		if err := RetireDaemonRecord(cacheRoot, leaf); err != nil {
			t.Errorf("retire missing: %v", err)
		}
	})

	t.Run("retire twice", func(t *testing.T) {
		if err := WritePendingDaemonRecord(cacheRoot, leaf, validPending()); err != nil {
			t.Fatal(err)
		}
		if err := RetireDaemonRecord(cacheRoot, leaf); err != nil {
			t.Fatalf("first retire: %v", err)
		}
		// File is gone.
		if _, err := os.Stat(DaemonPath(cacheRoot, leaf)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("record file still exists after retire: %v", err)
		}
		// Second retire is a no-op.
		if err := RetireDaemonRecord(cacheRoot, leaf); err != nil {
			t.Errorf("second retire: %v", err)
		}
	})
}

// TestRetireDaemonRecord_FromActive asserts that retiring an active
// record deletes the file (retire = delete, per the doc on
// RetireDaemonRecord).
func TestRetireDaemonRecord_FromActive(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	if err := WritePendingDaemonRecord(cacheRoot, leaf, validPending()); err != nil {
		t.Fatal(err)
	}
	if err := PromoteDaemonRecord(cacheRoot, leaf, 99, "start-9"); err != nil {
		t.Fatal(err)
	}
	if err := RetireDaemonRecord(cacheRoot, leaf); err != nil {
		t.Fatalf("retire active: %v", err)
	}
	if _, err := os.Stat(DaemonPath(cacheRoot, leaf)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("record file still exists after retire from active")
	}
}

// TestLoadDaemonRecord_MissingReturnsErrNoDaemonRecord asserts the
// sentinel: a missing record file is distinguishable from a malformed
// one.
func TestLoadDaemonRecord_MissingReturnsErrNoDaemonRecord(t *testing.T) {
	cacheRoot := t.TempDir()
	_, err := LoadDaemonRecord(cacheRoot, "/nope")
	if !errors.Is(err, ErrNoDaemonRecord) {
		t.Errorf("err = %v, want ErrNoDaemonRecord", err)
	}
}

// TestLoadDaemonRecord_MalformedReturnsWrappedError asserts a corrupt
// record surfaces as a non-sentinel error (callers must NOT treat it
// as "no record"; it means trusted state was corrupted/tampered).
func TestLoadDaemonRecord_MalformedReturnsWrappedError(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	if _, err := EnsureRoot(cacheRoot); err != nil {
		t.Fatal(err)
	}
	path := DaemonPath(cacheRoot, leaf)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadDaemonRecord(cacheRoot, leaf)
	if err == nil {
		t.Fatal("expected error for malformed record, got nil")
	}
	if errors.Is(err, ErrNoDaemonRecord) {
		t.Errorf("malformed record returned ErrNoDaemonRecord (should be a parse error)")
	}
}

// TestDeleteDaemonRecord_Idempotent asserts DeleteDaemonRecord is a
// no-op on a missing file (used by reconciliation).
func TestDeleteDaemonRecord_Idempotent(t *testing.T) {
	cacheRoot := t.TempDir()
	if err := DeleteDaemonRecord(cacheRoot, "/nope"); err != nil {
		t.Errorf("delete missing: %v", err)
	}
	if err := WritePendingDaemonRecord(cacheRoot, "/leaf", validPending()); err != nil {
		t.Fatal(err)
	}
	if err := DeleteDaemonRecord(cacheRoot, "/leaf"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(DaemonPath(cacheRoot, "/leaf")); !errors.Is(err, os.ErrNotExist) {
		t.Error("record still exists after delete")
	}
}

// TestWritePendingDaemonRecord_ConcurrentWriters asserts that two
// concurrent pending writes to the same leaf do not corrupt the record
// (atomic rename ensures one wins) and the active-owner check prevents
// a second pending write after a promote. Two goroutines writing
// pending before any promote: both may succeed (each overwrites the
// other's pending — re-arm semantics), but the file MUST be valid
// JSON afterwards. This documents the concurrency contract: pending
// writes against an UNOWNED leaf are last-writer-wins; pending writes
// against an ACTIVE leaf fail closed.
func TestWritePendingDaemonRecord_ConcurrentWriters(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/shared/leaf"
	var wg sync.WaitGroup
	var ok, fail int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := validPending()
			rec.RequestID = "req-" + string(rune('A'+i))
			if err := WritePendingDaemonRecord(cacheRoot, leaf, rec); err == nil {
				atomic.AddInt32(&ok, 1)
			} else {
				atomic.AddInt32(&fail, 1)
			}
		}(i)
	}
	wg.Wait()
	// At least one writer succeeded.
	if atomic.LoadInt32(&ok) == 0 {
		t.Fatal("no writer succeeded")
	}
	// The file is valid JSON and loadable.
	got, err := LoadDaemonRecord(cacheRoot, leaf)
	if err != nil {
		t.Fatalf("load after concurrent writes: %v", err)
	}
	if got.State != DaemonStatePending {
		t.Errorf("State = %q, want pending", got.State)
	}
}

// TestWritePendingDaemonRecord_RecordFileMode asserts the record file
// is mode 0o600 (owner-only; it carries the unguessable marker).
func TestWritePendingDaemonRecord_RecordFileMode(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	if err := WritePendingDaemonRecord(cacheRoot, leaf, validPending()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(DaemonPath(cacheRoot, leaf))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != LockFileMode {
		t.Errorf("record mode = %o, want %o", info.Mode().Perm(), LockFileMode)
	}
}

// TestWritePendingDaemonRecord_CreatesParentDir asserts that writing a
// record for a leaf whose daemons/ dir does not yet exist (fresh
// install) succeeds — EnsureRoot is called.
func TestWritePendingDaemonRecord_CreatesParentDir(t *testing.T) {
	cacheRoot := t.TempDir()
	// No build-control tree yet.
	if _, err := os.Stat(filepath.Join(cacheRoot, RootName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expected no build-control tree yet")
	}
	if err := WritePendingDaemonRecord(cacheRoot, "/leaf", validPending()); err != nil {
		t.Fatalf("write with missing parent: %v", err)
	}
	if _, err := os.Stat(DaemonPath(cacheRoot, "/leaf")); err != nil {
		t.Errorf("record not created: %v", err)
	}
}

// TestPromoteDaemonRecord_RejectsZeroOrNegativePID asserts the promote
// validates the pid (a zero/negative pid is a caller bug, not a
// verification result).
func TestPromoteDaemonRecord_RejectsZeroOrNegativePID(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	if err := WritePendingDaemonRecord(cacheRoot, leaf, validPending()); err != nil {
		t.Fatal(err)
	}
	if err := PromoteDaemonRecord(cacheRoot, leaf, 0, "start"); err == nil {
		t.Error("expected error for zero pid, got nil")
	}
	if err := PromoteDaemonRecord(cacheRoot, leaf, -1, "start"); err == nil {
		t.Error("expected error for negative pid, got nil")
	}
}

// TestPromoteDaemonRecord_RejectsEmptyStartIdentity asserts the promote
// requires a non-empty start identity (procidentity needs it to detect
// PID reuse on subsequent verifications).
func TestPromoteDaemonRecord_RejectsEmptyStartIdentity(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf"
	if err := WritePendingDaemonRecord(cacheRoot, leaf, validPending()); err != nil {
		t.Fatal(err)
	}
	if err := PromoteDaemonRecord(cacheRoot, leaf, 1, ""); err == nil {
		t.Error("expected error for empty start identity, got nil")
	}
}

// keep time imported for the CreatedAt.IsZero checks above.
var _ = time.Now
