package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// newServeServerForSnapshotTest builds a minimal serveServer with the
// build-snapshot fields populated, for testing freezeBuildSnapshot
// directly (no facade, no harness, no real activation).
func newServeServerForSnapshotTest(t *testing.T, cacheScopeDir string) *serveServer {
	t.Helper()
	return &serveServer{
		buildSnapshots: buildengine.NewParentSnapshotStore(),
		cacheScopeDir:  cacheScopeDir,
	}
}

// TestFreezeBuildSnapshot_FreezeOncePerParentLifetime asserts the
// parent freezes the capability snapshot for a worktree only ONCE per
// parent lifetime. A re-activation or agent-callable reload (deactivate
// → activate) must NOT refresh the snapshot from a changed durable
// approval on disk — that would let an agent-callable route grant or
// refresh build capabilities without a parent restart (spec
// §Authorization and security, ticket 06: "changed approval takes
// effect only after parent restart; agent-callable activate/reload
// cannot grant or refresh build capabilities").
func TestFreezeBuildSnapshot_FreezeOncePerParentLifetime(t *testing.T) {
	wt := t.TempDir()
	cacheDir := t.TempDir()
	leaf := filepath.Join(cacheDir, "gradle")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}

	// Plant a manifest and write a matching durable approval.
	manifestContent := `version: 1
builds:
  - root: backend
    tool: gradle
    containers:
      images:
        - pgvector/pgvector:pg16
`
	if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte(manifestContent), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := buildmanifest.Load(wt)
	if err != nil {
		t.Fatal(err)
	}
	digest := buildmanifest.Digest(m)
	caps := m.CapabilitySet(buildmanifest.HostPolicy{})
	canon, _ := canonicalWorktree(wt)
	loc := buildControlApprovalLocation(cacheDir, canon)
	if err := buildmanifest.ApproveAt(leaf, loc, digest, caps); err != nil {
		t.Fatal(err)
	}

	s := newServeServerForSnapshotTest(t, cacheDir)

	// First freeze: the snapshot is frozen from the durable approval.
	s.freezeBuildSnapshot(wt)
	got, err := s.buildSnapshots.Lookup(canon)
	if err != nil {
		t.Fatalf("first freeze: Lookup: %v", err)
	}
	if got.Policy.Digest != digest {
		t.Fatalf("first freeze: Digest = %q, want %q", got.Policy.Digest, digest)
	}
	if len(got.Policy.Capabilities.Images) != 1 || got.Policy.Capabilities.Images[0] != "pgvector/pgvector:pg16" {
		t.Fatalf("first freeze: Images = %v, want [pgvector/pgvector:pg16]", got.Policy.Capabilities.Images)
	}

	// Now change the durable approval on disk to a DIFFERENT digest +
	// capability set (simulating the host running `omac build approve`
	// again after editing the manifest). The freeze-once guard must
	// prevent the in-memory snapshot from being refreshed.
	newManifestContent := `version: 1
builds:
  - root: backend
    tool: gradle
    containers:
      images:
        - postgres:17
`
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte(newManifestContent), 0o644); err != nil {
		t.Fatal(err)
	}
	m2, err := buildmanifest.Load(wt)
	if err != nil {
		t.Fatal(err)
	}
	newDigest := buildmanifest.Digest(m2)
	newCaps := m2.CapabilitySet(buildmanifest.HostPolicy{})
	if newDigest == digest {
		t.Fatal("test setup: new manifest must have a different digest")
	}
	if err := buildmanifest.ApproveAt(leaf, loc, newDigest, newCaps); err != nil {
		t.Fatal(err)
	}

	// Second freeze (as a re-activation or reload would trigger): the
	// in-memory snapshot must NOT change — it stays at the FIRST frozen
	// digest + capability set. A changed approval takes effect only
	// after parent restart.
	s.freezeBuildSnapshot(wt)
	got2, err := s.buildSnapshots.Lookup(canon)
	if err != nil {
		t.Fatalf("second freeze: Lookup: %v", err)
	}
	if got2.Policy.Digest != digest {
		t.Errorf("second freeze: Digest = %q, want %q (freeze-once: changed approval must NOT refresh the snapshot without parent restart)", got2.Policy.Digest, digest)
	}
	if len(got2.Policy.Capabilities.Images) != 1 || got2.Policy.Capabilities.Images[0] != "pgvector/pgvector:pg16" {
		t.Errorf("second freeze: Images = %v, want [pgvector/pgvector:pg16] (freeze-once: changed approval must NOT refresh the snapshot)", got2.Policy.Capabilities.Images)
	}
}

// TestFreezeBuildSnapshot_DeactivateReactivateDoesNotRefresh asserts
// the snapshot survives a deactivate and is NOT refreshed by a
// subsequent re-activation. This is the agent-callable reload attack:
// deactivate → (host approves on disk) → re-activate. The re-activate
// must see the ORIGINAL snapshot, not the new durable approval.
func TestFreezeBuildSnapshot_DeactivateReactivateDoesNotRefresh(t *testing.T) {
	wt := t.TempDir()
	cacheDir := t.TempDir()
	leaf := filepath.Join(cacheDir, "gradle")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	// No manifest → freezeBuildSnapshot freezes a zero snapshot (the
	// normal standard-Gradle case).
	s := newServeServerForSnapshotTest(t, cacheDir)
	canon, _ := canonicalWorktree(wt)

	// First "activation": freezes a zero snapshot.
	s.freezeBuildSnapshot(wt)
	got, err := s.buildSnapshots.Lookup(canon)
	if err != nil {
		t.Fatalf("first freeze: %v", err)
	}
	if got.Policy.Digest != "" {
		t.Fatalf("first freeze: Digest = %q, want empty (no manifest)", got.Policy.Digest)
	}

	// Now plant a manifest + durable approval (simulating the host
	// editing the manifest and running `omac build approve` after this
	// parent started).
	manifestContent := `version: 1
builds:
  - root: backend
    tool: gradle
    containers:
      images:
        - pgvector/pgvector:pg16
`
	if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte(manifestContent), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := buildmanifest.Load(wt)
	if err != nil {
		t.Fatal(err)
	}
	digest := buildmanifest.Digest(m)
	caps := m.CapabilitySet(buildmanifest.HostPolicy{})
	loc := buildControlApprovalLocation(cacheDir, canon)
	if err := buildmanifest.ApproveAt(leaf, loc, digest, caps); err != nil {
		t.Fatal(err)
	}

	// "Deactivate" (the snapshot store is NOT cleared by deactivate —
	// freeze-once per parent lifetime means the snapshot persists).
	// "Re-activate": freezeBuildSnapshot must be a no-op because a
	// snapshot already exists for this worktree.
	s.freezeBuildSnapshot(wt)
	got2, err := s.buildSnapshots.Lookup(canon)
	if err != nil {
		t.Fatalf("re-activate freeze: %v", err)
	}
	if got2.Policy.Digest != "" {
		t.Errorf("re-activate freeze: Digest = %q, want empty (freeze-once: re-activation must NOT pick up the new on-disk approval without parent restart)", got2.Policy.Digest)
	}
	if len(got2.Policy.Capabilities.Images) != 0 {
		t.Errorf("re-activate freeze: Images = %v, want empty (freeze-once: re-activation must NOT pick up the new on-disk approval)", got2.Policy.Capabilities.Images)
	}
}

// TestFreezeBuildSnapshot_NoCacheScopeIsNoOp asserts the freeze is a
// no-op when the parent has no cache scope prepared (the snapshot
// store stays empty, so builds surface ErrNoSnapshot — build
// unavailable in this parent).
func TestFreezeBuildSnapshot_NoCacheScopeIsNoOp(t *testing.T) {
	wt := t.TempDir()
	s := newServeServerForSnapshotTest(t, "") // no cache scope
	s.freezeBuildSnapshot(wt)
	canon, _ := canonicalWorktree(wt)
	_, err := s.buildSnapshots.Lookup(canon)
	if err == nil {
		t.Error("freeze with no cache scope must not freeze a snapshot")
	}
}

// TestFreezeBuildSnapshot_UnapprovedDirLeavesNoSnapshot asserts a
// directory with a manifest but NO durable approval leaves NO snapshot
// frozen — build unavailable until `omac build approve` + parent
// restart (ticket 06).
func TestFreezeBuildSnapshot_UnapprovedDirLeavesNoSnapshot(t *testing.T) {
	wt := t.TempDir()
	cacheDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte("version: 1\nbuilds:\n  - root: .\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newServeServerForSnapshotTest(t, cacheDir)
	s.freezeBuildSnapshot(wt)
	canon, _ := canonicalWorktree(wt)
	_, err := s.buildSnapshots.Lookup(canon)
	if err == nil {
		t.Error("unapproved dir: snapshot was frozen, want none (build unavailable until approve + restart)")
	}
}

// TestFreezeBuildSnapshot_NoManifestFreezesZeroSnapshot asserts a
// standard-Gradle project (no .omac/build.yaml) freezes a zero
// snapshot so builds proceed with default capabilities (no approval
// required).
func TestFreezeBuildSnapshot_NoManifestFreezesZeroSnapshot(t *testing.T) {
	wt := t.TempDir()
	cacheDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cacheDir, "gradle"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := newServeServerForSnapshotTest(t, cacheDir)
	s.freezeBuildSnapshot(wt)
	canon, _ := canonicalWorktree(wt)
	got, err := s.buildSnapshots.Lookup(canon)
	if err != nil {
		t.Fatalf("no manifest: Lookup: %v", err)
	}
	if got.Policy.Digest != "" {
		t.Errorf("no manifest: Digest = %q, want empty", got.Policy.Digest)
	}
	// HostPolicy is the default ceiling; verify it's the zero value the
	// freeze helper uses (buildrun.HostPolicy(0)).
	host := buildrun.HostPolicy(0)
	if got.Policy.HostPolicy.MaxHeap != host.MaxHeap || got.Policy.HostPolicy.MaxDuration != host.MaxDuration {
		t.Errorf("no manifest: HostPolicy = %+v, want %+v (default ceiling)", got.Policy.HostPolicy, host)
	}
}
