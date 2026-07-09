package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

const testDenialText = "X-Omac-Sandbox: denied\nprotected\n"

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
		DenialText:     testDenialText,
	}
	cleanup, err := g.prepareMarkers()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "--ro-bind /dev/null "+netrc) {
		t.Errorf("still using /dev/null; should use marker file: %s", joined)
	}
	if !strings.Contains(joined, "--ro-bind "+g.markerFile+" "+netrc) {
		t.Errorf("no marker-file bind found: %s", joined)
	}
	// The bind source must exist (bwrap reads it at launch) and carry the
	// denial text verbatim.
	got, err := os.ReadFile(g.markerFile)
	if err != nil {
		t.Fatalf("marker file unreadable: %v", err)
	}
	if string(got) != testDenialText {
		t.Errorf("marker file content = %q, want %q", got, testDenialText)
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
		DenialText:     testDenialText,
	}
	cleanup, err := g.prepareMarkers()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "--tmpfs "+sshDir) {
		t.Errorf("still using plain tmpfs; should use marker dir: %s", joined)
	}
	if !strings.Contains(joined, "--ro-bind "+g.markerDir+" "+sshDir) {
		t.Errorf("no marker-dir bind found: %s", joined)
	}
	// The .omac-denied file inside the marker dir must exist and carry the
	// denial text (not, e.g., a temp-file path — regression guard).
	got, err := os.ReadFile(filepath.Join(g.markerDir, markerDirFileName))
	if err != nil {
		t.Fatalf("marker-dir notice unreadable: %v", err)
	}
	if string(got) != testDenialText {
		t.Errorf("marker-dir notice content = %q, want %q", got, testDenialText)
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
	cleanup, err := g.prepareMarkers()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--ro-bind /dev/null "+netrc) {
		t.Errorf("should fall back to /dev/null: %s", joined)
	}
}

func TestPrepareMarkersNoopWithoutDenialText(t *testing.T) {
	g := &Grants{ProtectedPaths: []string{"/x"}}
	cleanup, err := g.prepareMarkers()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if g.markerFile != "" || g.markerDir != "" {
		t.Errorf("expected no markers without denial text, got file=%q dir=%q", g.markerFile, g.markerDir)
	}
}
