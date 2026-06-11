package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

func learnGrants(t *testing.T, home string) *Grants {
	t.Helper()
	return &Grants{
		Workdir:        filepath.Join(home, "work"),
		ReadPaths:      []string{"/usr", filepath.Join(home, ".gitconfig")},
		AllowPaths:     []string{filepath.Join(home, ".cache")},
		ProtectedPaths: []string{filepath.Join(home, ".ssh"), filepath.Join(home, ".netrc")},
		NetworkMode:    sandboxprofile.ModeBlocked,
	}
}

// testRecorder builds a learnRecorder with a fixed fake home so the
// test is independent of the real temp-dir layout (t.TempDir() lives
// under /var/folders on macOS, which the recorder excludes by design).
func testRecorder(home string, g *Grants) *learnRecorder {
	r := &learnRecorder{
		seen:      map[string]bool{},
		stop:      make(chan struct{}),
		protected: g.ProtectedPaths,
		home:      home,
	}
	r.excluded = append(r.excluded, g.ReadPaths...)
	r.excluded = append(r.excluded, g.WritePaths...)
	r.excluded = append(r.excluded, g.AllowPaths...)
	r.excluded = append(r.excluded, g.Workdir, "/tmp-test")
	return r
}

func TestLearnRecorderAggregation(t *testing.T) {
	home := "/home/u"
	r := testRecorder(home, learnGrants(t, home))

	// Observed: files in a new project, a granted dir, a protected dir,
	// a system dir, and a temp dir.
	r.record(filepath.Join(home, "Files", "projects", "newproj", "src"))
	r.record(filepath.Join(home, "Files", "projects", "newproj", "src", "deep", "deeper"))
	r.record(filepath.Join(home, ".cache", "go-build"))   // granted -> excluded
	r.record(filepath.Join(home, ".ssh"))                 // protected -> excluded
	r.record("/usr/lib")                                  // system -> excluded
	r.record("/tmp-test/scratch")                         // temp -> excluded
	r.record(filepath.Join(home, "other-project", "sub")) // new
	r.record(filepath.Join(home, "work", "nested"))       // workdir -> excluded

	got := r.candidates()
	want := []string{
		// 4+ components below home truncate to 3 (~/Files/projects/<name>).
		filepath.Join(home, "Files", "projects", "newproj"),
		// <= 3 components below home stay as observed.
		filepath.Join(home, "other-project", "sub"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

func TestLearnRecorderProtectedAncestorsExcluded(t *testing.T) {
	home := "/home/u"
	r := testRecorder(home, learnGrants(t, home))
	// home itself is an ancestor of ~/.ssh: offering it would cover the
	// protected path -> must be excluded.
	r.record(home)
	if got := r.candidates(); len(got) != 0 {
		t.Errorf("ancestor of protected path offered: %v", got)
	}
}

func TestCollapseAncestors(t *testing.T) {
	got := collapseAncestors([]string{"/a/b/c", "/a/b", "/d", "/a/b/e"})
	if !slices.Equal(got, []string{"/a/b", "/d"}) {
		t.Errorf("collapse = %v", got)
	}
}

func TestWithUnrestrictedFilesystem(t *testing.T) {
	g := learnGrants(t, t.TempDir())
	u := g.withUnrestrictedFilesystem()
	if !slices.Contains(u.AllowPaths, "/") {
		t.Error("learn grants must include /")
	}
	if len(u.ProtectedPaths) != 0 {
		t.Error("learn grants must drop protected denials")
	}
	// Original untouched.
	if slices.Contains(g.AllowPaths, "/") || len(g.ProtectedPaths) == 0 {
		t.Error("original grants mutated")
	}
}

func TestOfferLearnedFoldersYes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "default.json")
	if err := sandboxprofile.WriteProfile(profilePath, &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{Allow: []string{"~/.cache"}},
	}); err != nil {
		t.Fatal(err)
	}
	newProj := filepath.Join(home, "newproj")
	var out strings.Builder
	err := OfferLearnedFolders(profilePath, []string{newProj}, strings.NewReader("y\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(profilePath)
	if !strings.Contains(string(data), `"~/newproj"`) {
		t.Errorf("profile missing learned folder (home-abbreviated): %s", data)
	}
	if !strings.Contains(string(data), `"~/.cache"`) {
		t.Errorf("existing entries lost: %s", data)
	}
	if !strings.Contains(string(data), "\n  ") {
		t.Error("rewritten profile not pretty-printed")
	}
}

func TestOfferLearnedFoldersNo(t *testing.T) {
	dir := t.TempDir()
	profilePath := filepath.Join(dir, "default.json")
	if err := sandboxprofile.WriteProfile(profilePath, &sandboxprofile.Profile{}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(profilePath)
	var out strings.Builder
	if err := OfferLearnedFolders(profilePath, []string{"/x"}, strings.NewReader("n\n"), &out); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(profilePath)
	if string(before) != string(after) {
		t.Error("profile must be unchanged on 'no'")
	}
	if !strings.Contains(out.String(), "unchanged") {
		t.Errorf("output = %q", out.String())
	}
}

func TestOfferLearnedFoldersEmpty(t *testing.T) {
	var out strings.Builder
	if err := OfferLearnedFolders("/nonexistent.json", nil, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no new folders") {
		t.Errorf("output = %q", out.String())
	}
}

func TestDiagLogFormat(t *testing.T) {
	var buf strings.Builder
	d := &diagSink{w: &buf, stderr: &buf}
	d.Logf("omac sandbox: net ALLOW example.com:443 (allow_domain)")
	d.Logf("omac sandbox: warning: something odd")
	d.Logf("omac sandbox: notice: skipping nonexistent path /x")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %v", lines)
	}
	if !strings.Contains(lines[0], "INFO") || !strings.Contains(lines[0], "net") ||
		!strings.Contains(lines[0], "ALLOW example.com:443") {
		t.Errorf("net line = %q", lines[0])
	}
	if !strings.Contains(lines[1], "WARN") {
		t.Errorf("warn line = %q", lines[1])
	}
	if !strings.Contains(lines[2], "INFO") || !strings.Contains(lines[2], "fs") {
		t.Errorf("fs line = %q", lines[2])
	}
	// Timestamp prefix: "2026-06-11 10:42:01 ".
	if len(lines[0]) < 20 || lines[0][4] != '-' || lines[0][13] != ':' {
		t.Errorf("timestamp missing/misaligned: %q", lines[0])
	}
}
