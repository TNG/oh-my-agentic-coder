//go:build vuln && linux

package sandboxrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// stripBareTmp removes the baseline's "/tmp"/"/private/tmp" grant AND its
// separate "$TMPDIR" grant, isolating this test from two other, already-
// tracked defects (TestSecurityBaselineDoesNotGrantHostTmp and the $TMPDIR
// entry baseline.go grants alongside it) that would otherwise make the
// cache-sharing property below untestable on its own: every path this test
// (or any Go test) creates via t.TempDir() lives under $TMPDIR, so without
// stripping it too, every sandbox already has broad read-write access to
// this test's own scratch tree regardless of any cache-specific grant.
func stripBareTmp(paths []string) []string {
	tmpdir := os.Getenv("TMPDIR")
	return slices.DeleteFunc(paths, func(p string) bool {
		return p == "/tmp" || p == "/private/tmp" || (tmpdir != "" && p == tmpdir)
	})
}

// TestSecurityToolCacheNotWritableAcrossSessionsAtKernelLevel is the
// kernel-enforcement complement to TestSecurityDefaultToolCacheNotShared
// AcrossWorkdirs (argv-shape only): it actually runs bubblewrap twice,
// simulating two independent sessions granted the same persistent cache
// directory read-write (start.go/serve.go inject `--allow <cache dir>`
// unconditionally, independent of workdir), and shows one session's
// sandboxed process can overwrite a file the other session wrote there.
func TestSecurityToolCacheNotWritableAcrossSessionsAtKernelLevel(t *testing.T) {
	requireWorkingBwrap(t)

	omac := buildOmac(t)
	cacheDir := t.TempDir() // stands in for the real, home-derived shared cache dir
	marker := filepath.Join(cacheDir, "cached-tool-manifest")

	run := func(allowCache bool, script string) (string, error) {
		wd := t.TempDir()
		p := &sandboxprofile.Profile{
			Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
			Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
		}
		g, err := ResolveGrants(p, wd, nil)
		if err != nil {
			t.Fatal(err)
		}
		g.WritePaths = stripBareTmp(g.WritePaths)
		g.AllowPaths = stripBareTmp(g.AllowPaths)
		g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac))
		if allowCache {
			g.AllowPaths = append(g.AllowPaths, cacheDir)
		}
		stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
		tail := append(append([]string{}, stage2...), "--", "/bin/sh", "-c", script)
		argv, err := BuildBwrapArgv(g, tail)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		return string(out), err
	}

	// "Session A": writes the file a real tool-cache population would leave
	// behind.
	if out, err := run(true, "echo from-session-a > "+marker); err != nil {
		t.Fatalf("session A setup write failed: %v: %s", err, out)
	}
	if data, err := os.ReadFile(marker); err != nil || !strings.Contains(string(data), "from-session-a") {
		t.Fatalf("control: session A's write did not land at %s (%v): the fixture is broken, not the security property", marker, err)
	}

	// Control: an UNGRANTED sandbox cannot touch the same file — proves
	// this test's sandbox construction really does confine writes in
	// general, so a successful overwrite below is about cache-sharing
	// specifically, not a broken fixture.
	out, ctlErr := run(false, "echo from-ungranted > "+marker)
	if ctlErr == nil {
		if data, _ := os.ReadFile(marker); strings.Contains(string(data), "from-ungranted") {
			t.Fatalf("control: a sandbox with NO grant to %s could still overwrite it (%s): the fixture's confinement itself is broken, not the security property", cacheDir, out)
		}
	}

	// "Session B": an entirely separate sandbox launch (different workdir,
	// standing in for a different project/session), granted the SAME
	// persistent cache directory — exactly what start.go/serve.go do by
	// default, independent of workdir.
	if out, err := run(true, "echo from-session-b > "+marker); err != nil {
		t.Fatalf("session B write failed: %v: %s", err, out)
	}

	data, err := os.ReadFile(marker)
	if err == nil && strings.Contains(string(data), "from-session-b") {
		t.Errorf("a second, independent sandboxed session overwrote a file the first session wrote into the shared persistent tool cache (%s): "+
			"the cache is a shared writable location across sessions, not merely across workdirs — one session can poison another's cached tool state", marker)
	}
}
