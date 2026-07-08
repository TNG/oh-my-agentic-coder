package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestBwrapMarkerFileUsedWhenDenialTextSet(t *testing.T) {
	home := t.TempDir()
	netrc := filepath.Join(home, ".netrc")
	if err := os.WriteFile(netrc, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Grants{
		Workdir:        home,
		AllowPaths:     []string{home},
		ProtectedPaths: []string{netrc},
		NetworkMode:    sandboxprofile.ModeBlocked,
		DenialText:     "X-Omac-Sandbox: denied\nprotected\n",
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "--ro-bind /dev/null "+netrc) {
		t.Errorf("still using /dev/null; should use marker file: %s", joined)
	}
	if !strings.Contains(joined, "--ro-bind /tmp/omac-marker-") {
		t.Errorf("no marker file bind found: %s", joined)
	}
}

func TestBwrapMarkerDirUsedWhenDenialTextSet(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	g := &Grants{
		Workdir:        home,
		AllowPaths:     []string{home},
		ProtectedPaths: []string{sshDir},
		NetworkMode:    sandboxprofile.ModeBlocked,
		DenialText:     "X-Omac-Sandbox: denied\nprotected\n",
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "--tmpfs "+sshDir) {
		t.Errorf("still using plain tmpfs; should use marker dir: %s", joined)
	}
	if !strings.Contains(joined, "--bind /tmp/omac-markerdir-") {
		t.Errorf("no marker dir bind found: %s", joined)
	}
}

func TestBwrapFallsBackToDevnullWhenNoDenialText(t *testing.T) {
	home := t.TempDir()
	netrc := filepath.Join(home, ".netrc")
	if err := os.WriteFile(netrc, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Grants{
		Workdir:        home,
		AllowPaths:     []string{home},
		ProtectedPaths: []string{netrc},
		NetworkMode:    sandboxprofile.ModeBlocked,
	}
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--ro-bind /dev/null "+netrc) {
		t.Errorf("should fall back to /dev/null: %s", joined)
	}
}
