package skilltrust

import (
	"os"
	"path/filepath"
	"testing"
)

// isolate points HOME and XDG_CONFIG_HOME at temp dirs so the approvals
// store resolves under a throwaway location (registry.GlobalDir honors
// XDG_CONFIG_HOME).
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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
	if err := Approve("skill", "sha256:abc", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !Exists() {
		t.Error("store should exist after Approve")
	}
	ok, _ := IsApproved("skill", "sha256:abc")
	if !ok {
		t.Error("approved (name, hash) should be approved")
	}
	// A different hash for the same name is NOT approved (content-keyed).
	if ok, _ := IsApproved("skill", "sha256:different"); ok {
		t.Error("a different bundle hash must not be approved")
	}
	// A different name is not approved.
	if ok, _ := IsApproved("other", "sha256:abc"); ok {
		t.Error("a different name must not be approved")
	}
}

func TestApproveIsAdditivePerName(t *testing.T) {
	isolate(t)
	// The same name may be registered under multiple harnesses / workdirs,
	// each with its own content, so approvals must accumulate — not clobber.
	_ = Approve("skill", "sha256:v1", "")
	_ = Approve("skill", "sha256:v2", "")

	for _, h := range []string{"sha256:v1", "sha256:v2"} {
		if ok, _ := IsApproved("skill", h); !ok {
			t.Errorf("hash %s should remain approved (additive per name)", h)
		}
	}
	// Re-approving an identical (name, hash) is idempotent (no duplicate).
	_ = Approve("skill", "sha256:v1", "")
	s, _ := Load()
	if len(s.Approved) != 2 {
		t.Errorf("expected 2 approvals, got %d", len(s.Approved))
	}
}

func TestRevokeIsScopedToHash(t *testing.T) {
	isolate(t)
	_ = Approve("foo", "sha256:opencode", "") // same name, two harnesses
	_ = Approve("foo", "sha256:claude", "")
	_ = Approve("bar", "sha256:2", "")

	removed, err := Revoke("foo", "sha256:opencode")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !removed {
		t.Error("Revoke should report removal")
	}
	if ok, _ := IsApproved("foo", "sha256:opencode"); ok {
		t.Error("the revoked (name, hash) should no longer be approved")
	}
	// A same-name copy under a different hash keeps its approval.
	if ok, _ := IsApproved("foo", "sha256:claude"); !ok {
		t.Error("Revoke must not touch a same-name copy with a different hash")
	}
	if ok, _ := IsApproved("bar", "sha256:2"); !ok {
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
	_ = Approve("s", "h", "")
	if err := EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized (2nd): %v", err)
	}
	if ok, _ := IsApproved("s", "h"); !ok {
		t.Error("EnsureInitialized must not clobber existing approvals")
	}
}

func TestApprovalsSurviveReload(t *testing.T) {
	isolate(t)
	if err := Approve("skill", "sha256:abc", ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	// A fresh Load (new process would do the same) sees the persisted state.
	s, err := Load()
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
	if err := Approve("x", "h", ""); err != ErrNoGlobalDir {
		t.Errorf("Approve without a config dir = %v, want ErrNoGlobalDir", err)
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
	if err := Approve("s", "sha256:h", src); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	snap, ok := SnapshotPath("s", "sha256:h")
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
	if _, err := Revoke("s", "sha256:h"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, ok := SnapshotPath("s", "sha256:h"); ok {
		t.Error("snapshot should be gone after Revoke")
	}
}
