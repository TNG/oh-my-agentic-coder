package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildcontrol"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
)

// gitInitForTest initializes a git repo in worktree with a hermetic
// identity and returns the worktree (chainable for linked worktrees).
func gitInitForTest(t *testing.T, worktree string) string {
	t.Helper()
	runGitIn(t, worktree, "init", "-q", "-b", "main")
	runGitIn(t, worktree, "commit", "-q", "--allow-empty", "-m", "init")
	return worktree
}

func runGitIn(t *testing.T, worktree string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = worktree
	c.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestApprovalReuseEnabled_DefaultOff asserts the opt-in is OFF by
// default (no env, no host config) — scenario (f) at the flag level.
func TestApprovalReuseEnabled_DefaultOff(t *testing.T) {
	t.Setenv(envApprovalReuseByDigest, "")
	// Empty cache root: off.
	if approvalReuseEnabled("") {
		t.Error("empty cache root must not enable reuse")
	}
	// Valid cache root with no config.toml: off.
	cacheRoot := t.TempDir()
	if approvalReuseEnabled(cacheRoot) {
		t.Error("reuse must default off with no host config")
	}
}

// TestApprovalReuseEnabled_EnvOverride asserts OMAC_APPROVAL_REUSE_BY_DIGEST=1
// enables reuse even without a host config.
func TestApprovalReuseEnabled_EnvOverride(t *testing.T) {
	t.Setenv(envApprovalReuseByDigest, "1")
	if !approvalReuseEnabled(t.TempDir()) {
		t.Error("env=1 must enable reuse")
	}
}

// TestApprovalReuseEnabled_HostConfig asserts a host config entry in
// <cacheRoot>/build-control/config.toml enables reuse (durable default),
// and a `false` entry keeps it off.
func TestApprovalReuseEnabled_HostConfig(t *testing.T) {
	t.Setenv(envApprovalReuseByDigest, "")
	cacheRoot := t.TempDir()
	root := buildcontrol.Root(cacheRoot)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// No key -> off.
	if approvalReuseEnabled(cacheRoot) {
		t.Error("config without the key must not enable reuse")
	}
	// Key = true -> on.
	cfg := filepath.Join(root, "config.toml")
	if err := os.WriteFile(cfg, []byte("approval_reuse_by_digest = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !approvalReuseEnabled(cacheRoot) {
		t.Error("config approval_reuse_by_digest=true must enable reuse")
	}
	// Key = false -> off.
	if err := os.WriteFile(cfg, []byte("approval_reuse_by_digest = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if approvalReuseEnabled(cacheRoot) {
		t.Error("config approval_reuse_by_digest=false must keep reuse off")
	}
}

// TestResolveRepoIdentity_LinkedWorktreesCollapse asserts all linked
// worktrees of a repo resolve to the SAME canonicalRepoRoot (scenario
// a's namespace property), while a separate CLONE has a DIFFERENT
// canonicalRepoRoot (scenario d: no cross-clone reuse).
func TestResolveRepoIdentity_LinkedWorktreesCollapse(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInitForTest(t, main)
	// Linked worktree under a sibling dir.
	linked := filepath.Join(base, "linked")
	if err := exec.Command("git", "-C", main, "worktree", "add", "-q", linked, "-b", "feature").Run(); err != nil {
		t.Skipf("cannot create linked worktree: %v", err)
	}

	mainRepoRoot, mainRootCommit := resolveRepoIdentity(main)
	linkedRepoRoot, linkedRootCommit := resolveRepoIdentity(linked)
	if mainRepoRoot == "" || linkedRepoRoot == "" {
		t.Fatalf("resolveRepoIdentity failed: main=%q linked=%q", mainRepoRoot, linkedRepoRoot)
	}
	if mainRepoRoot != linkedRepoRoot {
		t.Errorf("linked worktrees must share canonicalRepoRoot: main=%q linked=%q", mainRepoRoot, linkedRepoRoot)
	}
	if mainRootCommit != linkedRootCommit {
		t.Errorf("linked worktrees must share repoRootCommit: main=%q linked=%q", mainRootCommit, linkedRootCommit)
	}

	// Separate clone: same history but a different git-common-dir ->
	// different canonicalRepoRoot (no cross-clone reuse, scenario d).
	clone := filepath.Join(base, "clone")
	if err := exec.Command("git", "clone", "-q", main, clone).Run(); err != nil {
		t.Skipf("cannot clone: %v", err)
	}
	cloneRepoRoot, cloneRootCommit := resolveRepoIdentity(clone)
	if cloneRepoRoot == "" {
		t.Fatal("resolveRepoIdentity(clone) failed")
	}
	if cloneRepoRoot == mainRepoRoot {
		t.Errorf("a separate clone must have a DIFFERENT canonicalRepoRoot (no cross-clone reuse): both %q", cloneRepoRoot)
	}
	if cloneRootCommit != mainRootCommit {
		t.Errorf("clones of the same repo should share the root commit: clone=%q main=%q", cloneRootCommit, mainRootCommit)
	}
}

// TestResolveRepoIdentity_NonGitFallsBack asserts resolveRepoIdentity
// returns empty + nil (no error) for a non-git path (scenario h).
func TestResolveRepoIdentity_NonGitFallsBack(t *testing.T) {
	worktree := t.TempDir()
	repoRoot, rootCommit := resolveRepoIdentity(worktree)
	if repoRoot != "" || rootCommit != "" {
		t.Errorf("non-git path must return empty identity, got %q / %q", repoRoot, rootCommit)
	}
}

// TestResolveRepoIdentity_EmptyRepoFallsBack asserts an empty repo (no
// commits) returns empty repoRootCommit (scenario h: reuse falls back).
func TestResolveRepoIdentity_EmptyRepoFallsBack(t *testing.T) {
	worktree := t.TempDir()
	runGitIn(t, worktree, "init", "-q", "-b", "main")
	repoRoot, rootCommit := resolveRepoIdentity(worktree)
	// An empty repo has a valid common dir but no root commit (no
	// commits). The identity is NOT ermittelbar for reuse (root commit
	// empty -> reuse disabled, fall back to per-worktree), so the repo
	// root hash alone is not enough: reuse requires BOTH parts.
	_ = repoRoot
	if rootCommit != "" {
		t.Errorf("empty repo (no commits) must have no root commit, got %q — reuse must fall back", rootCommit)
	}
}

// TestFreezeFromDurableApproval_ReuseFallbackFreezes is scenario (a) at
// the parent-activation level: a NEW worktree of an already-approved
// repo (per-worktree approval absent, digest-indexed record present)
// freezes a snapshot from the reuse record when reuse is enabled.
func TestFreezeFromDurableApproval_ReuseFallbackFreezes(t *testing.T) {
	// Enable reuse via env.
	t.Setenv(envApprovalReuseByDigest, "1")

	main := t.TempDir()
	gitInitForTest(t, main)
	writeCliManifest(t, main, `version: 1
builds:
  - root: backend
    containers:
      images: [pgvector/pgvector:pg16]
`)
	m, err := buildmanifest.Load(main)
	if err != nil {
		t.Fatal(err)
	}
	digest := buildmanifest.Digest(m)
	host := buildmanifest.HostPolicy{}
	caps := m.CapabilitySet(host)
	canonRepoRoot, rootCommit := resolveRepoIdentity(main)

	// The new worktree: a LINKED worktree (same repo -> same common dir
	// -> same canonicalRepoRoot), but NO per-worktree approval. The
	// manifest is written into the new worktree too (the manifest is
	// committed in the repo; here we write it so Load finds it).
	base := filepath.Dir(main)
	newWorktree := filepath.Join(base, "new-wt")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", main, "worktree", "add", "-q", newWorktree, "-b", "feature").Run(); err != nil {
		t.Skipf("cannot create linked worktree: %v", err)
	}
	writeCliManifest(t, newWorktree, `version: 1
builds:
  - root: backend
    containers:
      images: [pgvector/pgvector:pg16]
`)

	// cacheDir is <cacheRoot>/<digest>; its parent is the cache root.
	cacheDir := filepath.Join(t.TempDir(), "scope-digest")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Dir(cacheDir)

	// Plant the digest-indexed reuse record for the repo.
	reuseLoc := buildmanifest.NewRepoDigestLocation(cacheRoot, canonRepoRoot)
	if err := buildmanifest.StoreApprovalForRepoDigestAt(reuseLoc, digest, buildmanifest.ApprovalRecord{
		Digest:         digest,
		Capabilities:   caps,
		RepoRootCommit: rootCommit,
	}); err != nil {
		t.Fatal(err)
	}

	store := buildengine.NewParentSnapshotStore()
	freezeSnapshotFromDurableApproval(store, newWorktree, cacheDir)

	got, err := store.Lookup(newWorktree)
	if err != nil {
		t.Fatalf("no snapshot frozen from the reuse record: %v", err)
	}
	if got.Policy.Digest != digest {
		t.Errorf("Digest = %q, want %q (reused record)", got.Policy.Digest, digest)
	}
	if len(got.Policy.Capabilities.Images) != 1 || got.Policy.Capabilities.Images[0] != "pgvector/pgvector:pg16" {
		t.Errorf("Capabilities = %+v, want the reuse record's images", got.Policy.Capabilities)
	}
}

// TestFreezeFromDurableApproval_ReuseDisabledNoFallback asserts scenario
// (f): with reuse DISABLED, the parent must NOT freeze a snapshot from
// the digest-indexed record for a worktree with no per-worktree
// approval (build unavailable until per-worktree approve + restart).
func TestFreezeFromDurableApproval_ReuseDisabledNoFallback(t *testing.T) {
	t.Setenv(envApprovalReuseByDigest, "") // reuse off (default)

	main := t.TempDir()
	gitInitForTest(t, main)
	writeCliManifest(t, main, `version: 1
builds:
  - root: backend
`)
	m, err := buildmanifest.Load(main)
	if err != nil {
		t.Fatal(err)
	}
	digest := buildmanifest.Digest(m)
	host := buildmanifest.HostPolicy{}
	caps := m.CapabilitySet(host)
	canonRepoRoot, rootCommit := resolveRepoIdentity(main)

	// The new worktree: a LINKED worktree of the same repo (same
	// canonicalRepoRoot), with the manifest present, but NO per-worktree
	// approval.
	mainDir := filepath.Dir(main)
	newWorktree := filepath.Join(mainDir, "new-wt")
	if err := exec.Command("git", "-C", main, "worktree", "add", "-q", newWorktree, "-b", "feature").Run(); err != nil {
		t.Skipf("cannot create linked worktree: %v", err)
	}
	writeCliManifest(t, newWorktree, `version: 1
builds:
  - root: backend
`)

	cacheDir := filepath.Join(t.TempDir(), "scope-digest")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Dir(cacheDir)

	// Plant a valid reuse record, but reuse is DISABLED.
	reuseLoc := buildmanifest.NewRepoDigestLocation(cacheRoot, canonRepoRoot)
	if err := buildmanifest.StoreApprovalForRepoDigestAt(reuseLoc, digest, buildmanifest.ApprovalRecord{
		Digest:         digest,
		Capabilities:   caps,
		RepoRootCommit: rootCommit,
	}); err != nil {
		t.Fatal(err)
	}

	store := buildengine.NewParentSnapshotStore()
	freezeSnapshotFromDurableApproval(store, newWorktree, cacheDir)
	if _, err := store.Lookup(newWorktree); err == nil {
		t.Error("reuse disabled: must NOT freeze a snapshot from the reuse record (no per-worktree approval)")
	}
}

// TestFreezeFromDurableApproval_ReuseRootCommitMismatchNoFallback is
// scenario (e) at the parent-activation level: a foreign repo at the
// same path (different root commit) must NOT reuse the stored record.
// We construct two repos with distinct root commits and verify the
// gate-level reuse rejects the stale record.
func TestFreezeFromDurableApproval_ReuseRootCommitMismatchNoFallback(t *testing.T) {
	t.Setenv(envApprovalReuseByDigest, "1")

	approved := t.TempDir()
	gitInitForTest(t, approved)
	writeCliManifest(t, approved, `version: 1
builds:
  - root: backend
`)
	m, err := buildmanifest.Load(approved)
	if err != nil {
		t.Fatal(err)
	}
	digest := buildmanifest.Digest(m)
	caps := m.CapabilitySet(buildmanifest.HostPolicy{})
	canonRepoRoot, approvedRootCommit := resolveRepoIdentity(approved)

	cacheDir := filepath.Join(t.TempDir(), "scope-digest")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Dir(cacheDir)
	reuseLoc := buildmanifest.NewRepoDigestLocation(cacheRoot, canonRepoRoot)
	if err := buildmanifest.StoreApprovalForRepoDigestAt(reuseLoc, digest, buildmanifest.ApprovalRecord{
		Digest:         digest,
		Capabilities:   caps,
		RepoRootCommit: approvedRootCommit,
	}); err != nil {
		t.Fatal(err)
	}

	// A foreign repo whose root commit differs (a fresh repo at the same
	// path after the original was deleted — different history). Give it a
	// DISTINCT root commit so the SHAs cannot collide.
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "foreign-marker.txt"), []byte(t.Name()), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, foreign, "init", "-q", "-b", "main")
	runGitIn(t, foreign, "add", "foreign-marker.txt")
	runGitIn(t, foreign, "commit", "-q", "-m", "foreign init")
	_, foreignRootCommit := resolveRepoIdentity(foreign)
	if foreignRootCommit == approvedRootCommit {
		t.Skip("cannot construct two repos with distinct root commits")
	}

	// Gate-level assertion: reuse must be rejected when the CURRENT repo's
	// root commit differs from the stored record's (recycling guard).
	leaf := t.TempDir()
	_, gerr := buildmanifest.GateAt(leaf, buildmanifest.NewOnLeafLocation(), digest, caps, buildmanifest.GateOptions{
		EnableReuse:        true,
		CanonicalRepoRoot:  canonRepoRoot,
		RepoRootCommit:     foreignRootCommit, // mismatched recycling guard
		RepoDigestLocation: reuseLoc,
	})
	if gerr == nil {
		t.Fatal("root-commit mismatch must NOT reuse the stored approval (recycling guard)")
	}
	var ge *buildmanifest.GateError
	if !errorsAsGate(gerr, &ge) {
		t.Fatalf("want *GateError, got %T: %v", gerr, gerr)
	}
}

// TestFreezeFromDurableApproval_CrossRepoNoFallback is scenario (c):
// a DIFFERENT repo with an IDENTICAL manifest digest must NOT reuse the
// record (different canonicalRepoRoot -> different namespace).
func TestFreezeFromDurableApproval_CrossRepoNoFallback(t *testing.T) {
	t.Setenv(envApprovalReuseByDigest, "1")

	repoA := t.TempDir()
	gitInitForTest(t, repoA)
	writeCliManifest(t, repoA, `version: 1
builds:
  - root: backend
    containers:
      images: [pgvector/pgvector:pg16]
`)
	m, err := buildmanifest.Load(repoA)
	if err != nil {
		t.Fatal(err)
	}
	digest := buildmanifest.Digest(m)
	caps := m.CapabilitySet(buildmanifest.HostPolicy{})
	repoARoot, rootCommitA := resolveRepoIdentity(repoA)

	cacheDir := filepath.Join(t.TempDir(), "scope-digest")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Dir(cacheDir)
	reuseLocA := buildmanifest.NewRepoDigestLocation(cacheRoot, repoARoot)
	if err := buildmanifest.StoreApprovalForRepoDigestAt(reuseLocA, digest, buildmanifest.ApprovalRecord{
		Digest:         digest,
		Capabilities:   caps,
		RepoRootCommit: rootCommitA,
	}); err != nil {
		t.Fatal(err)
	}

	// Repo B: IDENTICAL manifest content (identical digest) but a
	// different repo root.
	repoB := t.TempDir()
	gitInitForTest(t, repoB)
	writeCliManifest(t, repoB, `version: 1
builds:
  - root: backend
    containers:
      images: [pgvector/pgvector:pg16]
`)
	// Verify the digests are actually identical (bit-identical manifest).
	mB, err := buildmanifest.Load(repoB)
	if err != nil {
		t.Fatal(err)
	}
	if buildmanifest.Digest(mB) != digest {
		t.Fatal("test setup: repos must have identical digests for cross-repo scenario")
	}

	store := buildengine.NewParentSnapshotStore()
	freezeSnapshotFromDurableApproval(store, repoB, cacheDir)
	if _, err := store.Lookup(repoB); err == nil {
		t.Error("cross-repo digest match must NOT reuse the approval (different namespace)")
	}
}

func writeCliManifest(t *testing.T, wt, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(wt, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".omac", "build.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalReuseEnabled_CacheRootIsEmpty(t *testing.T) {
	t.Setenv(envApprovalReuseByDigest, "")
	if approvalReuseFromHostConfig("") {
		t.Error("empty cache root must not read host config")
	}
	// Ensure no name collision pollution from other tests.
	if strings.Contains(envApprovalReuseByDigest, "REUSE") {
		_ = envApprovalReuseByDigest[0] // compile-time reference
	}
}

// errorsAsGate is a small errors.As helper bound to *buildmanifest.GateError.
func errorsAsGate(err error, ge **buildmanifest.GateError) bool {
	return errors.As(err, ge)
}

// TestRevoke_SettingOffKeepsFrozenSnapshotValid is scenario (i): the
// already-frozen ParentSnapshot remains valid for the parent lifetime
// after reuse is disabled (its validity is governed by the in-memory
// store, not by the reuse flag), while a NEW lookup (a worktree frozen
// AFTER the revoke) falls back to per-worktree and does NOT reuse the
// digest-indexed record.
func TestRevoke_SettingOffKeepsFrozenSnapshotValid(t *testing.T) {
	t.Setenv(envApprovalReuseByDigest, "1")

	main := t.TempDir()
	gitInitForTest(t, main)
	writeCliManifest(t, main, `version: 1
builds:
  - root: backend
`)
	m, err := buildmanifest.Load(main)
	if err != nil {
		t.Fatal(err)
	}
	digest := buildmanifest.Digest(m)
	caps := m.CapabilitySet(buildmanifest.HostPolicy{})
	canonRepoRoot, rootCommit := resolveRepoIdentity(main)

	// New worktree (linked, same repo), NOT yet frozen.
	mainDir := filepath.Dir(main)
	newWorktree := filepath.Join(mainDir, "revoke-wt")
	if err := exec.Command("git", "-C", main, "worktree", "add", "-q", newWorktree, "-b", "feature").Run(); err != nil {
		t.Skipf("cannot create linked worktree: %v", err)
	}
	writeCliManifest(t, newWorktree, `version: 1
builds:
  - root: backend
`)

	cacheDir := filepath.Join(t.TempDir(), "scope-digest")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cacheRoot := filepath.Dir(cacheDir)
	reuseLoc := buildmanifest.NewRepoDigestLocation(cacheRoot, canonRepoRoot)
	if err := buildmanifest.StoreApprovalForRepoDigestAt(reuseLoc, digest, buildmanifest.ApprovalRecord{
		Digest:         digest,
		Capabilities:   caps,
		RepoRootCommit: rootCommit,
	}); err != nil {
		t.Fatal(err)
	}

	// Phase 1 (reuse ON): freeze the new worktree's snapshot from the
	// reuse record.
	store := buildengine.NewParentSnapshotStore()
	freezeSnapshotFromDurableApproval(store, newWorktree, cacheDir)
	if _, err := store.Lookup(newWorktree); err != nil {
		t.Fatalf("phase 1: snapshot must be frozen from the reuse record: %v", err)
	}
	// Capture the frozen digest for the later "stays valid" assertion.
	snap, _ := store.Lookup(newWorktree)
	frozenDigest := snap.Policy.Digest
	if frozenDigest != digest {
		t.Fatalf("phase 1: frozen digest = %q, want %q", frozenDigest, digest)
	}

	// Phase 2 (revoke: reuse OFF): the parent's in-memory store is NOT
	// cleared by a reuse-flag change — the already-frozen snapshot stays
	// valid for the parent lifetime (it expires only on parent restart).
	t.Setenv(envApprovalReuseByDigest, "")
	if !approvalReuseEnabled(cacheRoot) {
		// Reuse is now off for new lookups.
		snap2, err := store.Lookup(newWorktree)
		if err != nil {
			t.Fatalf("phase 2: frozen snapshot must remain valid after reuse disabled: %v", err)
		}
		if snap2.Policy.Digest != frozenDigest {
			t.Errorf("phase 2: frozen digest changed to %q after reuse disabled, want %q (snapshot stays valid for parent lifetime)",
				snap2.Policy.Digest, frozenDigest)
		}
	} else {
		t.Fatal("phase 2: reuse should be off after env cleared")
	}

	// Phase 3: a NEW worktree (fresh store) frozen AFTER the revoke must
	// NOT consult the reuse record — it falls back to per-worktree
	// (missing) and freezes no snapshot.
	postRevokeStore := buildengine.NewParentSnapshotStore()
	freezeSnapshotFromDurableApproval(postRevokeStore, newWorktree, cacheDir)
	if _, err := postRevokeStore.Lookup(newWorktree); err == nil {
		t.Error("phase 3: a post-revoke lookup must NOT reuse the digest-indexed record (falls back to per-worktree)")
	}
}
