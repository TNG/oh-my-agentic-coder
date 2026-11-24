package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func writeFiles(t *testing.T, dir string, n int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func profileWithScan(workdir string, scan *sandboxprofile.DenyScan) *sandboxprofile.Profile {
	return &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{DenyScan: scan},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
}

func defaultScanParams() denyScanParams {
	return denyScanParamsFor(&sandboxprofile.Profile{})
}

func int64Ptr(n int64) *int64 { return &n }

func TestDenyScanLimitFromProfile(t *testing.T) {
	setDenyByName(t, false)
	workdir := canonicalPath(t.TempDir())
	for i := range 3 {
		writeFiles(t, filepath.Join(workdir, fmt.Sprintf("a%d", i)), 1000)
	}
	env := writeEnv(t, filepath.Join(workdir, "zzz"))

	_, err := ResolveGrants(profileWithScan(workdir, &sandboxprofile.DenyScan{MaxEntries: int64Ptr(1000)}), workdir, nil)
	if err == nil || !strings.Contains(err.Error(), "1000-entry limit") {
		t.Fatalf("limit 1000: want the 1000-entry limit error, got %v", err)
	}

	g, err := ResolveGrants(profileWithScan(workdir, &sandboxprofile.DenyScan{MaxEntries: int64Ptr(5000)}), workdir, nil)
	if err != nil {
		t.Fatalf("limit 5000: %v", err)
	}
	if !slices.Contains(g.ProtectedPaths, env) {
		t.Errorf("%s not protected with limit 5000: %v", env, g.ProtectedPaths)
	}
}

func TestDenyScanSkipDirNamesAnyDepth(t *testing.T) {
	setDenyByName(t, false)
	workdir := canonicalPath(t.TempDir())
	skipped := filepath.Join(workdir, "a", "b", "build-gradle")
	writeFiles(t, skipped, 2000)
	inSkipped := writeEnv(t, filepath.Join(skipped, "x"))
	shallow := writeEnv(t, filepath.Join(workdir, "a"))

	limit := &sandboxprofile.DenyScan{MaxEntries: int64Ptr(1000)}
	if _, err := ResolveGrants(profileWithScan(workdir, limit), workdir, nil); err == nil {
		t.Fatal("control: without skip_dir_names the 2000-file folder must exceed the limit")
	}

	scan := &sandboxprofile.DenyScan{MaxEntries: int64Ptr(1000), SkipDirNames: []string{"build-gradle"}}
	g, err := ResolveGrants(profileWithScan(workdir, scan), workdir, nil)
	if err != nil {
		t.Fatalf("skip_dir_names must keep the scan under the limit: %v", err)
	}
	if !slices.Contains(g.ProtectedPaths, shallow) {
		t.Errorf("%s not protected", shallow)
	}
	if slices.Contains(g.ProtectedPaths, inSkipped) {
		t.Errorf("%s below a skipped folder must not be scanned", inSkipped)
	}
}

func TestDenyScanEntriesCounted(t *testing.T) {
	root := canonicalPath(t.TempDir())
	writeFiles(t, root, 249)

	got := walkGlobMatches([]string{root}, []string{".env"}, nil, denyScanParams{limit: 1000}, nil, denyScanConcurrency)
	if len(got) != 1 || got[0].root != root {
		t.Fatalf("walkGlobMatches = %+v", got)
	}
	if got[0].entries != 250 || got[0].hitCap {
		t.Errorf("entries = %d, hitCap = %v; want 250 (root plus 249 files), false", got[0].entries, got[0].hitCap)
	}
}
