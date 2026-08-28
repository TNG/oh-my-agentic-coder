package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

const testDenialText = "X-Omac-Sandbox: denied\nprotected\n"

// wantInertMarker asserts the bytes bwrap binds into the sandbox carry
// the denial text but cannot execute: every content line is commented
// (#213 — the marker is bound over shell configs, which get sourced).
func wantInertMarker(t *testing.T, path, wantText string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("marker unreadable: %v", err)
	}
	for i, line := range strings.Split(string(got), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			t.Errorf("%s line %d is executable content: %q", path, i+1, line)
		}
	}
	for _, want := range strings.Split(strings.TrimSpace(wantText), "\n") {
		if want == "" {
			continue
		}
		if !strings.Contains(string(got), strings.TrimSpace(want)) {
			t.Errorf("%s lost denial text %q, got:\n%s", path, want, got)
		}
	}
}

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
	// denial text — in its inert form, since the same marker masks shell
	// configs.
	wantInertMarker(t, g.markerFile, testDenialText)
}

// TestBwrapMarkerNeutralizesHostileDenialText pins the scope note in
// #213: denial.marker_file is profile-configurable, so a profile must
// not be able to bind a command over a file the sandboxed process
// sources.
func TestBwrapMarkerNeutralizesHostileDenialText(t *testing.T) {
	home := t.TempDir()
	profile := filepath.Join(home, ".profile")
	if err := os.WriteFile(profile, []byte("export SECRET=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Grants{
		Workdir:        home,
		AllowPaths:     []string{home},
		ProtectedPaths: []string{profile},
		NetworkMode:    sandboxprofile.ModeBlocked,
		DenialText:     "touch " + filepath.Join(home, "pwned") + "\n",
	}
	cleanup, err := g.prepareMarkers()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	got, err := os.ReadFile(g.markerFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# touch " + filepath.Join(home, "pwned") + "\n"; string(got) != want {
		t.Errorf("marker file = %q; want neutralized %q", got, want)
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
	wantInertMarker(t, filepath.Join(g.markerDir, markerDirFileName), testDenialText)
}

func TestBwrapMarkerDirHonorsCustomName(t *testing.T) {
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const customName = ".access-restricted"
	g := &Grants{
		Workdir:        home,
		AllowPaths:     []string{home},
		ProtectedPaths: []string{sshDir},
		NetworkMode:    sandboxprofile.ModeBlocked,
		DenialText:     testDenialText,
		DenialDirName:  customName,
	}
	cleanup, err := g.prepareMarkers()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// The notice inside the marker dir must use the configured name, not
	// the hard-coded default.
	if _, err := os.Stat(filepath.Join(g.markerDir, customName)); err != nil {
		t.Errorf("custom marker_dir_name %q not honored: %v", customName, err)
	}
	if _, err := os.Stat(filepath.Join(g.markerDir, markerDirFileName)); err == nil {
		t.Errorf("default name %q written despite custom marker_dir_name", markerDirFileName)
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
