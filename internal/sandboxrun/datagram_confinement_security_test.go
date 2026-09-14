//go:build vuln && linux

package sandboxrun

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// What "filtered" covers.
//
// In filtered mode the sandbox is required to deny the child's network access
// at the kernel level, letting through only the supervisor proxy on loopback,
// the ports the profile opens, granted Unix sockets, and the platform's local
// DNS facilities. Everything the user sees — the approval popup, the domain
// allow and deny lists, the audit trail of what the agent reached — is built
// on that denial holding, because only traffic that has to go through the
// proxy can be filtered at all.
//
// On Linux the denial is implemented with Landlock, whose network rules cover
// TCP connect and bind and nothing else. Landlock has no concept of a
// datagram, and bwrap is deliberately run without a network namespace, so no
// second mechanism picks up what Landlock cannot express. Anything that is not
// TCP therefore leaves the sandbox unexamined: DNS queries, QUIC — which is
// how a current browser or HTTP client prefers to speak — and any tunnel a
// process cares to build out of UDP.
//
// That is an exfiltration path with no prompt, no filtering and no record, for
// data the agent was legitimately allowed to read. This test compares the two
// protocols across the same boundary to the same address, so the difference it
// reports is the protocol and nothing else.

// requireWorkingBwrap fails, rather than skips, when the sandbox cannot be
// launched. The suite's runner reads a skipped test as a passing one, so
// skipping here would report the sandbox as confining datagrams on every
// machine that cannot test it — the one answer that must never be guessed.
// Deliberately not the package's requireBwrap, which skips by design because
// the tests it guards are not security assertions.
func requireWorkingBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Fatalf("bwrap is not installed (%v): this test cannot confirm datagram confinement without it; run the suite in the e2e container", err)
	}
	if err := exec.Command("bwrap", "--ro-bind", "/", "/", "true").Run(); err != nil {
		t.Fatalf("bwrap cannot create a sandbox here (%v), usually unprivileged user namespaces being disabled: run the suite in the e2e container", err)
	}
}

// firstNonLoopbackIPv4 returns an address on a real interface. Loopback is
// deliberately avoided: the sandbox is allowed to reach some loopback
// services, so a loopback probe could not distinguish a policy allowance from
// a gap in enforcement.
func firstNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("enumerate interfaces: %v", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if v4 := ipnet.IP.To4(); v4 != nil {
					return v4.String()
				}
			}
		}
	}
	t.Fatal("no non-loopback IPv4 interface: this test needs one to tell egress from loopback traffic")
	return ""
}

// buildOmac compiles the real binary, which stage 2 must be: applying the
// Landlock rules is the omac binary's own job and the test binary cannot
// stand in for it.
func buildOmac(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "omac")
	build := exec.Command("go", "build", "-o", bin, "github.com/TNG/oh-my-agentic-coder/cmd/omac")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build omac: %v\n%s", err, out)
	}
	return bin
}

// TestSecurityDatagramEgressConfined asserts that filtered mode stops a
// datagram leaving the sandbox, as it stops a TCP connection.
func TestSecurityDatagramEgressConfined(t *testing.T) {
	requireWorkingBwrap(t)
	if !LandlockNetSupported() {
		t.Fatalf("Landlock network rules unavailable (ABI %d < 4): filtered mode is not enforced at all here, so this test cannot tell a gap from an unconfigured host", LandlockABI())
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Fatalf("bash is required (the probes use its /dev/udp and /dev/tcp): %v", err)
	}

	host := firstNonLoopbackIPv4(t)

	udpConn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer udpConn.Close()
	udpPort := udpConn.LocalAddr().(*net.UDPAddr).Port

	tcpLn, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer tcpLn.Close()
	tcpPort := tcpLn.Addr().(*net.TCPAddr).Port

	omac := buildOmac(t)
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		// Neither probe port is opened: both must be out of reach.
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeFiltered},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac))

	stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
	probe := func(script string) (string, int) {
		t.Helper()
		tail := append(append([]string{}, stage2...), "--", "/bin/bash", "-c", script)
		argv, err := BuildBwrapArgv(g, tail)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return string(out), ee.ExitCode()
			}
			t.Fatalf("exec: %v (%s)", err, out)
		}
		return string(out), 0
	}

	// Control: a TCP connection to the same unopened port is refused by the
	// kernel. It proves the sandbox is really enforcing filtered mode, so the
	// datagram result below is about the protocol rather than about a
	// misconfigured launch.
	out, code := probe(fmt.Sprintf(`exec 3<>/dev/tcp/%s/%d`, host, tcpPort))
	if code == 0 {
		t.Fatalf("a TCP connection to an unopened port succeeded (%s): filtered mode is not being enforced, so this test proves nothing", out)
	}

	probe(fmt.Sprintf(`echo omac-datagram-probe > /dev/udp/%s/%d`, host, udpPort))

	buf := make([]byte, 128)
	_ = udpConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := udpConn.ReadFrom(buf)
	if err != nil {
		return // nothing arrived: the property holds
	}
	t.Errorf("a datagram sent from inside the sandbox arrived outside it carrying %q, while TCP to the same host was refused: anything the agent can read can be sent out over UDP — DNS, QUIC, a hand-rolled tunnel — with no prompt, no domain filtering and no audit record", string(buf[:n]))
}
