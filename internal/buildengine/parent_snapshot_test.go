package buildengine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// TestParentSnapshotStore_LookupMissingReturnsErrNoSnapshot asserts the
// store returns ErrNoSnapshot (not a bare nil) when no snapshot is
// frozen for the requested worktree. The engine surfaces this as a
// policy denial with the host diagnostic requiring `omac build
// approve` + parent restart (ticket 06).
func TestParentSnapshotStore_LookupMissingReturnsErrNoSnapshot(t *testing.T) {
	s := NewParentSnapshotStore()
	_, err := s.Lookup("/repo/unapproved")
	if !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("Lookup on empty store: err = %v, want ErrNoSnapshot", err)
	}
	if !strings.Contains(err.Error(), "omac build approve") {
		t.Errorf("ErrNoSnapshot message missing the approve+restart diagnostic: %v", err)
	}
}

// TestParentSnapshotStore_FreezeThenLookup asserts Freeze stores an
// immutable snapshot keyed by canonical worktree and Lookup returns it.
func TestParentSnapshotStore_FreezeThenLookup(t *testing.T) {
	s := NewParentSnapshotStore()
	wt := "/repo/worktree"
	snap := ParentSnapshot{
		Worktree: wt,
		Policy: PolicySnapshot{
			Digest:       "abc123",
			Capabilities: buildmanifest.CapabilitySet{Images: []string{"pgvector/pgvector:pg16"}},
		},
	}
	s.Freeze(wt, snap)
	got, err := s.Lookup(wt)
	if err != nil {
		t.Fatalf("Lookup after Freeze: %v", err)
	}
	if got.Policy.Digest != "abc123" {
		t.Errorf("Digest = %q, want abc123", got.Policy.Digest)
	}
	if len(got.Policy.Capabilities.Images) != 1 || got.Policy.Capabilities.Images[0] != "pgvector/pgvector:pg16" {
		t.Errorf("Capabilities.Images = %v", got.Policy.Capabilities.Images)
	}
}

// TestParentSnapshotStore_DistinctWorktreesKeptSeparately asserts the
// store keys snapshots by canonical worktree so two worktrees sharing
// a Gradle leaf keep distinct snapshots (ticket 06: trusted state is
// namespaced by canonical worktree identity even when worktrees share
// a leaf).
func TestParentSnapshotStore_DistinctWorktreesKeptSeparately(t *testing.T) {
	s := NewParentSnapshotStore()
	wtA, wtB := "/repo/wt-a", "/repo/wt-b"
	s.Freeze(wtA, ParentSnapshot{Worktree: wtA, Policy: PolicySnapshot{Digest: "aaa"}})
	s.Freeze(wtB, ParentSnapshot{Worktree: wtB, Policy: PolicySnapshot{Digest: "bbb"}})
	a, _ := s.Lookup(wtA)
	b, _ := s.Lookup(wtB)
	if a.Policy.Digest != "aaa" || b.Policy.Digest != "bbb" {
		t.Errorf("distinct worktrees collapsed: a=%q b=%q", a.Policy.Digest, b.Policy.Digest)
	}
}

// TestParentSnapshotStore_FreezeOverwritesOnRestart asserts a second
// Freeze for the same worktree overwrites the prior snapshot — the
// parent restarted, so the new approval is in effect. A changed
// approval takes effect only after parent restart (ticket 06).
func TestParentSnapshotStore_FreezeOverwritesOnRestart(t *testing.T) {
	s := NewParentSnapshotStore()
	wt := "/repo/wt"
	s.Freeze(wt, ParentSnapshot{Worktree: wt, Policy: PolicySnapshot{Digest: "old"}})
	s.Freeze(wt, ParentSnapshot{Worktree: wt, Policy: PolicySnapshot{Digest: "new"}})
	got, _ := s.Lookup(wt)
	if got.Policy.Digest != "new" {
		t.Errorf("Digest after re-freeze = %q, want new", got.Policy.Digest)
	}
}

// TestParentSnapshotProvider_ReadOnly asserts the SnapshotProvider
// adapter returned by ParentSnapshotProvider reads the frozen snapshot
// and does NOT write approvals or replace snapshots. A build request
// can only read; it cannot advance or replace the snapshot (ticket 06).
func TestParentSnapshotProvider_ReadOnly(t *testing.T) {
	s := NewParentSnapshotStore()
	wt := "/repo/wt"
	s.Freeze(wt, ParentSnapshot{
		Worktree: wt,
		Policy: PolicySnapshot{
			Digest:       "frozen",
			Capabilities: buildmanifest.CapabilitySet{BuildRoots: []string{"backend"}},
		},
	})
	provider := s.ParentSnapshotProvider()
	// First call: returns the frozen snapshot.
	got, err := provider(wt, "/leaf", buildrun.Request{})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if got.Digest != "frozen" {
		t.Errorf("Digest = %q, want frozen", got.Digest)
	}
	// Second call: still the same snapshot (no advance/replace).
	got2, _ := provider(wt, "/leaf", buildrun.Request{})
	if got2.Digest != "frozen" {
		t.Errorf("second call Digest = %q, want frozen (snapshot must be immutable)", got2.Digest)
	}
	// The leaf and req args are ignored by the parent-snapshot adapter
	// (the snapshot is in memory, keyed by worktree; the host ceiling
	// was frozen at activation). Verify the adapter does not panic on
	// a different leaf/req.
	if _, err := provider(wt, "/different-leaf", buildrun.Request{MaxDuration: 99999}); err != nil {
		t.Errorf("provider with different leaf/req: %v", err)
	}
}

// TestParentSnapshotProvider_MissingWorktreeReturnsErrNoSnapshot
// asserts the provider returns ErrNoSnapshot for an unapproved
// worktree, surfacing the approve+restart diagnostic (ticket 06).
func TestParentSnapshotProvider_MissingWorktreeReturnsErrNoSnapshot(t *testing.T) {
	s := NewParentSnapshotStore()
	provider := s.ParentSnapshotProvider()
	_, err := provider("/repo/unapproved", "/leaf", buildrun.Request{})
	if !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("provider on unapproved worktree: err = %v, want ErrNoSnapshot", err)
	}
}

// TestFreezeFromApproval_StoresDigestAndCaps asserts the
// FreezeFromApproval helper freezes a ParentSnapshot carrying the
// approved digest, capability set, and host ceiling. This is the
// helper the parent's activation logic (serve) and pre-launch logic
// (start) call after writing/loading the durable approval record.
func TestFreezeFromApproval_StoresDigestAndCaps(t *testing.T) {
	s := NewParentSnapshotStore()
	wt := "/repo/wt"
	caps := buildmanifest.CapabilitySet{Images: []string{"postgres:17"}}
	host := buildmanifest.HostPolicy{MaxHeap: "2g", MaxDuration: 1800}
	s.FreezeFromApproval(wt, "deadbeef", caps, host)
	got, _ := s.Lookup(wt)
	if got.Policy.Digest != "deadbeef" {
		t.Errorf("Digest = %q, want deadbeef", got.Policy.Digest)
	}
	if len(got.Policy.Capabilities.Images) != 1 || got.Policy.Capabilities.Images[0] != "postgres:17" {
		t.Errorf("Capabilities.Images = %v", got.Policy.Capabilities.Images)
	}
	if got.Policy.HostPolicy.MaxHeap != "2g" || got.Policy.HostPolicy.MaxDuration != 1800 {
		t.Errorf("HostPolicy = %+v, want MaxHeap=2g MaxDuration=1800", got.Policy.HostPolicy)
	}
}

// TestFreezeFromDurableApproval_NoManifestFreezesZeroSnapshot asserts
// the parent freezes a zero snapshot when the worktree has no
// .omac/build.yaml — the normal standard-Gradle-project case (builds
// proceed with default capabilities, no approval required).
func TestFreezeFromDurableApproval_NoManifestFreezesZeroSnapshot(t *testing.T) {
	wt := t.TempDir()
	cacheDir := t.TempDir()
	store := NewParentSnapshotStore()
	freezeSnapshotFromDurableApprovalInProcess(store, wt, cacheDir)
	got, err := store.Lookup(wt)
	if err != nil {
		t.Fatalf("Lookup after freeze (no manifest): %v", err)
	}
	if got.Policy.Digest != "" {
		t.Errorf("Digest = %q, want empty (no manifest)", got.Policy.Digest)
	}
	if len(got.Policy.Capabilities.Images) != 0 || len(got.Policy.Capabilities.BuildRoots) != 0 {
		t.Errorf("Capabilities = %+v, want zero (no manifest)", got.Policy.Capabilities)
	}
}

// TestFreezeFromDurableApproval_DigestMismatchLeavesNoSnapshot asserts
// a changed manifest (digest mismatch with the durable approval) leaves
// NO snapshot frozen — build unavailable until `omac build approve` +
// parent restart. This is the core trust-boundary guarantee: a changed
// manifest cannot update the snapshot or activate before explicit host
// approval + parent restart (ticket 06).
func TestFreezeFromDurableApproval_DigestMismatchLeavesNoSnapshot(t *testing.T) {
	wt := t.TempDir()
	cacheDir := t.TempDir()
	// Plant a manifest with container images.
	writeManifestForSnapshotTest(t, wt, `version: 1
builds:
  - root: backend
    tool: gradle
    containers:
      images:
        - pgvector/pgvector:pg16
`)
	// Write a durable approval for a DIFFERENT digest (simulate a
	// changed manifest since last approval).
	loc := buildControlApprovalLocationForTest(t, cacheDir, wt)
	leaf := gradleLeafForTest(cacheDir)
	approved := buildmanifest.CapabilitySet{Images: []string{"postgres:16"}}
	if err := buildmanifest.ApproveAt(leaf, loc, "stale-digest-not-matching-current", approved); err != nil {
		t.Fatal(err)
	}
	store := NewParentSnapshotStore()
	freezeSnapshotFromDurableApprovalInProcess(store, wt, cacheDir)
	_, err := store.Lookup(wt)
	if !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("Lookup after digest mismatch: err = %v, want ErrNoSnapshot (build unavailable until approve + restart)", err)
	}
}

// TestFreezeFromDurableApproval_MatchingDigestFreezesSnapshot asserts
// the parent freezes the snapshot from the durable approval when the
// manifest's current digest matches the approved digest. This is the
// activation path: the host ran `omac build approve`, then restarted
// the parent, and the parent freezes the approved capability set.
func TestFreezeFromDurableApproval_MatchingDigestFreezesSnapshot(t *testing.T) {
	wt := t.TempDir()
	cacheDir := t.TempDir()
	manifestContent := `version: 1
builds:
  - root: backend
    tool: gradle
    containers:
      images:
        - pgvector/pgvector:pg16
`
	writeManifestForSnapshotTest(t, wt, manifestContent)
	// Compute the actual digest of the manifest we just wrote.
	m, err := buildmanifest.Load(wt)
	if err != nil {
		t.Fatal(err)
	}
	digest := buildmanifest.Digest(m)
	caps := m.CapabilitySet(buildmanifest.HostPolicy{})
	loc := buildControlApprovalLocationForTest(t, cacheDir, wt)
	leaf := gradleLeafForTest(cacheDir)
	if err := buildmanifest.ApproveAt(leaf, loc, digest, caps); err != nil {
		t.Fatal(err)
	}
	store := NewParentSnapshotStore()
	freezeSnapshotFromDurableApprovalInProcess(store, wt, cacheDir)
	got, err := store.Lookup(wt)
	if err != nil {
		t.Fatalf("Lookup after matching approval: %v", err)
	}
	if got.Policy.Digest != digest {
		t.Errorf("Digest = %q, want %q", got.Policy.Digest, digest)
	}
	if len(got.Policy.Capabilities.Images) != 1 || got.Policy.Capabilities.Images[0] != "pgvector/pgvector:pg16" {
		t.Errorf("Capabilities.Images = %v", got.Policy.Capabilities.Images)
	}
}

// TestFreezeFromDurableApproval_NoApprovalLeavesNoSnapshot asserts a
// manifest present but no durable approval record leaves NO snapshot
// frozen — build unavailable until `omac build approve` + parent
// restart. An agent-callable activate/reload route can never grant or
// refresh build capabilities (ticket 06).
func TestFreezeFromDurableApproval_NoApprovalLeavesNoSnapshot(t *testing.T) {
	wt := t.TempDir()
	cacheDir := t.TempDir()
	writeManifestForSnapshotTest(t, wt, `version: 1
builds:
  - root: backend
    tool: gradle
    containers:
      images:
        - pgvector/pgvector:pg16
`)
	store := NewParentSnapshotStore()
	freezeSnapshotFromDurableApprovalInProcess(store, wt, cacheDir)
	_, err := store.Lookup(wt)
	if !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("Lookup with manifest but no approval: err = %v, want ErrNoSnapshot", err)
	}
}

// TestFreezeFromDurableApproval_MalformedManifestLeavesNoSnapshot
// asserts a malformed manifest leaves no snapshot (build unavailable —
// the engine surfaces the manifest error as a policy denial).
func TestFreezeFromDurableApproval_MalformedManifestLeavesNoSnapshot(t *testing.T) {
	wt := t.TempDir()
	cacheDir := t.TempDir()
	writeManifestForSnapshotTest(t, wt, "version: not-yaml-garbage\n  : [")
	store := NewParentSnapshotStore()
	freezeSnapshotFromDurableApprovalInProcess(store, wt, cacheDir)
	_, err := store.Lookup(wt)
	if !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("Lookup with malformed manifest: err = %v, want ErrNoSnapshot", err)
	}
}

// TestParentSnapshotStore_StringIsDiagnostic asserts the String helper
// returns a non-secret diagnostic representation (used by tests and
// diagnostics; never exposes secret material — the snapshot holds no
// secrets).
func TestParentSnapshotStore_StringIsDiagnostic(t *testing.T) {
	s := NewParentSnapshotStore()
	wt := "/repo/wt"
	s.Freeze(wt, ParentSnapshot{Worktree: wt})
	out := s.String()
	if !strings.Contains(out, "ParentSnapshotStore") {
		t.Errorf("String() missing type name: %q", out)
	}
	if !strings.Contains(out, wt) {
		t.Errorf("String() missing worktree key: %q", out)
	}
}

// freezeSnapshotFromDurableApprovalInProcess is a thin wrapper around
// the cli package's freezeSnapshotFromDurableApproval so the
// buildengine package can exercise the parent's activation logic
// without importing cli (which would be a dependency cycle). It
// reimplements the same logic inline — the canonical implementation
// lives in internal/cli/build_broker_wiring.go. If the two drift the
// cli-level integration tests will catch it.
func freezeSnapshotFromDurableApprovalInProcess(store *ParentSnapshotStore, canonicalWorktree, cacheDir string) {
	manifest, err := buildmanifest.Load(canonicalWorktree)
	if err != nil {
		return
	}
	host := buildrun.HostPolicy(0)
	if !manifest.HasManifest() {
		store.FreezeFromApproval(canonicalWorktree, "", buildmanifest.CapabilitySet{HostPolicy: host}, host)
		return
	}
	digest := buildmanifest.Digest(manifest)
	loc := buildControlApprovalLocationForTest(nil, cacheDir, canonicalWorktree)
	leaf := gradleLeafForTest(cacheDir)
	rec, err := buildmanifest.LoadApprovalAt(leaf, loc)
	if err != nil || rec.Digest == "" || rec.Digest != digest {
		return
	}
	store.FreezeFromApproval(canonicalWorktree, digest, rec.Capabilities, host)
}

func writeManifestForSnapshotTest(t *testing.T, wt, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildControlApprovalLocationForTest(_ *testing.T, cacheDir, canonicalWorktree string) buildmanifest.Location {
	// Use the public buildmanifest API directly (the cli package's
	// buildControlApprovalLocation delegates to the same call).
	root := filepath.Dir(cacheDir)
	return buildmanifest.NewBuildControlLocation(root, canonicalWorktree)
}

func gradleLeafForTest(cacheDir string) string {
	return filepath.Join(cacheDir, "gradle")
}
