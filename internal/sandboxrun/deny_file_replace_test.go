package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// denyFileFixture mirrors the opencode harness grant shape (#347): a
// read+write dir grant with one explicitly denied file inside it.
type denyFileFixture struct {
	Dir     string // granted read+write
	File    string // denied, inside Dir
	Sibling string // not denied, inside Dir
	Content string // original content of File
}

const denyFileSecret = "HOST-SERVICE-SETTINGS-0f3c"

// newDenyFileFixture creates the dir, the sibling file and, when withFile is
// set, the denied file, and resolves grants the way `omac start` does for a
// harness: the dir as --allow, the file as --deny.
func newDenyFileFixture(t *testing.T, withFile bool) (*denyFileFixture, *Grants) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "opencode")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fx := &denyFileFixture{
		Dir:     dir,
		File:    filepath.Join(dir, "service.json"),
		Sibling: filepath.Join(dir, "other.json"),
		Content: `{"port":1,"note":"` + denyFileSecret + `"}`,
	}
	if withFile {
		if err := os.WriteFile(fx.File, []byte(fx.Content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(fx.Sibling, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Allow: []string{dir}, Deny: []string{fx.File}},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsPath(g.ProtectedPaths, fx.File) {
		t.Fatalf("denied file %s missing from ProtectedPaths %v", fx.File, g.ProtectedPaths)
	}
	if !containsPath(g.AllowPaths, dir) {
		t.Fatalf("dir grant %s missing from AllowPaths %v", dir, g.AllowPaths)
	}
	return fx, g
}

func containsPath(list []string, p string) bool {
	for _, v := range list {
		if v == p {
			return true
		}
	}
	return false
}

// probeScript tries every way to read, write or replace the denied file from
// inside the sandbox; each attempt runs on its own so one failure does not
// skip the rest. The sibling write is the control: the dir grant still works.
func (fx *denyFileFixture) probeScript() string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	d, f := q(fx.Dir), q(fx.File)
	return strings.Join([]string{
		"cat " + f,
		"echo APPENDED >> " + f,
		"echo TRUNCATED > " + f,
		"printf REPLACED > " + d + "/.tmp-rename && mv -f " + d + "/.tmp-rename " + f,
		"ln -s /etc/hosts " + d + "/.tmp-link && mv -f " + d + "/.tmp-link " + f,
		"ln -sf /etc/hosts " + f,
		"rm -f " + f,
		"mv " + f + " " + d + "/.moved",
		"ln " + f + " " + d + "/.hardlink && cat " + d + "/.hardlink",
		"printf CREATED > " + f + " && echo PROBE-WROTE-FILE",
		"echo SIBLING-OK > " + q(fx.Sibling) + " && cat " + q(fx.Sibling),
	}, "\n") + "\nexit 0\n"
}

// check asserts the host-side outcome of probeScript: the denied file kept its
// content (or still does not exist), was never readable, was not moved or
// linked away, and the sibling in the same granted dir stayed writable.
func (fx *denyFileFixture) check(t *testing.T, out string, withFile bool) {
	t.Helper()
	if strings.Contains(out, "PROBE-WROTE-FILE") {
		t.Errorf("the denied path was writable inside the sandbox: %q", out)
	}
	if strings.Contains(out, denyFileSecret) {
		t.Errorf("denied file content was readable inside the sandbox: %q", out)
	}
	fi, err := os.Lstat(fx.File)
	switch {
	case withFile && err != nil:
		t.Errorf("denied file was removed or moved away: %v", err)
	case withFile && fi.Mode()&os.ModeSymlink != 0:
		t.Errorf("denied file was replaced by a symlink")
	case withFile:
		if got, _ := os.ReadFile(fx.File); string(got) != fx.Content {
			t.Errorf("denied file changed: %q, want %q", got, fx.Content)
		}
	case !withFile && err == nil:
		t.Errorf("denied file was created inside the sandbox (mode %v)", fi.Mode())
	}
	for _, name := range []string{".moved", ".hardlink"} {
		if _, err := os.Lstat(filepath.Join(fx.Dir, name)); err == nil {
			t.Errorf("%s exists: the denied file was moved or linked out of its denied path", name)
		}
	}
	if got, _ := os.ReadFile(fx.Sibling); strings.TrimSpace(string(got)) != "SIBLING-OK" {
		t.Errorf("sibling in the granted dir is no longer writable (content %q); the deny must not narrow the dir grant", got)
	}
}
