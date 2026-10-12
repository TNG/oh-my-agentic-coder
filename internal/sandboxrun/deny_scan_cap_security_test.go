package sandboxrun

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestSecurityDenyScanCapWarnsAndKeepsMatches covers a granted tree too
// large for the launch-time protection scan. Grant resolution succeeds,
// but the tree is reported as incomplete so launch can ask the user, and a
// .env found before the scan stopped is still hidden.
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
}

// TestConfirmIncompleteDenyScan covers the question asked before launch
// when a folder could not be fully scanned: it names every folder, starts
// only on "y" or --accept-incomplete-scan, and declines without a terminal.
func TestConfirmIncompleteDenyScan(t *testing.T) {
	roots := []string{"/home/me/data", "/home/me/archive"}
	cases := []struct {
		name        string
		input       string
		interactive bool
		accept      bool
		want        bool
		wantOut     string
	}{
		{name: "yes", input: "y\n", interactive: true, want: true, wantOut: "Start anyway? [y/N]"},
		{name: "default is no", input: "\n", interactive: true, want: false, wantOut: "--accept-incomplete-scan"},
		{name: "no terminal", interactive: false, want: false, wantOut: "no terminal to confirm"},
		{name: "flag skips question", interactive: true, accept: true, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			got := confirmIncompleteDenyScan(&out, strings.NewReader(c.input), c.interactive, c.accept, roots)
			if got != c.want {
				t.Errorf("confirm = %v, want %v", got, c.want)
			}
			for _, want := range []string{roots[0], roots[1], c.wantOut} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output must contain %q, got:\n%s", want, out.String())
				}
			}
			if c.accept && strings.Contains(out.String(), "Start anyway?") {
				t.Errorf("--accept-incomplete-scan must not ask, got:\n%s", out.String())
			}
		})
	}
	if !confirmIncompleteDenyScan(io.Discard, strings.NewReader(""), false, false, nil) {
		t.Error("a complete scan must start without asking")
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
