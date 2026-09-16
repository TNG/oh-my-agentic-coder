package skilltrust

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// isolate points HOME and XDG_CONFIG_HOME at temp dirs so the approvals
// store resolves under a throwaway location (registry.GlobalDir honors
// XDG_CONFIG_HOME).
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// skillDir makes a minimal real skill directory. Approve requires one (it
// freezes a snapshot), so the store-bookkeeping tests below pass this rather
// than a bare name.
func skillDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "omac.yaml"), []byte("name: s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// bundleHash computes the real bundle hash for a directory.
func bundleHash(t *testing.T, dir string) string {
	t.Helper()
	h, err := config.BundleHash(dir)
	if err != nil {
		t.Fatalf("BundleHash(%s): %v", dir, err)
	}
	return h
}

// approveDir approves a skill directory using its real bundle hash.
func approveDir(t *testing.T, name, dir string) string {
	t.Helper()
	h := bundleHash(t, dir)
	if err := Approve(name, h, dir); err != nil {
		t.Fatalf("Approve(%q): %v", name, err)
	}
	return h
}

func TestUnapprovedByDefault(t *testing.T) {
	isolate(t)
	if Exists() {
		t.Fatal("store should not exist before any approval")
	}
	ok, err := IsApproved("skill", "sha256:abc")
	if err != nil {
		t.Fatalf("IsApproved: %v", err)
	}
	if ok {
		t.Error("nothing should be approved on a fresh store (fail closed)")
	}
}

func TestApproveThenIsApproved(t *testing.T) {
	isolate(t)
	d := skillDir(t)
	h := approveDir(t, "skill", d)
	if !Exists() {
		t.Error("store should exist after Approve")
	}
	ok, _ := IsApproved("skill", h)
	if !ok {
		t.Error("approved (name, hash) should be approved")
	}
	// A different hash for the same name is NOT approved (content-keyed).
	if ok, _ := IsApproved("skill", "sha256:different"); ok {
		t.Error("a different bundle hash must not be approved")
	}
	// A different name is not approved.
	if ok, _ := IsApproved("other", h); ok {
		t.Error("a different name must not be approved")
	}
}

func TestApproveIsAdditivePerName(t *testing.T) {
	isolate(t)
	// The same name may be registered under multiple harnesses / workdirs,
	// each with its own content, so approvals must accumulate — not clobber.
	d1 := skillDir(t)
	d2 := skillDir(t)
	// Make d2 distinct so BundleHash produces a different hash.
	if err := os.WriteFile(filepath.Join(d2, "v2.txt"), []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	h1 := approveDir(t, "skill", d1)
	h2 := approveDir(t, "skill", d2)

	for _, h := range []string{h1, h2} {
		if ok, _ := IsApproved("skill", h); !ok {
			t.Errorf("hash %s should remain approved (additive per name)", h)
		}
	}
	// Re-approving an identical (name, hash) is idempotent (no duplicate).
	if err := Approve("skill", h1, d1); err != nil {
		t.Fatalf("re-approve: %v", err)
	}
	s, _ := load()
	if len(s.Approved) != 2 {
		t.Errorf("expected 2 approvals, got %d", len(s.Approved))
	}
}

func TestRevokeIsScopedToHash(t *testing.T) {
	isolate(t)
	// Same name, two different content trees (two harnesses), one other skill.
	dFooA := skillDir(t)
	dFooB := skillDir(t) // different dir => different hash (different inode/dir path in hash)
	// Make dFooB content distinct.
	if err := os.WriteFile(filepath.Join(dFooB, "extra.txt"), []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	dBar := skillDir(t)
	hFooA := approveDir(t, "foo", dFooA)
	hFooB := approveDir(t, "foo", dFooB)
	hBar := approveDir(t, "bar", dBar)

	removed, err := Revoke("foo", hFooA)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !removed {
		t.Error("Revoke should report removal")
	}
	if ok, _ := IsApproved("foo", hFooA); ok {
		t.Error("the revoked (name, hash) should no longer be approved")
	}
	// A same-name copy under a different hash keeps its approval.
	if ok, _ := IsApproved("foo", hFooB); !ok {
		t.Error("Revoke must not touch a same-name copy with a different hash")
	}
	if ok, _ := IsApproved("bar", hBar); !ok {
		t.Error("Revoke must not touch other skills")
	}
	if removed, _ := Revoke("foo", "sha256:missing"); removed {
		t.Error("revoking a missing (name, hash) should report nothing removed")
	}
}

func TestEnsureInitializedClosesFirstUpgradeWindow(t *testing.T) {
	isolate(t)
	if Exists() {
		t.Fatal("store should be absent initially")
	}
	if err := EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if !Exists() {
		t.Error("store must exist after EnsureInitialized, so the first-upgrade window closes")
	}
	// Idempotent and non-destructive: approve, then EnsureInitialized again.
	d := skillDir(t)
	h := approveDir(t, "s", d)
	if err := EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized (2nd): %v", err)
	}
	if ok, _ := IsApproved("s", h); !ok {
		t.Error("EnsureInitialized must not clobber existing approvals")
	}
}

func TestApprovalsSurviveReload(t *testing.T) {
	isolate(t)
	approveDir(t, "skill", skillDir(t))
	// A fresh Load (new process would do the same) sees the persisted state.
	s, err := load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(s.Approved) != 1 || s.Approved[0].Name != "skill" {
		t.Fatalf("persisted store = %+v", s.Approved)
	}
}

func TestFailClosedWithoutHome(t *testing.T) {
	// No HOME and no XDG => no host-only dir can be resolved.
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if os.Getenv("HOME") != "" {
		t.Skip("HOME could not be cleared on this platform")
	}
	if err := Approve("x", "h", t.TempDir()); err != errNoGlobalDir {
		t.Errorf("Approve without a config dir = %v, want errNoGlobalDir", err)
	}
	if ok, _ := IsApproved("x", "h"); ok {
		t.Error("must fail closed when no store location is resolvable")
	}
}

func TestSnapshotFreezesContentAndRevokeRemovesIt(t *testing.T) {
	isolate(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "code.txt"), []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Approve with a real dir -> a snapshot is created and resolvable.
	h := approveDir(t, "s", src)
	snap, ok := SnapshotPath("s", h)
	if !ok {
		t.Fatal("snapshot should exist after Approve with a dir")
	}
	got, err := os.ReadFile(filepath.Join(snap, "code.txt"))
	if err != nil || string(got) != "ORIGINAL" {
		t.Fatalf("snapshot content = %q, err=%v; want ORIGINAL", got, err)
	}

	// Editing the SOURCE after approval must not change the snapshot.
	if err := os.WriteFile(filepath.Join(src, "code.txt"), []byte("TAMPERED"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(snap, "code.txt"))
	if string(got) != "ORIGINAL" {
		t.Errorf("snapshot changed with the source: %q (must be immutable)", got)
	}

	// Revoke removes the snapshot.
	if _, err := Revoke("s", h); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok := SnapshotPath("s", h); ok {
		t.Error("snapshot should be gone after Revoke")
	}
}

func TestSnapshotDoesNotBakeEscapingSymlink(t *testing.T) {
	isolate(t)
	// A host secret OUTSIDE the skill tree.
	outside := t.TempDir()
	secret := filepath.Join(outside, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE-KEY"), 0o600); err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	// (1) an escaping symlink an agent might plant; (2) a legit in-tree
	// relative symlink (e.g. node_modules/.bin style).
	if err := os.Symlink(secret, filepath.Join(src, "evil")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "real.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(src, "alias")); err != nil {
		t.Fatal(err)
	}

	h := bundleHash(t, src)
	snap, err := snapshot("s", h, src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// The escaping link must NOT appear as baked content (no secret in snapshot).
	if b, err := os.ReadFile(filepath.Join(snap, "evil")); err == nil {
		t.Fatalf("escaping symlink was materialized into the snapshot: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(snap, "evil")); err == nil {
		t.Error("escaping symlink should be dropped entirely, not recreated")
	}
	// The in-tree relative link is preserved and resolves within the snapshot.
	if b, err := os.ReadFile(filepath.Join(snap, "alias")); err != nil || string(b) != "ok" {
		t.Errorf("in-tree symlink not preserved: body=%q err=%v", b, err)
	}
}
