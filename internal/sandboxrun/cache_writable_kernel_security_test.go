//go:build linux

package sandboxrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/toolcache"
)

// stripBareTmp removes any /tmp, /private/tmp and the sandbox's own $TMPDIR
// from the grant lists so the test is not masked by a blanket /tmp write grant.
func stripBareTmp(paths []string) []string {
	tmpdir := os.Getenv("TMPDIR")
	out := paths[:0:0]
	for _, p := range paths {
		if p == "/tmp" || p == "/private/tmp" || (tmpdir != "" && p == tmpdir) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// TestSecurityToolCacheNotWritableAcrossSessionsAtKernelLevel verifies that
// two sandbox sessions started from different workdirs cannot write into each
// other's tool cache. With the default workdir scope each workdir resolves to
// a distinct cache directory, so kernel-level isolation requires no additional
// mechanism: session B simply has no grant on session A's directory.
//
// This complements TestSecurityDefaultToolCacheNotSharedAcrossWorkdirs (which
// only checks the resolved scope path) by actually running bubblewrap.
func TestSecurityToolCacheNotWritableAcrossSessionsAtKernelLevel(t *testing.T) {
	requireWorkingBwrap(t)

	omac := buildOmac(t)

	home := t.TempDir()
	t.Setenv("HOME", home)

	wdA := t.TempDir()
	wdB := t.TempDir()

	scopeA, err := toolcache.DescribePersistent(toolcache.DomainWorkdir, wdA)
	if err != nil {
		t.Fatal(err)
	}
	scopeB, err := toolcache.DescribePersistent(toolcache.DomainWorkdir, wdB)
	if err != nil {
		t.Fatal(err)
	}
	if scopeA.Dir == scopeB.Dir {
		t.Fatal("workdir scopes unexpectedly collided — the isolation property is untestable")
	}
	if err := os.MkdirAll(scopeA.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scopeB.Dir, 0o700); err != nil {
		t.Fatal(err)
	}

	markerA := filepath.Join(scopeA.Dir, "session-a-artifact")
	if err := os.WriteFile(markerA, []byte("from-session-a"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Session B: granted only scopeB.Dir (its own workdir cache), not scopeA.Dir.
	run := func(workdir, cacheDir, script string) (string, error) {
		p := &sandboxprofile.Profile{
			Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
			Filesystem: sandboxprofile.Filesystem{
				Allow: []string{cacheDir},
			},
			Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
		}
		g, err := ResolveGrants(p, workdir, nil)
		if err != nil {
			t.Fatal(err)
		}
		g.WritePaths = stripBareTmp(g.WritePaths)
		g.AllowPaths = stripBareTmp(g.AllowPaths)
		g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac))
		stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
		tail := append(append([]string{}, stage2...), "--", "/bin/sh", "-c", script)
		argv, err := BuildBwrapArgv(g, tail)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		return string(out), err
	}

	// Control: session B can write inside its own cache scope.
	if out, err := run(wdB, scopeB.Dir, "echo ok > "+filepath.Join(scopeB.Dir, "probe")); err != nil {
		t.Fatalf("control: session B cannot write to its own cache: %v: %s", err, out)
	}

	// Session B must NOT be able to read or overwrite session A's artifact.
	out, _ := run(wdB, scopeB.Dir, "cat "+markerA+" 2>&1")
	if strings.Contains(out, "from-session-a") {
		t.Errorf("session B (workdir %s, cache %s) could read session A's cached artifact at %s: "+
			"the workdir-scoped cache does not isolate sessions at the kernel level", wdB, scopeB.Dir, markerA)
	}

	if _, err := run(wdB, scopeB.Dir, "echo from-session-b > "+markerA); err == nil {
		if data, readErr := os.ReadFile(markerA); readErr == nil && strings.Contains(string(data), "from-session-b") {
			t.Errorf("session B overwrote session A's cache artifact %s: "+
				"workdir-scoped caches must not overlap", markerA)
		}
	}

	// Confirm session A's artifact is unchanged on the host.
	if data, err := os.ReadFile(markerA); err != nil || !strings.Contains(string(data), "from-session-a") {
		t.Errorf("session A's artifact was modified or unreadable after session B ran: data=%q err=%v", data, err)
	}
}
