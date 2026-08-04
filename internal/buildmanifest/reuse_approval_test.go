package buildmanifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRepoDigestApproval_RoundTrip asserts StoreApprovalForRepoDigest
// writes a digest-indexed, repo-namespaced approval record and
// LookupApprovalForRepoDigest reads it back. The record lives at
// <root>/build-control/approvals-by-repo/<sha256(canonicalRepoRoot)>/<digest>.json
// and carries the repoRootCommit recycling guard (ADR 0005).
func TestRepoDigestApproval_RoundTrip(t *testing.T) {
	cacheRoot := t.TempDir()
	repoA := "/home/u/work/repo/.git"
	digest := "abc123"
	rec := ApprovalRecord{
		Digest:         digest,
		Capabilities:   CapabilitySet{BuildRoots: []string{"backend"}, Images: []string{"pgvector/pgvector:pg16"}},
		RepoRootCommit: "ca9a9845294ed8c0f77dc2fe3f2efd84ee16e47c",
	}
	rec.ApprovedAt = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	if err := StoreApprovalForRepoDigest(cacheRoot, repoA, digest, rec); err != nil {
		t.Fatalf("StoreApprovalForRepoDigest: %v", err)
	}

	got, err := LookupApprovalForRepoDigest(cacheRoot, repoA, digest)
	if err != nil {
		t.Fatalf("LookupApprovalForRepoDigest: %v", err)
	}
	if got.Digest != rec.Digest {
		t.Errorf("Digest = %q, want %q", got.Digest, rec.Digest)
	}
	if got.RepoRootCommit != rec.RepoRootCommit {
		t.Errorf("RepoRootCommit = %q, want %q", got.RepoRootCommit, rec.RepoRootCommit)
	}
	if len(got.Capabilities.Images) != 1 || got.Capabilities.Images[0] != "pgvector/pgvector:pg16" {
		t.Errorf("Capabilities.Images = %v", got.Capabilities.Images)
	}
	// The file lives under the namespaced approvals-by-repo tree.
	dir := filepath.Join(cacheRoot, "build-control", "approvals-by-repo", repoHashHex(repoA))
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(paths) != 1 {
		t.Errorf("expected one record file under %s, got %v", dir, paths)
	}
}

// TestRepoDigestApproval_NamespacedByRepo asserts two different repos
// with the SAME digest land in different namespaces (no cross-repo
// reuse), and the same repo with DIFFERENT digests stores distinct
// records (a changed manifest always requires fresh approval).
func TestRepoDigestApproval_NamespacedByRepo(t *testing.T) {
	cacheRoot := t.TempDir()
	repoA := "/home/u/work/repo/.git"
	repoB := "/home/u/work/other/.git"
	digest := "same-digest"
	rec := ApprovalRecord{Digest: digest, RepoRootCommit: "commita"}
	if err := StoreApprovalForRepoDigest(cacheRoot, repoA, digest, rec); err != nil {
		t.Fatal(err)
	}
	// Cross-repo: repoB has no record for this digest.
	got, err := LookupApprovalForRepoDigest(cacheRoot, repoB, digest)
	if err != nil {
		t.Fatalf("LookupApprovalForRepoDigest(repoB): %v", err)
	}
	if got.Digest != "" {
		t.Errorf("cross-repo lookup returned a record: %+v", got)
	}
	// Same repo, distinct digest: a separate record exists.
	if err := StoreApprovalForRepoDigest(cacheRoot, repoA, "other-digest", ApprovalRecord{Digest: "other-digest", RepoRootCommit: "commita"}); err != nil {
		t.Fatal(err)
	}
	got2, _ := LookupApprovalForRepoDigest(cacheRoot, repoA, "other-digest")
	if got2.Digest != "other-digest" {
		t.Errorf("distinct-digest record not found: %+v", got2)
	}
}

// TestApprovalRecord_RepoRootCommitOmitempty asserts the RepoRootCommit
// field is omitted from the JSON when empty, so EXISTING per-worktree
// approval records (which predate ADR 0005) still marshal/unmarshal
// unchanged and the new field is additive.
func TestApprovalRecord_RepoRootCommitOmitempty(t *testing.T) {
	leaf := t.TempDir()
	rec := ApprovalRecord{Digest: "abc123", Capabilities: CapabilitySet{Images: []string{"postgres:17"}}}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "repoRootCommit") {
		t.Errorf("empty RepoRootCommit must be omitempty-omitted, got %s", data)
	}
	// The record round-trips through JSON without the field.
	var parsed ApprovalRecord
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Digest != rec.Digest {
		t.Errorf("Digest = %q, want %q", parsed.Digest, rec.Digest)
	}
	// Legacy per-worktree round-trip still works via the standard path.
	if err := StoreApprovalAt(leaf, NewOnLeafLocation(), rec); err != nil {
		t.Fatal(err)
	}
	back, err := LoadApprovalAt(leaf, NewOnLeafLocation())
	if err != nil {
		t.Fatal(err)
	}
	if back.Digest != rec.Digest || back.RepoRootCommit != "" {
		t.Errorf("legacy round-trip: %+v", back)
	}
}

// repoHashHex is a small stand-in for the canonical repo-root hash used
// in the assertion (the test verifies the record lands under the
// <sha256(canonicalRepoRoot)> namespace; the name is checked by
// buildcontrol's own unit tests).
func repoHashHex(s string) string {
	d := sha256.Sum256([]byte(s))
	return hex.EncodeToString(d[:])
}
