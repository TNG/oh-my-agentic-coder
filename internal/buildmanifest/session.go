package buildmanifest

import (
	"fmt"
	"time"
)

// GateError is returned by Gate when the build must NOT proceed unattended:
// either the manifest content changed (re-approval required) or there is no
// prior approval at all (first-ever build with this manifest). The CLI maps
// it to ExitPolicyDenied and prints the consolidated diff + restart
// instruction. The build never starts in this state — the human reviews
// before the first run after a change (spec.md:101).
type GateError struct {
	// Diff is the consolidated capability diff (empty for first-ever).
	Diff CapabilityDiff
	// Reason is the human-readable reason ("no prior approval", "manifest
	// changed since last approval", "host ceiling dropped below approved").
	Reason string
	// FirstEver is true when there is no prior approval at all.
	FirstEver bool
}

func (e *GateError) Error() string {
	return fmt.Sprintf("manifest gate: %s\n%s", e.Reason, e.Diff.Render())
}

// GateResult is the outcome of a successful (unattended) gate pass: the
// frozen capability set to use for this build and the digest that was
// matched against the active record. The CLI threads Capabilities into
// BuildConfig; the build proceeds with the FROZEN set even if the worktree
// file changes mid-session.
type GateResult struct {
	// Capabilities is the frozen-for-session effective capability set.
	Capabilities CapabilitySet
	// Digest is the manifest content digest that matched the active record.
	Digest string
}

// Gate implements the frozen-for-session approval gate. It is the seam the
// CLI calls after Load+Validate and before GrantsFor/RunBuild:
//
//   - If there is an active record (frozen-for-session) whose digest matches
//     the worktree manifest's digest, the build starts UNATTENDED with the
//     frozen capability set. Mid-session worktree edits do NOT take effect:
//     they change the digest, which then misses the active record → gate
//     fails with the consolidated diff + restart instruction (spec.md:101).
//   - If there is NO active record (first-ever build with this manifest, OR
//     the manifest changed since the session was frozen), the gate RECORDS
//     the approval (digest + effective capability set) AND fails with a
//     *GateError presenting the consolidated diff + restart instruction.
//     The first use PRESENTS the diff AND records approval (spec.md:101:
//     "presents one consolidated capability diff and records approval
//     against its digest and effective capability set"); the build does NOT
//     start this time — the human reviews the diff, then restarts. The next
//     run finds the now-matching active record and starts unattended.
//   - If the host ceiling has DROPPED below what was previously approved,
//     the gate re-records approval against the new (lower) capability set
//     and fails with the diff + restart instruction (the stored set was
//     invalidated by the ceiling drop).
//
// v1 has no auto-approve that SKIPS the review: the first build after any
// change always fails with the diff so the human sees it. The approval is
// recorded so the SECOND build (same digest) starts unattended. There is no
// `omac build approve` subcommand; the gate failure IS the approval prompt.
//
// host is the authority ceiling; the manifest's effective capability set is
// intersected with it. leaf is the resolved OMAC cache leaf (where
// `.omac-control/` lives). digest is Digest(manifest). caps is
// manifest.CapabilitySet(host).
//
// Gate uses the legacy OnLeaf location. GateAt accepts a Location so the
// ticket-06 build-control layout can store approvals under the host-only
// build-control root, namespaced by canonical worktree.
func Gate(leaf string, digest string, caps CapabilitySet) (GateResult, error) {
	return GateAt(leaf, NewOnLeafLocation(), digest, caps)
}

// GateAt is the location-aware variant of Gate. It reads/writes approval
// records at the location-selected path. Under the BuildControl layout
// the active record is the durable approval record itself (the parent
// holds an in-memory snapshot); a digest match against the durable
// approval starts unattended with the frozen set, and a mismatch
// records the new approval and fails with the diff + restart instruction.
func GateAt(leaf string, loc Location, digest string, caps CapabilitySet) (GateResult, error) {
	active, err := LoadActiveAt(leaf, loc)
	if err != nil {
		return GateResult{}, fmt.Errorf("load active manifest: %w", err)
	}
	if active.Digest == digest {
		// Digest matches the frozen-for-session record. Check the host
		// ceiling has not dropped below what was approved.
		if !ceilingStillValid(active.Capabilities.HostPolicy, caps.HostPolicy) {
			// Ceiling dropped: re-record approval against the new (lower)
			// capability set and fail with the diff + restart instruction.
			if err := ApproveAt(leaf, loc, digest, caps); err != nil {
				return GateResult{}, fmt.Errorf("re-record approval after ceiling drop: %w", err)
			}
			return GateResult{}, &GateError{
				Diff:   Diff(active.Capabilities, caps),
				Reason: "host policy ceiling dropped below the previously approved set — re-approval required",
			}
		}
		return GateResult{Capabilities: active.Capabilities, Digest: digest}, nil
	}
	// No active record, OR digest changed since the session was frozen:
	// record approval (so the next run starts unattended) and FAIL with the
	// consolidated diff + restart instruction. The first use PRESENTS the
	// diff AND records approval; the build does not start this time.
	if err := ApproveAt(leaf, loc, digest, caps); err != nil {
		return GateResult{}, fmt.Errorf("record approval: %w", err)
	}
	if active.Digest == "" {
		return GateResult{}, &GateError{
			Diff:      Diff(CapabilitySet{}, caps),
			Reason:    "no prior approval for this manifest — review the capability diff, then restart OMAC to activate (v1 has no auto-approve)",
			FirstEver: true,
		}
	}
	return GateResult{}, &GateError{
		Diff:   Diff(active.Capabilities, caps),
		Reason: "manifest changed since last approval — review the consolidated diff, then restart OMAC to activate",
	}
}

// ceilingStillValid reports whether the current host ceiling still covers
// the previously-approved capability set on every dimension a request
// actually used. A dimension the approved set did NOT request (zero in the
// approved Resources) is unaffected by the current ceiling. A dimension the
// approved set DID request requires a non-zero current ceiling that still
// covers it; a current zero ceiling means the host removed authorization
// for a dimension the manifest had requested → invalidate (re-approval).
func ceilingStillValid(prev, cur HostPolicy) bool {
	// Heap: a previously-approved non-empty heap request needs a current
	// ceiling that still covers it. (prev.MaxHeap is the ceiling at
	// approval time; if it was non-empty the request was bounded by it.)
	if cur.MaxHeap != "" && prev.MaxHeap != "" && heapAbove(prev.MaxHeap, cur.MaxHeap) {
		return false
	}
	if cur.MaxDuration > 0 && prev.MaxDuration > 0 && prev.MaxDuration > cur.MaxDuration {
		return false
	}
	return true
}

// Approve records the host user's acceptance of a manifest digest + its
// effective capability set, AND freezes it as the active-for-session
// record. Called by the CLI when the human reviews the consolidated diff
// and restarts to activate (v1: the gate failure IS the prompt; Approve is
// the wiring a future `omac build approve` subcommand — or an auto-approve
// policy — would call; today the human re-runs `omac build` after editing
// the manifest, which re-runs the gate. For the first build after a change
// to start unattended, the host user must run `omac build` once to fail
// with the diff, then run it again — OR a host-side helper calls Approve.
// This is documented in docs/build-command.md as a v1 limitation.)
//
// This function is exported for the CLI wiring and for tests; it is the
// single write path that makes a digest "approved + frozen for session".
//
// Approve uses the legacy OnLeaf location. ApproveAt accepts a Location.
func Approve(leaf string, digest string, caps CapabilitySet) error {
	return ApproveAt(leaf, NewOnLeafLocation(), digest, caps)
}

// ApproveAt records the approval at the location-selected path. Under
// the BuildControl layout the durable approval record IS the active
// record (the in-memory parent snapshot is derived from it at
// activation); StoreActiveAt is a no-op there.
func ApproveAt(leaf string, loc Location, digest string, caps CapabilitySet) error {
	now := time.Now().UTC()
	if err := StoreApprovalAt(leaf, loc, ApprovalRecord{
		Digest:       digest,
		Capabilities: caps,
		ApprovedAt:   now,
	}); err != nil {
		return err
	}
	return StoreActiveAt(leaf, loc, ActiveRecord{
		Digest:       digest,
		Capabilities: caps,
		ActivatedAt:  now,
	})
}

// ResetActive clears the active (frozen-for-session) record, forcing the
// next build to re-run the approval gate. Used by tests and (in future) by
// a teardown command. Does NOT clear the approval record (the host user's
// acceptance is persistent across sessions; only the frozen-for-session
// state is per-session).
func ResetActive(leaf string) error {
	return StoreActive(leaf, ActiveRecord{})
}
