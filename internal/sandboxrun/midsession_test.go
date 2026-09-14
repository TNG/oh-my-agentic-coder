package sandboxrun

import (
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
