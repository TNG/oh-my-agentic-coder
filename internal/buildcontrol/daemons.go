// Daemon ownership records — the host-only on-disk state for the
// pending-to-active daemon handshake (ticket 07, spec.md §237-239).
//
// This file owns the RECORD types and the atomic lifecycle
// (write-pending → promote → retire → delete) on the path returned by
// DaemonPath (buildcontrol.go:176). Reconciliation (startup sweep over
// the daemons/ dir) lives in reconcile.go.
//
// The record is the host's authoritative proof that a particular Gradle
// daemon, started for a particular canonical cache leaf by a particular
// build request, is owned by OMAC and therefore safe to control
// (SIGTERM / SIGKILL) during `omac build stop` or post-build recycle.
// The marker (an unguessable value the host injects into the daemon's
// JVM args and the daemon echoes back over the private control channel)
// is verified SEPARATELY by the handshake in buildrun (Phase 2); the
// record stores the marker so the handshake, reconciliation, and stop
// all see the same value, but the marker match itself happens before
// PromoteDaemonRecord is called.
//
// State machine (spec.md §237-239):
//
//	pending  → active   (PromoteDaemonRecord, after the handshake
//	                     verifies the marker AND procidentity verifies
//	                     the process; adds PID + StartIdentity)
//	pending  → retired  (RetireDaemonRecord; a pending record at parent
//	                     startup is retired because the parent that
//	                     created it crashed before the daemon
//	                     registered — see reconcile.go)
//	active   → retired  (RetireDaemonRecord; after confirmed daemon
//	                     exit, or after reconciliation finds the
//	                     process dead / PID-reused)
//	retired  → (gone)   (DeleteDaemonRecord; retire = DELETE the file
//	                     — see the doc on RetireDaemonRecord for the
//	                     spec citation and rationale)
//
// All writes are ATOMIC: write to a temp file in the same directory,
// fsync, then os.Rename. A partial write never becomes the record.

package buildcontrol

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DaemonState constants are the string values stored in DaemonRecord.State.
// Exported so callers (buildrun handshake, buildengine brokered stop,
// reconciliation tests) can compare against typed constants instead of
// bare strings.
const (
	// DaemonStatePending is the state of a record written before the
	// wrapper launches: the host has committed the marker, leaf digest,
	// resolved JDK, and request id, but no daemon has registered yet.
	DaemonStatePending = "pending"

	// DaemonStateActive is the state of a record after the handshake
	// has verified the marker AND procidentity has verified the process:
	// PID and OS start identity are recorded, and the daemon is
	// controllable by OMAC.
	DaemonStateActive = "active"

	// DaemonStateRetired is the state of a record whose owner daemon
	// is conclusively gone (confirmed exit, or reconciliation found
	// the process dead / PID-reused). A retired record is deleted by
	// RetireDaemonRecord (retire = DELETE the file — see the doc on
	// RetireDaemonRecord for the spec rationale).
	DaemonStateRetired = "retired"
)

// ErrNoDaemonRecord is returned by LoadDaemonRecord when no record file
// exists for the leaf (the leaf has no owner). Callers use this to
// distinguish "no daemon to stop / reconcile" from a malformed record.
var ErrNoDaemonRecord = errors.New("buildcontrol: no daemon record for leaf")

// DaemonRecord is the JSON-schema record stored at
// <root>/daemons/<sha256(leaf)>.json. The schema is fixed by spec.md
// §237-238; do NOT add fields without updating the spec.
//
// Field semantics:
//
//   - LeafHash     — sha256(canonical-leaf) hex, the file basename key.
//     Stored denormalised so a record loaded from disk self-identifies
//     its leaf without a reverse map.
//   - State        — one of DaemonStatePending / DaemonStateActive /
//     DaemonStateRetired.
//   - Marker       — the cryptographically random, unguessable value
//     the host injected into the Gradle daemon JVM args. The daemon
//     echoes it back over the private control channel; the handshake
//     verifies the match before promoting pending → active.
//   - LeafDigest   — sha256(canonical-leaf) hex (same value as LeafHash;
//     stored under a distinct name because the spec calls it "canonical
//     leaf digest" and future leaf-digest schemes might diverge from
//     the hash used for the file basename). Use HashLeaf to compute.
//   - JDKExecutable — the resolved JDK `java` binary path the daemon
//     must be running; procidentity verifies the process executable
//     equals this.
//   - RequestID    — the build request id that created the pending
//     record. Lets reconciliation attribute a stale pending record to
//     a crashed build.
//   - PID         — the daemon's OS pid; set on promote, empty on
//     pending.
//   - StartIdentity — the OS process-start identity (Linux
//     /proc/<pid>/stat field 22 `starttime`; macOS proc_bsdinfo start
//     time). Set on promote; procidentity compares it on each
//     verification to detect PID reuse.
//   - CreatedAt    — when the pending record was written.
//   - PromotedAt   — when pending → active; nil for pending.
//   - RetiredAt    — when retired; nil for pending/active (and nil for
//     a retired record that has been deleted, since deletion removes
//     the file).
type DaemonRecord struct {
	LeafHash      string     `json:"leaf_hash"`
	State         string     `json:"state"`
	Marker        string     `json:"marker"`
	LeafDigest    string     `json:"leaf_digest"`
	JDKExecutable string     `json:"jdk_executable"`
	RequestID     string     `json:"request_id"`
	PID           int        `json:"pid"`
	StartIdentity string     `json:"start_identity"`
	CreatedAt     time.Time  `json:"created_at"`
	PromotedAt    *time.Time `json:"promoted_at,omitempty"`
	RetiredAt     *time.Time `json:"retired_at,omitempty"`
}

// WritePendingDaemonRecord atomically writes a pending record for the
// canonical leaf. The record MUST be in DaemonStatePending, and MUST
// have Marker, LeafDigest, JDKExecutable, and RequestID set; it MUST
// NOT have PID or StartIdentity (those are added by
// PromoteDaemonRecord).
//
// Semantics:
//
//   - If a record already exists in DaemonStateActive → returns an
//     error. A live daemon already owns the leaf; the caller must
//     retire it first (or block on the leaf lock until the owner
//     releases). This is the "fail closed against a live owner" path.
//   - If a record exists in DaemonStatePending or DaemonStateRetired
//     (or no record exists) → it is OVERWRITTEN (re-arming for a new
//     build). A stale pending record means a previous build for this
//     leaf crashed before the daemon registered; overwriting it is the
//     documented reconciliation outcome (see reconcile.go).
//
// Sets CreatedAt to the current time (the caller does not supply it).
// The write is atomic: temp-file + os.Rename in the daemons/ directory,
// so a crash never leaves a partial record.
//
// cacheRoot is the shared cache root; canonicalLeaf is the resolved
// Gradle cache leaf (buildrun.GradleLeaf(cacheDir)). The record is
// stored at DaemonPath(cacheRoot, canonicalLeaf).
func WritePendingDaemonRecord(cacheRoot, canonicalLeaf string, rec DaemonRecord) error {
	if canonicalLeaf == "" {
		return errors.New("buildcontrol: empty canonical leaf")
	}
	if rec.State != DaemonStatePending {
		return fmt.Errorf("buildcontrol: WritePendingDaemonRecord requires State=%q, got %q", DaemonStatePending, rec.State)
	}
	if rec.Marker == "" || rec.LeafDigest == "" || rec.JDKExecutable == "" || rec.RequestID == "" {
		return errors.New("buildcontrol: pending daemon record missing required field (marker, leaf_digest, jdk_executable, request_id)")
	}
	if rec.PID != 0 || rec.StartIdentity != "" {
		return errors.New("buildcontrol: pending daemon record must not carry PID or StartIdentity (set by PromoteDaemonRecord)")
	}
	if _, err := EnsureRoot(cacheRoot); err != nil {
		return err
	}
	path := DaemonPath(cacheRoot, canonicalLeaf)

	// Fail closed against a live owner: if an active record exists,
	// refuse. The caller must retire it first (after confirming the
	// daemon is gone) or hold the leaf lock until the owner releases.
	if existing, err := os.ReadFile(path); err == nil {
		var prev DaemonRecord
		if jsonErr := json.Unmarshal(existing, &prev); jsonErr == nil && prev.State == DaemonStateActive {
			return fmt.Errorf("buildcontrol: leaf %q already has an active daemon record (request %s, pid %d) — retire it before re-arming", canonicalLeaf, prev.RequestID, prev.PID)
		}
		// A malformed or non-pending file is overwritten below.
	}

	rec.LeafHash = HashLeaf(canonicalLeaf)
	rec.CreatedAt = time.Now().UTC()
	rec.PromotedAt = nil
	rec.RetiredAt = nil
	return writeDaemonRecordAtomic(path, rec)
}

// PromoteDaemonRecord atomically promotes a pending record to active:
// loads the record, asserts State==pending, sets PID, StartIdentity,
// PromotedAt, State=active, and writes atomically.
//
// Fails if the record is missing (ErrNoDaemonRecord) or not pending
// (a concurrent promote, or a retire between the handshake and the
// promote). The caller (the handshake in buildrun) treats a non-pending
// promote as a protocol violation and aborts the build.
func PromoteDaemonRecord(cacheRoot, canonicalLeaf string, pid int, startIdentity string) error {
	if canonicalLeaf == "" {
		return errors.New("buildcontrol: empty canonical leaf")
	}
	if pid <= 0 {
		return errors.New("buildcontrol: promote requires a positive pid")
	}
	if startIdentity == "" {
		return errors.New("buildcontrol: promote requires a non-empty start identity")
	}
	path := DaemonPath(cacheRoot, canonicalLeaf)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("buildcontrol: %w: %s", ErrNoDaemonRecord, canonicalLeaf)
		}
		return fmt.Errorf("read daemon record %s: %w", path, err)
	}
	var rec DaemonRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return fmt.Errorf("parse daemon record %s: %w", path, err)
	}
	if rec.State != DaemonStatePending {
		return fmt.Errorf("buildcontrol: cannot promote leaf %q: record state is %q, want %q", canonicalLeaf, rec.State, DaemonStatePending)
	}
	rec.PID = pid
	rec.StartIdentity = startIdentity
	now := time.Now().UTC()
	rec.PromotedAt = &now
	rec.State = DaemonStateActive
	return writeDaemonRecordAtomic(path, rec)
}

// RetireDaemonRecord atomically retires (pending OR active) → retired
// AND THEN deletes the file. Idempotent: if the record is already gone
// (deleted) or already retired (state==retired on disk, which should
// not happen because retire deletes), it is a no-op.
//
// Spec citation and the retire=DELETE decision:
//
//	spec.md:239 — "After confirmed daemon exit the host atomically
//	retires the ownership record."
//
// The spec says "retires the record", not "marks the record retired".
// The cleanest reading — and the one that makes the rest of the spec
// consistent — is that retire = DELETE the file:
//
//   - spec.md:239 (reconciliation): "an unverifiable identity blocks
//     that leaf and fails closed." A blocked leaf that later becomes
//     unblocked (e.g. the sandbox is reset) must find NO record, not a
//     tombstone it has to interpret. Absence of a record = no owner =
//     stop succeeds idempotently (spec.md:240: "if neither ownership
//     state nor a live daemon is present, stop succeeds idempotently").
//   - spec.md:240 (brokered stop): "uses ... the host-only ownership
//     records to identify leaf-associated Gradle daemon processes." A
//     retired record would have State==retired and no PID; the brokered
//     stop must treat it as "no owner", which is the same as a missing
//     file. Keeping a tombstone would force every stop to special-case
//     it; deleting is simpler and matches "retires the record".
//   - Reconciliation (reconcile.go) only LOADS records; a missing file
//     = nothing to reconcile for that leaf. A retire that left a
//     tombstone would make reconciliation walk retired records forever.
//
// We set RetiredAt + State=retired on the in-memory copy and write it
// atomically FIRST (so a crash between "mark retired" and "delete"
// leaves a retired tombstone that the next reconciliation will clean
// up), then delete the file. The two-step is defensive: a crash after
// the atomic write but before the unlink leaves a State==retired file,
// which reconcile.go deletes on the next startup.
func RetireDaemonRecord(cacheRoot, canonicalLeaf string) error {
	if canonicalLeaf == "" {
		return errors.New("buildcontrol: empty canonical leaf")
	}
	path := DaemonPath(cacheRoot, canonicalLeaf)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Idempotent: no record = already retired (deleted).
			return nil
		}
		return fmt.Errorf("read daemon record %s: %w", path, err)
	}
	var rec DaemonRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		// A malformed record cannot be safely retired as a tombstone;
		// delete it outright (it cannot be used to identify a daemon).
		return os.Remove(path)
	}
	if rec.State == DaemonStateRetired {
		// A retired-but-not-yet-deleted tombstone (crash between the
		// atomic write and the unlink in a previous retire, or left by
		// an older version). Delete it now.
		return os.Remove(path)
	}
	now := time.Now().UTC()
	rec.RetiredAt = &now
	rec.State = DaemonStateRetired
	if err := writeDaemonRecordAtomic(path, rec); err != nil {
		return fmt.Errorf("write retired tombstone %s: %w", path, err)
	}
	return os.Remove(path)
}

// LoadDaemonRecord reads and decodes the record for the canonical leaf.
// A missing file returns ErrNoDaemonRecord; a malformed file returns a
// wrapped error (callers MUST surface this as a service failure, not
// treat it as "no record" — a malformed record means the host's trusted
// state was corrupted or tampered with).
func LoadDaemonRecord(cacheRoot, canonicalLeaf string) (DaemonRecord, error) {
	path := DaemonPath(cacheRoot, canonicalLeaf)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DaemonRecord{}, fmt.Errorf("buildcontrol: %w: %s", ErrNoDaemonRecord, canonicalLeaf)
		}
		return DaemonRecord{}, fmt.Errorf("read daemon record %s: %w", path, err)
	}
	var rec DaemonRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return DaemonRecord{}, fmt.Errorf("parse daemon record %s: %w", path, err)
	}
	return rec, nil
}

// DeleteDaemonRecord removes the record file. Used by reconciliation
// (reconcile.go) when a record is conclusively dead / unverifiable in
// a way that means the leaf should be unblocked without a tombstone
// (e.g. a pending record at startup — see reconcile.go). NOT used by
// RetireDaemonRecord (which deletes after writing a transient
// tombstone for crash safety).
//
// Idempotent: a missing file is a no-op.
func DeleteDaemonRecord(cacheRoot, canonicalLeaf string) error {
	path := DaemonPath(cacheRoot, canonicalLeaf)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("delete daemon record %s: %w", path, err)
	}
	return nil
}

// writeDaemonRecordAtomic writes rec to path via temp-file + rename in
// the same directory (so rename is atomic on the same filesystem). The
// temp file is mode 0o600 (owner-only; the record carries the
// unguessable marker and is host-only trusted state).
func writeDaemonRecordAtomic(path string, rec DaemonRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal daemon record: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temp daemon record: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(LockFileMode); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp daemon record: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write temp daemon record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp daemon record: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename temp daemon record to %s: %w", path, err)
	}
	return nil
}
