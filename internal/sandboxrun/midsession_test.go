package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func launchScan(t *testing.T, p *sandboxprofile.Profile, workdir string) *ProtectedScan {
	t.Helper()
	scan, err := ScanProtected(p, workdir, "", nil)
	if err != nil {
		t.Fatalf("ScanProtected: %v", err)
	}
	return scan
}

func TestWatchNewProtected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &sandboxprofile.Profile{}
	p.Workdir.Access = sandboxprofile.AccessReadWrite

	stop := make(chan struct{})
	defer close(stop)
	got := make(chan []string, 8)
	go func() {
		if err := WatchNewProtected(launchScan(t, p, dir), p, 10*time.Millisecond, func(paths []string) { got <- paths }, stop, WatchTuning{}); err != nil {
			t.Errorf("WatchNewProtected: %v", err)
		}
	}()

	// The launch-time .env is masked.
	select {
	case paths := <-got:
		t.Fatalf("alerted for launch-time file: %v", paths)
	case <-time.After(80 * time.Millisecond):
	}

	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case paths := <-got:
		if len(paths) != 1 || paths[0] != filepath.Join(dir, "sub", ".env") {
			t.Fatalf("alerted %v, want only %s", paths, filepath.Join(dir, "sub", ".env"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no alert for mid-session .env")
	}

	select {
	case paths := <-got:
		t.Fatalf("duplicate alert: %v", paths)
	case <-time.After(80 * time.Millisecond):
	}
}

// TestWatchNewProtectedFullPassOff covers the default full_pass_ticks: 0:
// the incremental ticks and the per-directory re-check must still detect
// ordinary mid-session files.
func TestWatchNewProtectedFullPassOff(t *testing.T) {
	dir := t.TempDir()
	p := &sandboxprofile.Profile{}
	p.Workdir.Access = sandboxprofile.AccessReadWrite

	stop := make(chan struct{})
	defer close(stop)
	got := make(chan []string, 8)
	go func() {
		if err := WatchNewProtected(launchScan(t, p, dir), p, 10*time.Millisecond, func(paths []string) { got <- paths }, stop, WatchTuning{}); err != nil {
			t.Errorf("WatchNewProtected: %v", err)
		}
	}()

	// First tick = snapshot done.
	select {
	case paths := <-got:
		t.Fatalf("alerted on empty dir: %v", paths)
	case <-time.After(80 * time.Millisecond):
	}

	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case paths := <-got:
		if len(paths) != 1 || paths[0] != filepath.Join(dir, ".env") {
			t.Fatalf("alerted %v, want only %s", paths, filepath.Join(dir, ".env"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no alert for mid-session .env with the full pass off (default)")
	}
}

// TestFullPassDue covers the opt-in full-pass cadence, including the
// positive path the old constant made untestable: 0 is off, N fires on
// every Nth tick.
func TestFullPassDue(t *testing.T) {
	cases := []struct {
		name    string
		ticks   int
		cadence int
		want    bool
	}{
		{"off", 6, 0, false},
		{"before", 5, 6, false},
		{"due", 6, 6, true},
		{"later", 12, 6, true},
		{"every", 1, 1, true},
		{"first", 0, 6, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (resolvedTuning{fullPassTicks: tc.cadence}).fullPassDue(tc.ticks); got != tc.want {
				t.Errorf("fullPassDue(ticks=%d, cadence=%d) = %v, want %v", tc.ticks, tc.cadence, got, tc.want)
			}
		})
	}
}

func TestWatchNewProtectedOverrideDeny(t *testing.T) {
	dir := t.TempDir()
	p := &sandboxprofile.Profile{}
	p.Workdir.Access = sandboxprofile.AccessReadWrite
	p.Filesystem.OverrideDeny = []string{".env"}

	stop := make(chan struct{})
	defer close(stop)
	got := make(chan []string, 8)
	go func() {
		if err := WatchNewProtected(launchScan(t, p, dir), p, 10*time.Millisecond, func(paths []string) { got <- paths }, stop, WatchTuning{}); err != nil {
			t.Errorf("WatchNewProtected: %v", err)
		}
	}()

	// A first tick fired means the launch-time snapshot is done; only
	// files created afterwards count as mid-session.
	select {
	case paths := <-got:
		t.Fatalf("alerted on empty dir: %v", paths)
	case <-time.After(80 * time.Millisecond):
	}

	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".envrc"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case paths := <-got:
		if len(paths) != 1 || paths[0] != filepath.Join(dir, ".envrc") {
			t.Fatalf("alerted %v, want only %s (the .env override must hold)", paths, filepath.Join(dir, ".envrc"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no alert for mid-session .envrc")
	}
	select {
	case paths := <-got:
		t.Fatalf("unexpected extra alert: %v", paths)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestWatchReportsEnvCreatedAfterScanA(t *testing.T) {
	dir := canonicalPath(t.TempDir())
	p := &sandboxprofile.Profile{}
	p.Workdir.Access = sandboxprofile.AccessReadWrite
	scan := launchScan(t, p, dir)
	env := writeEnv(t, dir)

	stop := make(chan struct{})
	defer close(stop)
	got := make(chan []string, 8)
	go func() {
		if err := WatchNewProtected(scan, p, 10*time.Millisecond, func(paths []string) { got <- paths }, stop, WatchTuning{}); err != nil {
			t.Errorf("WatchNewProtected: %v", err)
		}
	}()

	select {
	case paths := <-got:
		if !slices.Equal(paths, []string{env}) {
			t.Fatalf("first alert = %v, want only %s", paths, env)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no alert for a .env created after the launch scan")
	}
}

func TestWatchSkipsAcceptedRoots(t *testing.T) {
	setDenyByName(t, false)
	root, _ := lateEnvTree(t)
	p := profileWithReadRoots(1000, root)
	scan, err := ScanProtected(p, root, "", func([]string, int64) ScanChoice { return ScanStartIncomplete })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(scan.Accepted, []string{root}) {
		t.Fatalf("Accepted = %v, want [%s]", scan.Accepted, root)
	}
	writeEnv(t, root)

	stop := make(chan struct{})
	got := make(chan []string, 8)
	done := make(chan error, 1)
	go func() {
		done <- WatchNewProtected(scan, p, 10*time.Millisecond, func(paths []string) { got <- paths }, stop, WatchTuning{})
	}()
	select {
	case paths := <-got:
		t.Fatalf("alerted inside an accepted root: %v", paths)
	case err := <-done:
		t.Fatalf("watch stopped: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(stop)
	if err := <-done; err != nil {
		t.Errorf("WatchNewProtected: %v", err)
	}
}

func TestWatchUsesContinuedLimit(t *testing.T) {
	setDenyByName(t, false)
	root, _ := lateEnvTree(t)
	p := profileWithReadRoots(1000, root)
	var calls []recordedAsk
	scan, err := ScanProtected(p, root, "", scriptedAsk(&calls, ScanContinue, ScanContinue))
	if err != nil {
		t.Fatal(err)
	}
	if scan.Limit != 3000 {
		t.Fatalf("Limit = %d, want 3000", scan.Limit)
	}

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WatchNewProtected(scan, p, 10*time.Millisecond, func([]string) {}, stop, WatchTuning{FullPassTicks: 2})
	}()
	select {
	case err := <-done:
		t.Fatalf("watch stopped over a tree within the continued limit: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(stop)
	if err := <-done; err != nil {
		t.Errorf("WatchNewProtected: %v", err)
	}
}

func TestWatchGrowthPastLimitNamesSetting(t *testing.T) {
	setDenyByName(t, false)
	root := canonicalPath(t.TempDir())
	writeFiles(t, root, 100)
	p := profileWithReadRoots(1000, root)
	scan, err := ScanProtected(p, root, "/profiles/dev.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	defer close(stop)
	done := make(chan error, 1)
	go func() {
		done <- WatchNewProtected(scan, p, 10*time.Millisecond, func([]string) {}, stop, WatchTuning{})
	}()
	writeFiles(t, filepath.Join(root, "more"), 1500)
	select {
	case err := <-done:
		want := `raise "filesystem.deny_scan.max_entries" in /profiles/dev.yaml to keep watching it`
		if err == nil || !strings.Contains(err.Error(), "during the session") || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want the session-growth error ending in %q", err, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch kept running after the folder outgrew the limit")
	}
}
