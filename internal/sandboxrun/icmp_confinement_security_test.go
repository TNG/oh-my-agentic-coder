//go:build vuln && linux

package sandboxrun

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// requireICMPCapable fails, rather than skips, when this host cannot send
// an ICMP echo as an unprivileged process (no working "ping socket" —
// SOCK_DGRAM+IPPROTO_ICMP — via a permissive net.ipv4.ping_group_range,
// and no setuid ping binary). Deliberately not a skip, for the same reason
// requireWorkingBwrap isn't: a skipped test reads as datagram confinement
// holding for ICMP specifically, on every machine that cannot test it.
func requireICMPCapable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ping"); err != nil {
		t.Fatalf("ping is not installed (%v): this test cannot confirm ICMP confinement without it", err)
	}
	if out, err := exec.Command("ping", "-c1", "-W1", "127.0.0.1").CombinedOutput(); err != nil {
		t.Fatalf("ping cannot send an ICMP echo on this host even outside any sandbox (%v): %s\n"+
			"likely net.ipv4.ping_group_range excludes this process's groups and ping has no setuid bit; "+
			"run the suite in the e2e container, or as a user in a permitted group", err, out)
	}
}

// TestSecurityICMPEgressConfined is TestSecurityDatagramEgressConfined for
// ICMP: the finding's title is "UDP/ICMP", and Landlock's network rules
// (LANDLOCK_ACCESS_NET_BIND_TCP / _CONNECT_TCP) have no concept of ICMP any
// more than they do of UDP, so an echo request leaves the sandbox
// unexamined by the same mechanism already proven for datagrams.
//
// Pings the test HOST'S OWN real interface: the kernel answers its own
// interface's echo requests locally, so no separate listener process (which
// would itself need raw-socket privileges) is needed to observe whether the
// echo left the sandbox and got a reply.
func TestSecurityICMPEgressConfined(t *testing.T) {
	requireWorkingBwrap(t)
	requireICMPCapable(t)
	if !LandlockNetSupported() {
		t.Fatalf("Landlock network rules unavailable (ABI %d < 4): filtered mode is not enforced at all here, so this test cannot tell a gap from an unconfigured host", LandlockABI())
	}

	host := firstNonLoopbackIPv4(t)

	omac := buildOmac(t)
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		// Neither port is opened: TCP to it must be refused for the
		// control below to mean anything.
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeFiltered},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac))

	stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
	run := func(argv ...string) (string, int) {
		t.Helper()
		tail := append(append([]string{}, stage2...), "--")
		tail = append(tail, argv...)
		bwrapArgv, err := BuildBwrapArgv(g, tail)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(bwrapArgv[0], bwrapArgv[1:]...).CombinedOutput()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return string(out), ee.ExitCode()
			}
			t.Fatalf("exec: %v (%s)", err, out)
		}
		return string(out), 0
	}

	// Control: TCP to an unopened port on the same host is refused by
	// Landlock, proving filtered mode is genuinely enforced here — so the
	// ICMP result below is about the protocol, not a misconfigured launch.
	out, code := run("/bin/sh", "-c", fmt.Sprintf(`exec 3<>/dev/tcp/%s/1`, host))
	if code == 0 {
		t.Fatalf("a TCP connection to an unopened port succeeded (%s): filtered mode is not being enforced, so this test proves nothing", out)
	}

	out, code = run("ping", "-c1", "-W2", host)
	if code != 0 {
		return // no reply: the property holds (unlikely, since this is the host's own interface)
	}
	t.Errorf("an ICMP echo sent from inside the sandbox reached %s and got a reply, while TCP to the same host was refused: "+
		"ICMP is not covered by Landlock any more than UDP is, so it leaves the sandbox with no prompt, no filtering, and no audit record. output: %s", host, out)
}
