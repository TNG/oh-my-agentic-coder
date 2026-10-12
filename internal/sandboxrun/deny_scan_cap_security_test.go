package sandboxrun

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestSecurityDenyScanCapWarnsAndKeepsMatches covers a granted tree too
// large for the launch-time protection scan. Launch goes ahead, but the
// tree is reported as incomplete so the user is warned, and a .env found
// before the scan stopped is still hidden.
func TestSecurityDenyScanCapWarnsAndKeepsMatches(t *testing.T) {
	workdir := t.TempDir()

	// "aaa-protected" sorts before the padding tree, so the scan reaches
	// it before the cap; "zzz-protected" sorts after and is never visited.
	early := filepath.Join(workdir, "aaa-protected", ".env")
	late := filepath.Join(workdir, "zzz-protected", ".env")
	for _, f := range []string{early, late} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	populateLargeTree(t, workdir)

	prof := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(prof, workdir, nil)
	if err != nil {
		t.Fatalf("an incomplete scan must warn, not refuse launch: %v", err)
	}
	if !slices.Equal(g.IncompleteDenyScanRoots, []string{workdir}) {
		t.Errorf("IncompleteDenyScanRoots = %v, want [%s]", g.IncompleteDenyScanRoots, workdir)
	}
	if !slices.Contains(g.ProtectedPaths, early) {
		t.Errorf("match found before the cap must still be protected: %s", early)
	}

	var stderr bytes.Buffer
	warnIncompleteDenyScan(&stderr, g.IncompleteDenyScanRoots)
	if !strings.Contains(stderr.String(), "warning") || !strings.Contains(stderr.String(), workdir) {
		t.Errorf("warning must name %s, got:\n%s", workdir, stderr.String())
	}
}

// TestDenyScanCapNamesEveryIncompleteFolder checks that when several
// granted folders are too large to scan, all of them are reported, not
// just the first.
func TestDenyScanCapNamesEveryIncompleteFolder(t *testing.T) {
	big1, big2 := t.TempDir(), t.TempDir()
	populateLargeTree(t, big1)
	populateLargeTree(t, big2)

	prof := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Read: []string{big1, big2}},
	}
	g, err := ResolveGrants(prof, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.IncompleteDenyScanRoots, []string{big1, big2}) {
		t.Errorf("IncompleteDenyScanRoots = %v, want both %s and %s", g.IncompleteDenyScanRoots, big1, big2)
	}
}

// populateLargeTree fills root with more entries than the protection scan
// can enumerate, so the scan is guaranteed to be incomplete. It is laid out
// as many small directories whose names sort between "aaa-*" and "zzz-*",
// so a "zzz-*" target lies past the enumeration bound. Directory reads are kept
// cheap by spreading entries across many dirs.
func populateLargeTree(t *testing.T, root string) {
	t.Helper()
	const dirs = 400
	const perDir = 505

	workers := 16
	jobs := make(chan int, workers*2)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				dir := filepath.Join(root, fmt.Sprintf("g%04d", d))
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Errorf("mkdir %s: %v", dir, err)
					return
				}
				for f := 0; f < perDir; f++ {
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("n%05d", f)), nil, 0o644); err != nil {
						t.Errorf("write %s: %v", dir, err)
						return
					}
				}
			}
		}()
	}
	for d := 0; d < dirs; d++ {
		jobs <- d
	}
	close(jobs)
	wg.Wait()
}
