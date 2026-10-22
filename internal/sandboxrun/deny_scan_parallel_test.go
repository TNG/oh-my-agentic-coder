package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// walkOneDenyRootSequential is the walk as it was before folders were
// read in parallel. TestWalkOneDenyRootMatchesSequentialWalk checks the
// parallel walk against it.
func walkOneDenyRootSequential(root string, globs []string, protected map[string]bool) (matches []string, hitCap bool) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolved = root
	}
	rootSep := resolved + string(filepath.Separator)
	count := int64(0)
	_ = filepath.WalkDir(resolved, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		count++
		if count > denyScanLimit.Load() {
			hitCap = true
			return filepath.SkipAll
		}
		if path == resolved {
			return nil
		}
		if d.IsDir() && protected[path] {
			return filepath.SkipDir
		}
		for _, g := range globs {
			if ok, _ := filepath.Match(g, d.Name()); ok {
				matches = append(matches, filepath.Join(root, strings.TrimPrefix(path, rootSep)))
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if d.IsDir() && denyScanSkipDirNames[d.Name()] {
			return filepath.SkipDir
		}
		return nil
	})
	slices.Sort(matches)
	return matches, hitCap
}

// TestWalkOneDenyRootMatchesSequentialWalk builds a tree with every case
// the walk treats specially and checks that reading folders in parallel
// finds exactly what the sequential walk found: nested matches, a matched
// folder (masked whole, not descended), skipped dependency folders, an
// already-protected folder, a symlink to a folder (not followed), a
// symlink named .env (matched), an unreadable folder, and a root that is
// itself a symlink.
func TestWalkOneDenyRootMatchesSequentialWalk(t *testing.T) {
	real := t.TempDir()
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(real, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 30 {
		for j := range 5 {
			write(fmt.Sprintf("a%02d/b%d/file", i, j))
		}
		write(fmt.Sprintf("a%02d/b%d/c/.env", i, i%5))
	}
	write(".env")
	write("deep/1/2/3/4/5/6/7/8/.envrc")
	write("matched/.env/inside/.env") // folder named .env: only it is reported
	write("node_modules/pkg/.env")    // skipped dependency folder
	write("protected/.env")           // already denied by an ancestor rule
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(real, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, ".env"), filepath.Join(real, "a00", ".env")); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(real, "locked")
	write("locked/.env")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	root := filepath.Join(t.TempDir(), "link-to-root")
	if err := os.Symlink(real, root); err != nil {
		t.Fatal(err)
	}

	globs := []string{".env", ".envrc"}
	protected := map[string]bool{filepath.Join(real, "protected"): true}
	want, wantCap := walkOneDenyRootSequential(root, globs, protected)
	if len(want) < 33 || wantCap {
		t.Fatalf("test setup: sequential walk found %d matches (cap %v)", len(want), wantCap)
	}
	for range 20 { // repeat: the parallel walk's order varies run to run
		got, hitCap := walkOneDenyRoot(root, globs, protected, make(chan struct{}, denyScanConcurrency), nil, denyScanConcurrency)
		slices.Sort(got)
		if hitCap || !slices.Equal(got, want) {
			t.Fatalf("parallel walk differs from the sequential walk:\n got  %v (cap %v)\n want %v", got, hitCap, want)
		}
	}
}
