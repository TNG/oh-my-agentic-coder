package buildmanifest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIdentityForTest runs the git commands that resolve the repo
// identity (canonicalRepoRoot via `git rev-parse --git-common-dir` +
// EvalSymlinks, and repoRootCommit via `git rev-list --max-parents=0
// HEAD`) against the test worktree. It returns (canonicalRepoRoot,
// repoRootCommit). Tests that hit the gate with reuse enabled use this
// to compute the identity to store/look up the digest-indexed record.
func gitIdentityForTest(t *testing.T, worktree string) (canonicalRepoRoot, repoRootCommit string) {
	t.Helper()
	common, err := gitOutputForTest(worktree, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatalf("git rev-parse --git-common-dir: %v", err)
	}
	// git may return a relative common dir (".git") for the main
	// worktree; make it absolute against the worktree before resolving
	// symlinks, exactly as production resolveRepoIdentity does.
	if !filepath.IsAbs(common) {
		common = filepath.Join(worktree, common)
	}
	canon, err := filepath.EvalSymlinks(common)
	if err != nil {
		t.Fatalf("EvalSymlinks(common-dir %q): %v", common, err)
	}
	rootCommit, err := gitOutputForTest(worktree, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		t.Fatalf("git rev-list --max-parents=0 HEAD: %v", err)
	}
	return canon, rootCommit
}

func gitOutputForTest(worktree string, args ...string) (string, error) {
	c := exec.Command("git", args...)
	c.Dir = worktree
	out, err := c.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// initGitRepoForTest creates a git repo in worktree with one empty
// commit (so a root commit exists), using a hermetic identity.
func initGitRepoForTest(t *testing.T, worktree string) string {
	t.Helper()
	gitInDir := func(args ...string) {
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
	gitInDir("init", "-q", "-b", "main")
	gitInDir("commit", "-q", "--allow-empty", "-m", "init")
	return worktree
}

// initGitRepoWithFileForTest initializes a git repo whose ROOT commit
// includes the on-disk files already present in worktree (so the root
// commit SHA is content-dependent and distinct from an empty commit).
func initGitRepoWithFileForTest(t *testing.T, worktree string) {
	t.Helper()
	gitInDir := func(args ...string) {
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
	gitInDir("init", "-q", "-b", "main")
	gitInDir("add", "-A")
	gitInDir("commit", "-q", "-m", "foreign init")
}

// TestGate_NoReuseWhenDisabled asserts the reuse path is NOT consulted
// when the reuse flag is unset (scenario f): even when a valid
// digest-indexed record exists for the repo, GateAt must not reuse it —
// the per-worktree approval is the ONLY path, so a worktree with no
// per-worktree approval fails with a GateError (first-ever).
func TestGate_NoReuseWhenDisabled(t *testing.T) {
	worktree := t.TempDir()
	initGitRepoForTest(t, worktree)
	writeBuildManifest(t, worktree, `version: 1
builds:
  - root: backend
    containers:
      images: [postgres:17]
`)
	m, err := Load(worktree)
	if err != nil {
		t.Fatal(err)
	}
	host := HostPolicy{MaxHeap: "4g"}
	caps := m.CapabilitySet(host)
	digest := Digest(m)
	canonRepoRoot, rootCommit := gitIdentityForTest(t, worktree)

	leaf := t.TempDir()
	_ = leaf                   // gate's leaf arg is only used by the per-worktree location's on-leaf path
	loc := NewOnLeafLocation() // per-worktree approval location (empty -> no per-worktree approval)
	// Plant a valid digest-indexed reuse record.
	reuseLoc := NewRepoDigestLocation(t.TempDir(), canonRepoRoot)
	if err := StoreApprovalForRepoDigestAt(reuseLoc, digest, ApprovalRecord{
		Digest:         digest,
		Capabilities:   caps,
		RepoRootCommit: rootCommit,
	}); err != nil {
		t.Fatal(err)
	}

	// Reuse disabled (flag=false): the digest-indexed record must NOT be
	// consulted. The per-worktree gate fails (first-ever).
	_, err = GateAt(worktree, loc, digest, caps, GateOptions{})
	if err == nil {
		t.Fatal("gate with reuse disabled must fail (no per-worktree approval)")
	}
	var ge *GateError
	if !asGateError(err, &ge) {
		t.Fatalf("want *GateError, got %T: %v", err, err)
	}
	if !ge.FirstEver {
		t.Error("expected first-ever denial (reuse path must not be consulted when disabled)")
	}
}

// TestGate_ReuseSameRepoSameDigestSucceeds is scenario (a): a NEW
// worktree of an already-approved repo builds when the digest-indexed
// reuse record matches (same repo, same root commit). The per-worktree
// path misses; the reuse path hits.
func TestGate_ReuseSameRepoSameDigestSucceeds(t *testing.T) {
	// Approved repo: worktree A writes the digest-indexed record.
	repoWorktree := t.TempDir()
	initGitRepoForTest(t, repoWorktree)
	writeBuildManifest(t, repoWorktree, `version: 1
builds:
  - root: backend
    containers:
      images: [postgres:17]
`)
	mA, err := Load(repoWorktree)
	if err != nil {
		t.Fatal(err)
	}
	host := HostPolicy{MaxHeap: "4g"}
	caps := mA.CapabilitySet(host)
	digest := Digest(mA)
	canonRepoRoot, rootCommit := gitIdentityForTest(t, repoWorktree)

	cacheRoot := t.TempDir()
	reuseLoc := NewRepoDigestLocation(cacheRoot, canonRepoRoot)
	if err := StoreApprovalForRepoDigestAt(reuseLoc, digest, ApprovalRecord{
		Digest:         digest,
		Capabilities:   caps,
		RepoRootCommit: rootCommit,
	}); err != nil {
		t.Fatal(err)
	}

	// Per-worktree approval (same location used by both worktrees) is
	// deliberately empty: the new worktree has no per-worktree approval.
	loc := NewBuildControlLocation(cacheRoot, "/worktree/new")
	leaf := t.TempDir()

	res, err := GateAt(leaf, loc, digest, caps, GateOptions{
		EnableReuse:        true,
		CanonicalRepoRoot:  canonRepoRoot,
		RepoRootCommit:     rootCommit,
		RepoDigestLocation: reuseLoc,
	})
	if err != nil {
		t.Fatalf("reuse should succeed for a new worktree of an approved repo: %v", err)
	}
	if res.Digest != digest {
		t.Errorf("Digest = %q, want %q", res.Digest, digest)
	}
	if !res.Capabilities.HasBuildRoot("backend") {
		t.Error("reused caps missing build root")
	}
}

// TestGate_ReuseDigestMismatchFails is scenario (b): a changed manifest
// (different digest) has no digest-indexed reuse record -> no reuse,
// falls back to per-worktree -> missing per-worktree approval -> exit-3
// GateError (not first-ever necessarily; here there is no approval so
// first-ever).
func TestGate_ReuseDigestMismatchFails(t *testing.T) {
	repoWorktree := t.TempDir()
	initGitRepoForTest(t, repoWorktree)
	writeBuildManifest(t, repoWorktree, `version: 1
builds:
  - root: backend
    containers:
      images: [postgres:17]
`)
	m, err := Load(repoWorktree)
	if err != nil {
		t.Fatal(err)
	}
	host := HostPolicy{MaxHeap: "4g"}
	caps := m.CapabilitySet(host)
	digest := Digest(m)
	canonRepoRoot, rootCommit := gitIdentityForTest(t, repoWorktree)

	cacheRoot := t.TempDir()
	reuseLoc := NewRepoDigestLocation(cacheRoot, canonRepoRoot)
	// The reused/digest-indexed digest differs from the CURRENT manifest.
	changedDigest := "different-digest"
	if err := StoreApprovalForRepoDigestAt(reuseLoc, changedDigest, ApprovalRecord{
		Digest:         changedDigest,
		Capabilities:   caps,
		RepoRootCommit: rootCommit,
	}); err != nil {
		t.Fatal(err)
	}

	loc := NewBuildControlLocation(cacheRoot, "/worktree/new")
	leaf := t.TempDir()
	_, err = GateAt(leaf, loc, digest, caps, GateOptions{
		EnableReuse:        true,
		CanonicalRepoRoot:  canonRepoRoot,
		RepoRootCommit:     rootCommit,
		RepoDigestLocation: reuseLoc,
	})
	if err == nil {
		t.Fatal("changed manifest must NOT reuse a different-digest approval")
	}
	var ge *GateError
	if !asGateError(err, &ge) {
		t.Fatalf("want *GateError, got %T: %v", err, err)
	}
}

// TestGate_ReuseRootCommitMismatchFails is scenario (e): a foreign repo
// created at the same path after the original was deleted has a
// different root commit -> the stored record's RepoRootCommit no longer
// matches -> no reuse, falls back to per-worktree (missing) -> GateError.
func TestGate_ReuseRootCommitMismatchFails(t *testing.T) {
	// Repo 1: the approved repo.
	repoWorktree := t.TempDir()
	initGitRepoForTest(t, repoWorktree)
	writeBuildManifest(t, repoWorktree, `version: 1
builds:
  - root: backend
    containers:
      images: [postgres:17]
`)
	m, err := Load(repoWorktree)
	if err != nil {
		t.Fatal(err)
	}
	host := HostPolicy{MaxHeap: "4g"}
	digest := Digest(m)
	canonRepoRoot, rootCommitOfApproved := gitIdentityForTest(t, repoWorktree)

	cacheRoot := t.TempDir()
	reuseLoc := NewRepoDigestLocation(cacheRoot, canonRepoRoot)
	if err := StoreApprovalForRepoDigestAt(reuseLoc, digest, ApprovalRecord{
		Digest:         digest,
		Capabilities:   m.CapabilitySet(host),
		RepoRootCommit: rootCommitOfApproved,
	}); err != nil {
		t.Fatal(err)
	}

	// Now the FOREIGN repo at the SAME path: delete the original, create
	// a new repo with a DIFFERENT history (different root commit). The
	// canonicalRepoRoot is the same (same path), but the root commit
	// differs.
	if err := os.RemoveAll(repoWorktree); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repoWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	// The foreign repo gets a DISTINCT root commit (a marker file in the
	// root commit) so the SHA cannot collide with the approved repo's.
	foreignRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreignRepo, "foreign-marker.txt"), []byte(t.Name()), 0o644); err != nil {
		t.Fatal(err)
	}
	initGitRepoWithFileForTest(t, foreignRepo)
	// The foreign repo's common dir resolves to its own path, NOT the
	// approval's namespace. To simulate the recycling attack (same path,
	// different history) we assert the recorded root commit does NOT match
	// the current repo's root commit for the SAME canonicalRepoRoot.
	// We brute-force: the current root commit here is the foreign one.
	_, foreignRootCommit := gitIdentityForTest(t, foreignRepo)
	if foreignRootCommit == rootCommitOfApproved {
		t.Skip("cannot construct two repos with distinct root commits")
	}

	loc := NewBuildControlLocation(cacheRoot, "/worktree/new")
	leaf := t.TempDir()
	_, err = GateAt(leaf, loc, digest, m.CapabilitySet(host), GateOptions{
		EnableReuse:       true,
		CanonicalRepoRoot: canonRepoRoot,
		// The current repo's root commit (the foreign repo) differs from
		// the stored record's.
		RepoRootCommit:     foreignRootCommit,
		RepoDigestLocation: reuseLoc,
	})
	if err == nil {
		t.Fatal("root-commit mismatch must NOT reuse the stored approval")
	}
	var ge *GateError
	if !asGateError(err, &ge) {
		t.Fatalf("want *GateError, got %T: %v", err, err)
	}
}

// TestGate_ReuseHostCeilingDropInvalidates is scenario (g): a host
// ceiling drop below what was approved invalidates the reused record
// exactly as it invalidates a per-worktree record — the gate re-records
// against the new (lower) set and fails with a GateError.
func TestGate_ReuseHostCeilingDropInvalidates(t *testing.T) {
	repoWorktree := t.TempDir()
	initGitRepoForTest(t, repoWorktree)
	writeBuildManifest(t, repoWorktree, `version: 1
resources:
  maxHeap: 3g
`)
	m, err := Load(repoWorktree)
	if err != nil {
		t.Fatal(err)
	}
	hostHi := HostPolicy{MaxHeap: "4g"}
	digest := Digest(m)
	canonRepoRoot, rootCommit := gitIdentityForTest(t, repoWorktree)

	cacheRoot := t.TempDir()
	reuseLoc := NewRepoDigestLocation(cacheRoot, canonRepoRoot)
	if err := StoreApprovalForRepoDigestAt(reuseLoc, digest, ApprovalRecord{
		Digest:         digest,
		Capabilities:   m.CapabilitySet(hostHi),
		RepoRootCommit: rootCommit,
	}); err != nil {
		t.Fatal(err)
	}

	// Gate with the ceiling DROPPED: 3g request but host ceiling 2g.
	hostLo := HostPolicy{MaxHeap: "2g"}
	loc := NewBuildControlLocation(cacheRoot, "/worktree/new")
	leaf := t.TempDir()
	_, err = GateAt(leaf, loc, digest, m.CapabilitySet(hostLo), GateOptions{
		EnableReuse:        true,
		CanonicalRepoRoot:  canonRepoRoot,
		RepoRootCommit:     rootCommit,
		RepoDigestLocation: reuseLoc,
	})
	if err == nil {
		t.Fatal("host-ceiling drop must invalidate the reused record")
	}
	var ge *GateError
	if !asGateError(err, &ge) {
		t.Fatalf("want *GateError, got %T: %v", err, err)
	}
	if !strings.Contains(ge.Error(), "host policy ceiling dropped") {
		t.Errorf("error should mention ceiling drop: %v", ge)
	}
}

func writeBuildManifest(t *testing.T, worktree, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(worktree, ".omac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".omac", "build.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func asGateError(err error, ge **GateError) bool {
	e, ok := err.(*GateError)
	if ok {
		*ge = e
	}
	return ok
}
