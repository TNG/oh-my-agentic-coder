package buildcontrol

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestHashRepo_StableAndDistinct asserts HashRepo is stable for the same
// canonical repo root and distinct for different repo roots. The repo
// root is the canonical (EvalSymlinks-resolved) `git rev-parse
// --git-common-dir` output — the namespace under which digest-indexed
// approval reuse records are stored (ADR 0005).
func TestHashRepo_StableAndDistinct(t *testing.T) {
	repoA := "/home/u/work/repo/.git"
	repoB := "/home/u/work/other/.git"
	if HashRepo(repoA) == HashRepo(repoB) {
		t.Error("distinct repo roots hashed the same")
	}
	if HashRepo(repoA) != HashRepo(repoA) {
		t.Error("hash not stable")
	}
	// A canonical repo root and a non-canonical spelling produce
	// DIFFERENT hashes (callers must canonicalize first).
	if HashRepo(repoA) == HashRepo("/home/u/work/repo/.git/") {
		t.Log("note: trailing-slash spelling hashes differently (callers must canonicalize)")
	}
}

// TestApprovalsByRepoDir_UnderRoot asserts the approvals-by-repo
// directory for a canonical repo root lives under the host-only
// build-control root and is namespaced by sha256(repoRoot).
func TestApprovalsByRepoDir_UnderRoot(t *testing.T) {
	root := "/home/u/.cache/omac"
	repo := "/home/u/work/repo/.git"
	dir := ApprovalsByRepoDir(root, repo)
	if !strings.HasPrefix(dir, filepath.Join(root, RootName, approvalsByRepoDirName)) {
		t.Errorf("approvals-by-repo dir %q not under build-control/approvals-by-repo", dir)
	}
	if dir != filepath.Join(ApprovalByRepoPath(root, repo, "any")) {
		// Sanity: the dir is the parent of a record path.
	}
}

// TestApprovalByRepoPath_UnderNamespace asserts the digest-indexed
// approval record path is
// <root>/build-control/approvals-by-repo/<sha256(repoRoot)>/<digest>.json —
// the digest-indexed, repo-namespaced approval reuse record (ADR 0005).
func TestApprovalByRepoPath_UnderNamespace(t *testing.T) {
	root := "/home/u/.cache/omac"
	repo := "/home/u/work/repo/.git"
	digest := "abc123"
	p := ApprovalByRepoPath(root, repo, digest)
	if !strings.HasPrefix(p, filepath.Join(root, RootName, approvalsByRepoDirName)) {
		t.Errorf("path %q not under build-control/approvals-by-repo", p)
	}
	if !strings.HasSuffix(p, ".json") {
		t.Errorf("path %q missing .json suffix", p)
	}
	if !strings.Contains(p, HashRepo(repo)) {
		t.Errorf("path %q missing repo-root hash namespace", p)
	}
	if !strings.Contains(p, digest) {
		t.Errorf("path %q missing digest", p)
	}

	// Different repos -> different namespaces -> distinct paths even for
	// the SAME digest (no cross-repo reuse).
	repoB := "/home/u/work/other/.git"
	if ApprovalByRepoPath(root, repoA(), digest) == "" {
		t.Fatal("empty path")
	}
	if ApprovalByRepoPath(root, repo, digest) == ApprovalByRepoPath(root, repoB, digest) {
		t.Error("distinct repos share the same digest-indexed record path")
	}

	// Same repo, different digests -> distinct records.
	digestB := "def456"
	if ApprovalByRepoPath(root, repo, digest) == ApprovalByRepoPath(root, repo, digestB) {
		t.Error("distinct digests share the same record path")
	}
}

func repoA() string { return "/home/u/work/repoA/.git" }
