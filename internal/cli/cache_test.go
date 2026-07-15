package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

// stageActiveScope prepares a workdir cache scope, leaves it open
// (so its shared lock is held), and returns the marker file path and
// a closer the test must defer.
func stageActiveScope(t *testing.T, workdir string) (marker string) {
	t.Helper()
	scope, err := toolcache.PreparePersistent(toolcache.DomainWorkdir, workdir)
	if err != nil {
		t.Fatalf("PreparePersistent: %v", err)
	}
	t.Cleanup(func() { _ = scope.Close() })
	marker = filepath.Join(scope.Dir, "active")
	if err := os.WriteFile(marker, []byte("active"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	return marker
}

// stageInactiveScope prepares a workdir cache scope, writes a marker,
// and closes it so the scope is removable.
func stageInactiveScope(t *testing.T, workdir string) (marker, dir string) {
	t.Helper()
	scope, err := toolcache.PreparePersistent(toolcache.DomainWorkdir, workdir)
	if err != nil {
		t.Fatalf("PreparePersistent: %v", err)
	}
	marker = filepath.Join(scope.Dir, "cache-data")
	if err := os.WriteFile(marker, []byte("cache"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	dir = scope.Dir
	if err := scope.Close(); err != nil {
		t.Fatalf("close scope: %v", err)
	}
	return marker, dir
}

// TestRunCacheClearCurrent: clearing an inactive workdir scope reports
// "removed", returns ExitOK, and actually deletes the scope directory.
func TestRunCacheClearCurrent(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	marker, dir := stageInactiveScope(t, wd)

	env, read := captureEnv(t, wd)
	code := runCache([]string{"clear"}, env)
	if code != ExitOK {
		out, errOut := read()
		t.Fatalf("code = %d; stdout=%q stderr=%q", code, out, errOut)
	}
	out, _ := read()
	if !strings.Contains(out, "removed") {
		t.Errorf("expected 'removed' in output; got %q", out)
	}
	if !strings.Contains(out, dir) {
		t.Errorf("expected scope path %q in output; got %q", dir, out)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Errorf("scope marker still exists; stat err = %v", err)
	}
}

// TestRunCacheClearCurrentActive: clearing an active workdir scope reports
// "active", returns ExitOK, and leaves the scope intact.
func TestRunCacheClearCurrentActive(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	marker := stageActiveScope(t, wd)

	env, read := captureEnv(t, wd)
	code := runCache([]string{"clear"}, env)
	if code != ExitOK {
		out, errOut := read()
		t.Fatalf("code = %d; stdout=%q stderr=%q", code, out, errOut)
	}
	out, _ := read()
	if !strings.Contains(out, "active") {
		t.Errorf("expected 'active' in output; got %q", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("active scope data was removed: %v", err)
	}
}

// TestRunCacheClearAllReportsRemovedAndSkipped: `--all` walks every
// digest-scoped entry and renders each status. Removed entries vanish,
// active and skipped entries survive. Mirrors the unsafe-entry staging
// in internal/toolcache/clear_test.go (TestClearAllSkipsActiveAndUnsafeEntries):
// a 64-hex-char-named symlink pointing outside the cache root is
// reported as skipped and its target is left intact.
func TestRunCacheClearAllReportsRemovedAndSkipped(t *testing.T) {
	isolateHome(t)
	inactiveWd := t.TempDir()
	activeWd := t.TempDir()
	_, inactiveDir := stageInactiveScope(t, inactiveWd)
	activeMarker := stageActiveScope(t, activeWd)

	// Stage an unsafe digest-named entry: a symlink at the cache root
	// pointing outside it. ClearAll treats it as a scope but rejects it
	// as unsafe (not a directory), reporting "skipped".
	cacheRoot := filepath.Dir(inactiveDir)
	outside := t.TempDir()
	outsideMarker := filepath.Join(outside, "outside")
	if err := os.WriteFile(outsideMarker, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside marker: %v", err)
	}
	unsafePath := filepath.Join(cacheRoot, strings.Repeat("b", 64))
	if err := os.Symlink(outside, unsafePath); err != nil {
		t.Fatalf("symlink unsafe entry: %v", err)
	}

	env, read := captureEnv(t, inactiveWd)
	code := runCache([]string{"clear", "--all"}, env)
	if code != ExitOK {
		out, errOut := read()
		t.Fatalf("code = %d; stdout=%q stderr=%q", code, out, errOut)
	}
	out, _ := read()
	if !strings.Contains(out, "removed") {
		t.Errorf("expected 'removed' status; got %q", out)
	}
	if !strings.Contains(out, "active") {
		t.Errorf("expected 'active' status; got %q", out)
	}
	if !strings.Contains(out, "skipped") {
		t.Errorf("expected 'skipped' status; got %q", out)
	}
	if !strings.Contains(out, inactiveDir) {
		t.Errorf("expected inactive scope path in output; got %q", out)
	}
	if !strings.Contains(out, unsafePath) {
		t.Errorf("expected unsafe scope path %q in output; got %q", unsafePath, out)
	}
	if _, err := os.Lstat(inactiveDir); !os.IsNotExist(err) {
		t.Errorf("inactive scope dir should be removed; stat err = %v", err)
	}
	if _, err := os.Stat(activeMarker); err != nil {
		t.Errorf("active scope marker must survive --all: %v", err)
	}
	if _, err := os.Stat(outsideMarker); err != nil {
		t.Errorf("unsafe symlink target must survive --all: %v", err)
	}
}

// TestRunCacheClearRejectsUnknownArguments: unknown verbs and unknown
// flags must produce ExitMisuse, not run a clear.
func TestRunCacheClearRejectsUnknownArguments(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()

	cases := [][]string{
		{"purge"},           // unknown verb
		{"clear", "--nope"}, // unknown flag
		{},                  // no verb at all
	}
	for _, args := range cases {
		env, _ := captureEnv(t, wd)
		code := runCache(args, env)
		if code != ExitMisuse {
			t.Errorf("runCache(%v) = %d; want %d (ExitMisuse)", args, code, ExitMisuse)
		}
	}
}

// TestCacheCommandRegistered: the `cache` subcommand must be wired into
// the top-level command table and dispatch to runCache.
func TestCacheCommandRegistered(t *testing.T) {
	cmd, ok := commands()["cache"]
	if !ok {
		t.Fatal("commands() missing 'cache' entry")
	}
	if cmd.Name != "cache" {
		t.Errorf("cmd.Name = %q; want \"cache\"", cmd.Name)
	}
	if cmd.Run == nil {
		t.Error("cmd.Run is nil")
	}
	// Dispatch through the table and confirm it reaches runCache for
	// the clear verb (returning misuse for a bogus verb is enough proof
	// the Run func was actually wired, not a nil deref).
	env, _ := captureEnv(t, t.TempDir())
	if code := cmd.Run([]string{"bogus"}, env); code != ExitMisuse {
		t.Errorf("cache bogus verb = %d; want %d", code, ExitMisuse)
	}
}
