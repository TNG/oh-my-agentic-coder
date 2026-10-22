package sandboxrun

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/netprompt"
)

func TestWriteProtectProfilePaths(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(profile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Both come back; the pages file is created if missing.
	got, err := writeProtectProfilePaths(profile, profile, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{profile, filepath.Join(dir, "sandbox.pages.json")}
	if !slices.Equal(got, want) {
		t.Errorf("writeProtectProfilePaths = %v, want %v", got, want)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sandbox.pages.json"))
	if err != nil {
		t.Fatalf("pages file not created: %v", err)
	}
	if len(data) == 0 {
		t.Error("pages file must be created with a valid empty store, not 0 bytes")
	}

	// A second call leaves the existing pages file untouched.
	if err := os.WriteFile(filepath.Join(dir, "sandbox.pages.json"), []byte("{\"schema\":99}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeProtectProfilePaths(profile, profile, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "sandbox.pages.json"))
	if string(data) != "{\"schema\":99}" {
		t.Errorf("existing pages file must not be overwritten, got %q", data)
	}

	// A deny on the profile's parent directory drops both candidates:
	// deny is stricter than read-only, at any depth.
	got, err = writeProtectProfilePaths(profile, profile, []string{dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("candidates under a denied directory must be dropped, got %v", got)
	}
}

func TestWriteProtectProfilePathsSymlinkPolicy(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Path-form ref pointing at a symlink is rejected.
	if _, err := writeProtectProfilePaths(link, link, nil, io.Discard); err == nil {
		t.Error("a path-form symlinked profile must be rejected")
	}

	// A named ref resolving to a symlink protects the target (dotfiles
	// managers symlink ~/.config).
	got, err := writeProtectProfilePaths("default", link, nil, io.Discard)
	if err != nil {
		t.Fatalf("named-ref symlink must resolve, got %v", err)
	}
	if !slices.Contains(got, target) {
		t.Errorf("the symlink target must be protected, got %v", got)
	}
}

// A pages file that cannot be created must not fail the launch: the
// protection for it is skipped with a warning instead.
func TestWriteProtectProfilePathsPagesCreationFailureWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	profile := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(profile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	var buf strings.Builder
	got, err := writeProtectProfilePaths(profile, profile, nil, &buf)
	if err != nil {
		t.Fatalf("pages creation failure must not be fatal, got %v", err)
	}
	if !slices.Equal(got, []string{profile}) {
		t.Errorf("only the profile must remain protected, got %v", got)
	}
	if !strings.Contains(buf.String(), "write-protection is skipped") {
		t.Errorf("the skip must be warned about, got: %s", buf.String())
	}
}

// Learn mode must keep the write-protection (the profile is the file learn
// mode collects its results into at exit).
func TestWriteProtectedPathsSurviveLearnMode(t *testing.T) {
	g := &Grants{Workdir: "/w", WriteProtectedPaths: []string{"/w/.opencode/sandbox.json"}}
	out := g.withUnrestrictedFilesystem()
	if !slices.Equal(out.WriteProtectedPaths, g.WriteProtectedPaths) {
		t.Errorf("WriteProtectedPaths must survive learn mode, got %v", out.WriteProtectedPaths)
	}
	if len(out.ProtectedPaths) != 0 {
		t.Errorf("ProtectedPaths must still be dropped in learn mode, got %v", out.ProtectedPaths)
	}
}

// The empty store EnsureLearnedPolicyFile writes must load back cleanly,
// otherwise every first launch would warn about an unparseable page policy.
func TestWriteProtectProfilePathsPagesLoads(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "p.json")
	if err := os.WriteFile(profile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeProtectProfilePaths(profile, profile, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	lp, err := netprompt.LoadLearnedPolicy(filepath.Join(dir, "p.pages.json"))
	if err != nil {
		t.Fatalf("created pages file does not load: %v", err)
	}
	if entries := lp.Entries(); len(entries) != 0 {
		t.Errorf("created pages file must be empty, got %v", entries)
	}
}
