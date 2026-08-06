// Parent-startup reconciliation of daemon ownership records (ticket 07,
// spec.md §239). ReconcileDaemonRecords walks the daemons/ directory at
// parent startup (before the broker accepts builds) and brings the
// on-disk state in line with reality: a record whose owner process is
// conclusively dead or PID-reused is retired (deleted); a live matching
// process is left in place (remains controllable); an unverifiable
// process is left in place BUT the leaf is blocked (fail closed — the
// block is enforced at build time when a build on that leaf finds an
// active-but-unverifiable record, NOT by Reconcile itself).
//
// The pending-to-active handshake (spec.md §237) closes the
// parent-crash window between daemon creation and ownership
// registration: a pending record at startup means the parent that
// created it crashed BEFORE the daemon registered, so there is no PID
// to verify against. The marker is unguessable, so a pending record
// cannot be reconciled by marker at startup (there is no process to
// echo it back). Reconcile therefore RETIRES (deletes) a pending
// record: the build that created it is gone, and the next build on
// that leaf re-arms a fresh pending record. (spec.md §239: "a pending
// record is reconciled by its unguessable owner marker" — read as:
// the unguessable marker is what makes it SAFE to delete a pending
// record without verifying a process, because no other build can
// claim it; the next build re-arms cleanly.)

package buildcontrol

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/procidentity"
)

// DaemonVerifier is the procidentity seam ReconcileDaemonRecords uses
// to verify a record's process. Production wires
// procidentity.Verify; tests inject a fake so reconciliation tests do
// not need real processes.
//
// The contract mirrors procidentity.Verify:
//
//		Verify(pid, expectedJDKExecutable, expectedStart) (verified bool, id Identity, err error)
//
//	  - verified=true  → process is live and matches (executable,
//	    main class, and — when expectedStart is non-empty — start
//	    identity).
//	  - verified=false → process is live but does NOT match (executable
//	    mismatch, main class missing, or start-identity changed / PID
//	    reused). Reconcile retires the record.
//	  - err == procidentity.ErrNoSuchProcess → the pid is not alive.
//	    Reconcile retires the record.
//	  - err == procidentity.ErrUnverifiable → the platform cannot
//	    determine the identity (e.g. a sandbox blocks /proc or libproc).
//	    Reconcile leaves the record; the leaf is blocked (fail closed)
//	    at build time.
//	  - any other err → treated like ErrUnverifiable (leave the record,
//	    block the leaf).
type DaemonVerifier func(pid int, expectedJDKExecutable, expectedStart string) (bool, procidentity.Identity, error)

// defaultDaemonVerifier is procidentity.Verify, captured at package
// init so tests can swap daemonVerify without importing procidentity
// into every test file.
var defaultDaemonVerifier DaemonVerifier = func(pid int, exe, start string) (bool, procidentity.Identity, error) {
	return procidentity.Verify(pid, exe, start)
}

// daemonVerify is the swappable seam. Tests swap it; production calls
// through defaultDaemonVerifier.
var daemonVerify DaemonVerifier = defaultDaemonVerifier

// ReconcileDaemonRecords walks the daemons/ directory under the
// build-control root and brings every record in line with the live
// process state. Call this at parent startup (start / serve), before
// the broker accepts builds.
//
// Per spec.md §239:
//
//   - pending  → retire (delete). The parent that created it crashed
//     before the daemon registered; the unguessable marker makes it
//     safe to delete without verifying a process. The next build on
//     the leaf re-arms a fresh pending record.
//   - active + live + identity matches → leave (remains controllable).
//   - active + dead (ErrNoSuchProcess) → retire (delete).
//   - active + PID reused / executable mismatch (verified=false) →
//     retire (delete).
//   - active + unverifiable (ErrUnverifiable or other error) → leave
//     (the leaf is blocked, fail closed; the block is enforced at
//     build time, not here).
//   - retired  → delete (cleanup; a retired file should not exist
//     because RetireDaemonRecord deletes after writing a transient
//     tombstone, but a crash between the write and the unlink can
//     leave one).
//   - malformed record (JSON parse error) → delete (a corrupt
//     trusted-state file cannot be used to identify a daemon; deleting
//     it unblocks the leaf).
//
// Returns an error only if the walk itself fails (e.g. the daemons/
// directory cannot be read for a reason other than "does not exist").
// A missing daemons/ directory = nothing to reconcile = nil.
//
// Per-record failures (a single record that cannot be loaded or
// retired) do NOT abort the walk: Reconcile processes the rest and
// returns a combined error listing the failing leaves. This way one
// corrupt record does not block parent startup.
//
// cacheRoot is the shared cache root (parent of cache-scope dirs).
func ReconcileDaemonRecords(cacheRoot string) error {
	root := Root(cacheRoot)
	daemonsDir := filepath.Join(root, daemonsDir)
	entries, err := os.ReadDir(daemonsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No daemons/ dir = nothing to reconcile. EnsureRoot would
			// create it, but Reconcile is called before any build, so
			// the dir may legitimately not exist yet on a fresh
			// install.
			return nil
		}
		return fmt.Errorf("buildcontrol: read daemons dir %s: %w", daemonsDir, err)
	}

	var errs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		leafHash := strings.TrimSuffix(name, ".json")
		path := filepath.Join(daemonsDir, name)

		if rerr := reconcileDaemonRecordFile(path, leafHash); rerr != nil {
			errs = append(errs, rerr.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("buildcontrol: reconcile daemon records: %s", strings.Join(errs, "; "))
	}
	return nil
}

// reconcileDaemonRecordFile processes a single <leaf-hash>.json record
// file. It does NOT abort the walk on a per-record error; instead it
// returns the error so ReconcileDaemonRecords can collect it.
func reconcileDaemonRecordFile(path, leafHash string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Raced with a concurrent retire/delete; nothing to do.
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}

	var rec DaemonRecord
	if jerr := json.Unmarshal(data, &rec); jerr != nil {
		// A malformed record cannot be safely used to identify a
		// daemon; delete it to unblock the leaf (a future build re-
		// arms). This is the documented "malformed trusted state →
		// service failure, but do not block parent startup" path.
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("remove malformed %s: %w (parse err: %v)", path, rerr, jerr)
		}
		return nil
	}

	switch rec.State {
	case DaemonStatePending:
		// Parent crashed between wrapper launch and daemon registration.
		// No PID to verify; the unguessable marker makes it safe to
		// delete without verification. The next build re-arms.
		return removeRecord(path)

	case DaemonStateActive:
		// Verify the recorded process. procidentity returns
		// ErrNoSuchProcess for a dead pid, verified=false for a live
		// but mismatched (PID-reused / executable-changed) pid, and
		// ErrUnverifiable when the platform cannot tell.
		verified, _, verr := daemonVerify(rec.PID, rec.JDKExecutable, rec.StartIdentity)
		if verr != nil {
			if errors.Is(verr, procidentity.ErrNoSuchProcess) {
				return removeRecord(path)
			}
			// ErrUnverifiable or any other error: leave the record;
			// the leaf is blocked (fail closed) at build time.
			return nil
		}
		if !verified {
			// Live but mismatched (PID reused / executable changed /
			// start identity changed). Retire (delete).
			return removeRecord(path)
		}
		// Live and matches: leave the record (remains controllable).
		return nil

	case DaemonStateRetired:
		// A retired tombstone should not exist (RetireDaemonRecord
		// deletes after writing it), but a crash between the atomic
		// write and the unlink can leave one. Clean it up.
		return removeRecord(path)

	default:
		// Unknown state: the on-disk state is from a newer or older
		// version. Delete to unblock; a future build re-arms.
		return removeRecord(path)
	}
}

// removeRecord removes path, treating a missing file as success (a
// concurrent retire/delete may have beaten us to it).
func removeRecord(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
