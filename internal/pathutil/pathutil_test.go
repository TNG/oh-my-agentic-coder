package pathutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnderBoundary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	if !Under(root, filepath.Join(root, "a", ".env")) {
		t.Fatal("a path below the root must match")
	}
	if !Under(root, root) {
		t.Fatal("the root itself must match")
	}
	if Under(root, root+"-other") {
		t.Fatal("a sibling with the same prefix must not match")
	}
	if Under(root, filepath.Dir(root)) {
		t.Fatal("the parent must not match")
	}
}

func TestCanonicalUnresolvableTailKeepsAbsolutePath(t *testing.T) {
	base := Canonical(t.TempDir())
	if got, want := Canonical(filepath.Join(base, "missing", "file")), filepath.Join(base, "missing", "file"); got != want {
		t.Fatalf("Canonical = %q, want %q", got, want)
	}
}

func TestCanonicalResolvesSymlink(t *testing.T) {
	base := Canonical(t.TempDir())
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if got, want := Canonical(link), real; got != want {
		t.Fatalf("Canonical(link) = %q, want %q", got, want)
	}
	if got, want := Canonical(filepath.Join(link, "missing")), filepath.Join(real, "missing"); got != want {
		t.Fatalf("Canonical(link/missing) = %q, want %q", got, want)
	}
}

func TestCanonicalEmpty(t *testing.T) {
	if got := Canonical(""); got != "" {
		t.Fatalf("Canonical(\"\") = %q, want empty", got)
	}
}

func TestExistingParentsWithin(t *testing.T) {
	root := Canonical(t.TempDir())
	parent := filepath.Join(root, "parent")
	deep := filepath.Join(parent, "deep")
	if err := os.MkdirAll(deep, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := ExistingParentsWithin(root, []string{filepath.Join(deep, ".env"), filepath.Join(parent, "another"), filepath.Join(root+"-other", "not-covered"), filepath.Join(root, "missing", "secret")})
	if err != nil || len(got) != 2 || got[0] != parent || got[1] != deep {
		t.Fatalf("parents=%v error=%v", got, err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := ExistingParentsWithin(root, []string{filepath.Join(alias, ".env")}); err == nil {
		t.Fatal("accepted symlink parent")
	}
}
