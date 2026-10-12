package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestWatchNewProtected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &sandboxprofile.Profile{}
	p.Workdir.Access = sandboxprofile.AccessReadWrite

	stop := make(chan struct{})
	defer close(stop)
	got := make(chan string, 8)
	go func() {
		if err := WatchNewProtected(p, dir, 10*time.Millisecond, func(path string) { got <- path }, stop); err != nil {
			t.Errorf("WatchNewProtected: %v", err)
		}
	}()

	// The launch-time .env is masked.
	select {
	case path := <-got:
		t.Fatalf("alerted for launch-time file: %s", path)
	case <-time.After(80 * time.Millisecond):
	}

	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case path := <-got:
		if want := filepath.Join(dir, "sub", ".env"); path != want {
			t.Fatalf("alerted %s, want %s", path, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no alert for mid-session .env")
	}

	select {
	case path := <-got:
		t.Fatalf("duplicate alert: %s", path)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestWatchNewProtectedOverrideDeny(t *testing.T) {
	dir := t.TempDir()
	p := &sandboxprofile.Profile{}
	p.Workdir.Access = sandboxprofile.AccessReadWrite
	p.Filesystem.OverrideDeny = []string{".env"}

	stop := make(chan struct{})
	defer close(stop)
	got := make(chan string, 8)
	go func() {
		if err := WatchNewProtected(p, dir, 10*time.Millisecond, func(path string) { got <- path }, stop); err != nil {
			t.Errorf("WatchNewProtected: %v", err)
		}
	}()

	// A first tick fired means the launch-time snapshot is done; only
	// files created afterwards count as mid-session.
	select {
	case path := <-got:
		t.Fatalf("alerted on empty dir: %s", path)
	case <-time.After(80 * time.Millisecond):
	}

	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".envrc"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	select {
	case path := <-got:
		if want := filepath.Join(dir, ".envrc"); path != want {
			t.Fatalf("alerted %s, want only %s (the .env override must hold)", path, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no alert for mid-session .envrc")
	}
	select {
	case path := <-got:
		t.Fatalf("unexpected extra alert: %s", path)
	case <-time.After(80 * time.Millisecond):
	}
}

// TestWatchNewProtectedSkipsIncompleteRoot checks that the mid-session
// watch leaves out a root the launch scan could not finish: re-walking it
// each tick is costly, and where the cap falls shifts as files change.
// A .env early in that root (inside the cap) must not alert; one in a
// normal root still must.
func TestWatchNewProtectedSkipsIncompleteRoot(t *testing.T) {
	big, small := t.TempDir(), t.TempDir()
	populateLargeTree(t, big)

	p := &sandboxprofile.Profile{}
	p.Workdir.Access = sandboxprofile.AccessReadWrite
	p.Filesystem.Read = []string{small}

	stop := make(chan struct{})
	defer close(stop)
	got := make(chan string, 64)
	go func() {
		if err := WatchNewProtected(p, big, 10*time.Millisecond, func(path string) { got <- path }, stop); err != nil {
			t.Errorf("WatchNewProtected: %v", err)
		}
	}()

	writeEnv := func(dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(dir, ".env")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	// The launch snapshot of the big root takes a while. Probe the small
	// root until an alert shows the ticks are running.
	deadline := time.After(30 * time.Second)
	for i := 0; ; i++ {
		writeEnv(filepath.Join(small, fmt.Sprintf("probe%d", i)))
		select {
		case <-got:
		case <-time.After(200 * time.Millisecond):
			continue
		case <-deadline:
			t.Fatal("watch never alerted for the small root")
		}
		break
	}

	early := writeEnv(filepath.Join(big, "aaa"))
	late := writeEnv(filepath.Join(small, "late"))
	seenLate := false
	timeout := time.After(5 * time.Second)
	for {
		select {
		case path := <-got:
			if path == early {
				t.Fatalf("alerted for %s in a root the launch scan could not finish", path)
			}
			if path == late {
				seenLate = true
				timeout = time.After(300 * time.Millisecond) // give a wrong alert time to show up
			}
			continue
		case <-timeout:
		}
		break
	}
	if !seenLate {
		t.Fatalf("no alert for %s in a normal root", late)
	}
}
