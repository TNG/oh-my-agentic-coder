//go:build linux

package sandboxrun

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"golang.org/x/sys/unix"
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
// On Linux the denial is layered. Landlock network rules cover TCP connect and
// bind and nothing else. The datagram gap Landlock cannot express is closed by
// a seccomp-BPF filter (applyDatagramSeccomp) that denies every socket(2)
// combination outside a TCP-only allowlist (and the io_uring syscalls that
// could create a socket without socket(2)), so non-TCP egress such as UDP,
// QUIC, DNS, ICMP and tunnels built out of datagrams is blocked at socket
// creation rather than left unexamined. ICMP via raw ping is still flaky in CI
// because ping's success depends on net.ipv4.ping_group_range, which varies
// across runner environments independently of sandboxing; this UDP test is the
// stable witness that the datagram class is confined.
//
// This test compares a TCP connection and a UDP datagram across the same
// boundary to the same address, so the difference it reports is the protocol
// and nothing else.

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

// seccompProbeEnv marks a re-exec'd test-binary invocation that runs an
// in-sandbox probe for one of the seccomp security tests instead of the
// normal test body. The probe runs under the real stage2 enforcement stack
// (Landlock net rules + applyDatagramSeccomp) applied by `omac sandbox
// stage2` inside bwrap, then reports the result via its exit code:
// 0 means the security property holds, non-zero means a violation (the
// output explains which case). Re-execing the compiled test binary is what
// lets the probe issue raw socket(2)/io_uring syscalls from inside the
// enforced sandbox without the test process itself becoming sandboxed.
const seccompProbeEnv = "OMAC_SECCOMP_PROBE"

// runSeccompProbe launches the test binary inside a kernel-enforced sandbox
// so the probe body runs under the production stage2 stack. The binary is
// re-exec'd with seccompProbeEnv=mode and -test.run filtered to runName,
// which detects the env and runs the matching probe.
func runSeccompProbe(t *testing.T, mode, runName string) (string, int) {
	t.Helper()
	requireWorkingBwrap(t)
	if !LandlockNetSupported() {
		t.Fatalf("Landlock network rules unavailable (ABI %d < 4): kernel-enforced filtered mode is not enforced here, so this test cannot tell a filter gap from an unconfigured host", LandlockABI())
	}
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	omac := buildOmac(t)
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		// No ports opened: nothing is reachable except what the filter lets
		// through. The probes only test socket creation, not connect/bind.
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeFiltered},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The stage2 binary and the re-exec'd test binary both live outside the
	// workdir; grant their directories so they are visible in the namespace.
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(omac), filepath.Dir(testBin))

	stage2 := append([]string{omac, "sandbox", "stage2"}, Stage2Args(g)...)
	tail := append(append([]string{}, stage2...), "--", testBin, "-test.run=^"+runName+"$", "-test.v")
	argv, err := BuildBwrapArgv(g, tail)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), seccompProbeEnv+"="+mode)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("launch seccomp probe: %v\n%s", err, out)
	}
	return string(out), code
}

// TestSecuritySeccompDeniesNonTcpSocketProtocols asserts that the stage2
// seccomp filter denies socket(2) combinations outside the TCP allowlist —
// non-default transports over AF_INET, SOCK_SEQPACKET, SOCK_RAW and
// AF_PACKET — while plain SOCK_STREAM/IPPROTO_TCP still succeeds. Landlock
// mediates TCP only, so this filter is the sole layer that can express the
// denial; the assertion is therefore made directly against the enforced
// stack rather than against egress traffic.
func TestSecuritySeccompDeniesNonTcpSocketProtocols(t *testing.T) {
	if mode := os.Getenv(seccompProbeEnv); mode == "nontcp" {
		os.Exit(runNonTcpSocketProbe())
	}
	out, code := runSeccompProbe(t, "nontcp", "TestSecuritySeccompDeniesNonTcpSocketProtocols")
	if code != 0 {
		t.Fatalf("non-TCP socket protocols not denied by the seccomp filter (exit %d):\n%s", code, out)
	}
	t.Logf("probe output:\n%s", out)
}

// runNonTcpSocketProbe runs inside the enforced sandbox. Each case creates a
// socket(2) and checks whether the filter denies it. Allowed cases prove the
// filter is not over-blocking; denied cases prove the allowlist closes the
// non-TCP gap. Exit 0 when every case matches expectation.
func runNonTcpSocketProbe() int {
	type c struct {
		name               string
		domain, typ, proto int
		wantEPERM          bool
	}
	cases := []c{
		{"inet-stream-default", syscall.AF_INET, syscall.SOCK_STREAM, 0, false},
		{"inet-stream-tcp", syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP, false},
		{"inet-stream-sctp", syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_SCTP, true},
		{"inet-seqpacket-sctp", syscall.AF_INET, syscall.SOCK_SEQPACKET, syscall.IPPROTO_SCTP, true},
		{"inet-raw", syscall.AF_INET, syscall.SOCK_RAW, 0, true},
		{"af-packet-raw", syscall.AF_PACKET, syscall.SOCK_RAW, 0, true},
	}
	failed := 0
	for _, k := range cases {
		fd, err := syscall.Socket(k.domain, k.typ, k.proto)
		gotEPERM := errors.Is(err, syscall.EPERM)
		if fd >= 0 {
			syscall.Close(fd)
		}
		switch {
		case gotEPERM && !k.wantEPERM:
			fmt.Printf("FAIL %s: denied with EPERM, want allowed (%v)\n", k.name, err)
			failed++
		case !gotEPERM && k.wantEPERM:
			fmt.Printf("FAIL %s: allowed (err=%v), want EPERM\n", k.name, err)
			failed++
		default:
			fmt.Printf("OK %s\n", k.name)
		}
	}
	return failed
}

// TestSecuritySeccompDeniesIoUringSocketCreation asserts the stage2 seccomp
// filter denies the io_uring syscalls. Since Linux 5.19 a socket can be
// created in kernel context via io_uring without a socket(2) call, so
// filtering socket(2) alone cannot confine egress; denying the io_uring
// family closes that path. The host/container must permit io_uring outside
// the sandbox — otherwise the in-sandbox denial could not be told apart
// from the outer profile and the test fails rather than report a vacuous
// pass.
func TestSecuritySeccompDeniesIoUringSocketCreation(t *testing.T) {
	if mode := os.Getenv(seccompProbeEnv); mode == "iouring" {
		os.Exit(runIoUringProbe())
	}
	// Precondition: outside the sandbox io_uring must be usable. If the
	// host already denies it, the in-sandbox assertion would pass for the
	// wrong reason, so fail loudly instead of guessing.
	var params [128]byte
	fd, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, 4, uintptr(unsafe.Pointer(&params[0])), 0)
	if errno == syscall.EPERM {
		t.Fatalf("io_uring_setup returns EPERM outside the sandbox: the host/container denies io_uring, so the in-sandbox denial cannot be verified here; run in an e2e container that permits io_uring")
	}
	if errno == 0 {
		unix.Close(int(fd))
	}
	out, code := runSeccompProbe(t, "iouring", "TestSecuritySeccompDeniesIoUringSocketCreation")
	if code != 0 {
		t.Fatalf("io_uring not denied inside the sandbox (exit %d):\n%s", code, out)
	}
	t.Logf("probe output:\n%s", out)
}

// runIoUringProbe runs inside the enforced sandbox and asserts each io_uring
// syscall returns EPERM. Denying all three closes ring creation and every
// submission path, so no socket can be created through io_uring and no
// datagram built on it can send.
func runIoUringProbe() int {
	failed := 0
	check := func(name string, errno syscall.Errno) {
		if errno != syscall.EPERM {
			fmt.Printf("FAIL %s: errno=%d (%v), want EPERM\n", name, int(errno), errno)
			failed++
			return
		}
		fmt.Printf("OK %s: EPERM\n", name)
	}
	var params [128]byte
	_, _, e1 := unix.Syscall6(unix.SYS_IO_URING_SETUP, 4, uintptr(unsafe.Pointer(&params[0])), 0, 0, 0, 0)
	check("io_uring_setup", syscall.Errno(e1))
	// An invalid fd is used so the call cannot accidentally create state; the
	// filter is expected to fire at syscall entry, before any fd validation.
	_, _, e2 := unix.Syscall6(unix.SYS_IO_URING_ENTER, ^uintptr(0), 0, 0, 0, 0, 0)
	check("io_uring_enter", syscall.Errno(e2))
	_, _, e3 := unix.Syscall6(unix.SYS_IO_URING_REGISTER, ^uintptr(0), 0, 0, 0, 0, 0)
	check("io_uring_register", syscall.Errno(e3))
	return failed
}
