//go:build vuln && linux

package sandboxrun

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestSecurityHostTmpNotUsableAsSharedScratch asserts, by actually running
// bubblewrap, that a write to /tmp from inside the sandbox does not land in
// the real host's shared /tmp.
//
// This is the kernel-enforcement complement to
// TestSecurityPrivateTmpfsSurvivesTmpSubpathGrants (which asserts the argv
// shape only): it exercises the current, unfixed default profile end to
// end. A pre-launch marker file at a name derived from this process's own
// pid — not the sandbox's per-launch scratch dir, which is already
// private — proves the write reached the shared host root, not a private
// tmpfs, if it appears there after the sandboxed write.
func TestSecurityHostTmpNotUsableAsSharedScratch(t *testing.T) {
	requireWorkingBwrap(t)

	omac := buildOmac(t)
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac))

	marker := fmt.Sprintf("omac-kernel-probe-%d-%d", os.Getpid(), rand.Int63())
	hostPath := filepath.Join("/tmp", marker)
	defer os.Remove(hostPath)

	if _, err := os.Stat(hostPath); err == nil {
		t.Fatalf("control: the marker path %s already exists before the test ran: the fixture is broken, not the security property", hostPath)
	}

	stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
	tail := append(append([]string{}, stage2...), "--", "/bin/sh", "-c", "echo from-sandbox > "+hostPath)
	argv, err := BuildBwrapArgv(g, tail)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		t.Logf("sandboxed write exited non-zero (%v): %s — a permission or missing-mount error also satisfies the property (no shared /tmp reachable)", err, out)
		return
	}

	if _, statErr := os.Stat(hostPath); statErr == nil {
		content, _ := os.ReadFile(hostPath)
		t.Errorf("a file written by the sandboxed process to /tmp/%s is visible on the real host filesystem (content: %q): "+
			"the confined agent can read, alter, or delete whatever any other session or program on the machine left in /tmp, and vice versa", marker, string(content))
	}
}
