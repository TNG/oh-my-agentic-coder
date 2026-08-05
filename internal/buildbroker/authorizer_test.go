package buildbroker

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStartAuthorizer_AcceptsSessionWorktree asserts the start
// authorizer accepts its canonical session worktree (including via a
// symlink that resolves to it).
func TestStartAuthorizer_AcceptsSessionWorktree(t *testing.T) {
	tmp := t.TempDir()
	canon, _ := filepath.EvalSymlinks(tmp)
	authz := StartAuthorizer(canon)
	got, err := authz(tmp)
	if err != nil {
		t.Fatalf("direct: %v", err)
	}
	if got != canon {
		t.Errorf("direct: got %q, want %q", got, canon)
	}
	// A symlink that resolves to the session worktree is accepted.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(tmp, link); err != nil {
		t.Fatal(err)
	}
	got, err = authz(link)
	if err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if got != canon {
		t.Errorf("symlink: got %q, want %q", got, canon)
	}
}

// TestStartAuthorizer_RejectsOtherWorktree asserts the start
// authorizer rejects a worktree that is not the session worktree.
func TestStartAuthorizer_RejectsOtherWorktree(t *testing.T) {
	session := t.TempDir()
	canon, _ := filepath.EvalSymlinks(session)
	authz := StartAuthorizer(canon)
	other := t.TempDir()
	if _, err := authz(other); err != ErrUnauthorized {
		t.Errorf("other: err = %v, want ErrUnauthorized", err)
	}
}

// TestStartAuthorizer_RejectsNonexistent asserts a non-existent path
// is rejected (canonicalization fails).
func TestStartAuthorizer_RejectsNonexistent(t *testing.T) {
	session := t.TempDir()
	canon, _ := filepath.EvalSymlinks(session)
	authz := StartAuthorizer(canon)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := authz(missing); err != ErrUnauthorized {
		t.Errorf("nonexistent: err = %v, want ErrUnauthorized", err)
	}
}

// TestServeAuthorizer_AcceptsActiveDirUnderRoot asserts the serve
// authorizer accepts an active directory under a configured root.
func TestServeAuthorizer_AcceptsActiveDirUnderRoot(t *testing.T) {
	root := t.TempDir()
	canonRoot, _ := filepath.EvalSymlinks(root)
	active := filepath.Join(canonRoot, "project")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := NewServeActiveDirs()
	dirs.Add(active)
	authz := ServeAuthorizer([]string{canonRoot}, dirs.IsActive)
	got, err := authz(active)
	if err != nil {
		t.Fatalf("active under root: %v", err)
	}
	if got != active {
		t.Errorf("got %q, want %q", got, active)
	}
}

// TestServeAuthorizer_RejectsInactiveDir asserts a directory that is
// not active is rejected.
func TestServeAuthorizer_RejectsInactiveDir(t *testing.T) {
	root := t.TempDir()
	canonRoot, _ := filepath.EvalSymlinks(root)
	inactive := filepath.Join(canonRoot, "inactive")
	if err := os.MkdirAll(inactive, 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := NewServeActiveDirs()
	authz := ServeAuthorizer([]string{canonRoot}, dirs.IsActive)
	if _, err := authz(inactive); err != ErrUnauthorized {
		t.Errorf("inactive: err = %v, want ErrUnauthorized", err)
	}
}

// TestServeAuthorizer_RejectsDirOutsideRoot asserts a directory
// outside the configured roots is rejected even when active.
func TestServeAuthorizer_RejectsDirOutsideRoot(t *testing.T) {
	root := t.TempDir()
	canonRoot, _ := filepath.EvalSymlinks(root)
	outside := t.TempDir()
	canonOutside, _ := filepath.EvalSymlinks(outside)
	dirs := NewServeActiveDirs()
	dirs.Add(canonOutside)
	authz := ServeAuthorizer([]string{canonRoot}, dirs.IsActive)
	if _, err := authz(canonOutside); err != ErrUnauthorized {
		t.Errorf("outside root: err = %v, want ErrUnauthorized", err)
	}
}

// TestServeAuthorizer_EmptyRootsAllowsAnyActive asserts an empty
// roots list allows any active directory (the serve default).
func TestServeAuthorizer_EmptyRootsAllowsAnyActive(t *testing.T) {
	active := t.TempDir()
	canon, _ := filepath.EvalSymlinks(active)
	dirs := NewServeActiveDirs()
	dirs.Add(canon)
	authz := ServeAuthorizer(nil, dirs.IsActive)
	got, err := authz(active)
	if err != nil {
		t.Fatalf("empty roots: %v", err)
	}
	if got != canon {
		t.Errorf("got %q, want %q", got, canon)
	}
}

// TestServeAuthorizer_RejectsSymlinkTraversal asserts a symlink that
// escapes the configured root is rejected.
func TestServeAuthorizer_RejectsSymlinkTraversal(t *testing.T) {
	root := t.TempDir()
	canonRoot, _ := filepath.EvalSymlinks(root)
	escape := t.TempDir()
	canonEscape, _ := filepath.EvalSymlinks(escape)
	// Plant a symlink inside root that points outside.
	link := filepath.Join(canonRoot, "escape-link")
	if err := os.Symlink(canonEscape, link); err != nil {
		t.Fatal(err)
	}
	// The symlink target (canonEscape) is added to active dirs.
	dirs := NewServeActiveDirs()
	dirs.Add(canonEscape)
	authz := ServeAuthorizer([]string{canonRoot}, dirs.IsActive)
	// The client sends the symlink path inside root, but it
	// canonicalizes to canonEscape which is NOT under canonRoot.
	if _, err := authz(link); err != ErrUnauthorized {
		t.Errorf("symlink traversal: err = %v, want ErrUnauthorized", err)
	}
}

// TestServeActiveDirs_AddRemoveIsActive asserts the active-dirs set
// is thread-safe and Add/Remove/IsActive behave.
func TestServeActiveDirs_AddRemoveIsActive(t *testing.T) {
	d := NewServeActiveDirs()
	d.Add("/a")
	d.Add("/b")
	if !d.IsActive("/a") || !d.IsActive("/b") {
		t.Errorf("Add/IsActive failed")
	}
	if d.IsActive("/c") {
		t.Errorf("/c should not be active")
	}
	d.Remove("/a")
	if d.IsActive("/a") {
		t.Errorf("/a should be removed")
	}
	got := d.List()
	if len(got) != 1 || got[0] != "/b" {
		t.Errorf("List = %v, want [/b]", got)
	}
}
