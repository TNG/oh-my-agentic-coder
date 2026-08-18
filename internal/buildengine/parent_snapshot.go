package buildengine

import (
	"errors"
	"fmt"
	"sync"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// ParentSnapshot is the parent-owned in-memory capability snapshot
// frozen at activation, keyed by canonical worktree. The parent (start
// or serve) freezes one before launching the inner process (start) or
// at activation when the canonical identity + current digest match a
// durable approval (serve). A build request only compares against this
// snapshot; it cannot advance or replace it (spec §Authorization and
// security, ticket 06).
//
// The snapshot is immutable once frozen: Digest, Capabilities, and
// HostPolicy are set at freeze time and never mutated. A changed
// manifest cannot update the snapshot or activate before explicit host
// approval + parent restart — the snapshot is the frozen-for-session
// view the engine consumes for every build in this parent's lifetime.
type ParentSnapshot struct {
	// Worktree is the canonical worktree the snapshot is keyed by.
	Worktree string
	// Policy is the immutable approved-policy snapshot.
	Policy PolicySnapshot
}

// ParentSnapshotStore is the parent-owned, thread-safe, in-memory
// store of ParentSnapshots keyed by canonical worktree. The parent
// populates it at activation (serve) or before launching the inner
// process (start); the broker's engine invoker reads from it via the
// ParentSnapshotProvider adapter.
//
// A build request can ONLY read the snapshot; it cannot advance or
// replace it. Agent-callable serve activation and reload are NOT
// approval transitions: they do not write to this store. Only the
// parent's own activation logic (serve) or pre-launch logic (start)
// writes here, and only when the canonical identity + current digest
// match a durable approval record.
//
// An unapproved directory has build UNAVAILABLE: Lookup returns
// ErrNoSnapshot and the engine surfaces a host diagnostic requiring
// `omac build approve` + parent restart.
type ParentSnapshotStore struct {
	mu        sync.RWMutex
	snapshots map[string]ParentSnapshot
}

// NewParentSnapshotStore returns an empty ParentSnapshotStore.
func NewParentSnapshotStore() *ParentSnapshotStore {
	return &ParentSnapshotStore{snapshots: map[string]ParentSnapshot{}}
}

// Freeze records an immutable ParentSnapshot for canonicalWorktree.
// Called by the parent at activation (when canonical identity +
// current digest match a durable approval) or before launch (start).
// A subsequent Freeze for the same worktree overwrites the prior
// snapshot (the parent restarted, so the new approval is in effect).
func (s *ParentSnapshotStore) Freeze(canonicalWorktree string, snap ParentSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots[canonicalWorktree] = snap
}

// Lookup returns the frozen ParentSnapshot for canonicalWorktree, or
// ErrNoSnapshot when none is frozen (build unavailable for this
// directory — the engine surfaces a host diagnostic requiring `omac
// build approve` + parent restart).
func (s *ParentSnapshotStore) Lookup(canonicalWorktree string) (ParentSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap, ok := s.snapshots[canonicalWorktree]
	if !ok {
		return ParentSnapshot{}, ErrNoSnapshot
	}
	return snap, nil
}

// ErrNoSnapshot is returned by ParentSnapshotStore.Lookup when no
// snapshot is frozen for the requested worktree. The engine surfaces it
// as a policy denial with a host diagnostic requiring `omac build
// approve` + parent restart.
var ErrNoSnapshot = errors.New("buildengine: no parent capability snapshot for this worktree (run `omac build approve` and restart the omac parent)")

// ParentSnapshotProvider returns a SnapshotProvider that reads from
// the parent-owned store. The engine calls it once per invocation;
// the provider looks up the frozen snapshot for the canonical worktree
// and returns its immutable PolicySnapshot. A missing snapshot is a
// policy denial (build unavailable — the host must approve + restart).
//
// The provider does NOT write approvals or replace snapshots: it is
// the read-only seam between the parent's in-memory state and the
// engine. The leaf argument is ignored (the parent snapshot is in
// memory, keyed by worktree); req is also ignored (the parent
// snapshot already froze the host ceiling at activation).
func (s *ParentSnapshotStore) ParentSnapshotProvider() SnapshotProvider {
	return func(worktree, leaf string, req buildrun.Request) (PolicySnapshot, error) {
		snap, err := s.Lookup(worktree)
		if err != nil {
			return PolicySnapshot{}, err
		}
		return snap.Policy, nil
	}
}

// FreezeFromApproval freezes a ParentSnapshot for canonicalWorktree
// from a durable approval record. The parent calls this at activation
// (serve) or before launch (start) when the canonical identity +
// current digest match the durable approval. host is the host policy
// ceiling to freeze into the snapshot.
//
// The "durable approval record" is either the per-worktree record or —
// when the opt-in reuse-by-digest feature is enabled (ADR 0005) and the
// per-worktree record misses — the digest-indexed, repo-namespaced
// reuse record (whose RepoRootCommit was verified against the current
// repo's root commit before freezing). The snapshot is still keyed by
// canonical worktree exactly as today: a reused record freezes THIS
// worktree's immutable snapshot; it never shares mutable state between
// worktrees.
//
// This is the helper a host-only `omac build approve` command (and
// the parent's activation logic) calls after writing the durable
// approval record; the snapshot takes effect for build requests only
// after the parent restarts (a running parent's in-memory store is not
// mutated by an external approve command — the approve writes the
// durable record; the next parent start freezes the snapshot from
// it).
func (s *ParentSnapshotStore) FreezeFromApproval(canonicalWorktree, digest string, caps buildmanifest.CapabilitySet, host buildmanifest.HostPolicy) {
	s.Freeze(canonicalWorktree, ParentSnapshot{
		Worktree: canonicalWorktree,
		Policy: PolicySnapshot{
			Digest:       digest,
			Capabilities: caps,
			HostPolicy:   host,
		},
	})
}

// String returns a debug representation of the store's keys. Used by
// tests and diagnostics; never exposes secret material (the snapshot
// holds no secrets — capabilities are non-secret).
func (s *ParentSnapshotStore) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.snapshots))
	for k := range s.snapshots {
		keys = append(keys, k)
	}
	return fmt.Sprintf("ParentSnapshotStore(%d worktrees: %v)", len(keys), keys)
}
