//go:build linux

package sandboxrun

import (
	"encoding/binary"
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

// seccompData builds a little-endian seccomp_data buffer carrying the given
// syscall number and socket(2) arguments. Only the fields the filter reads
// (nr, args[0..2]) are populated; the rest is zero.
func seccompData(nr, domain, typ, proto uint32) []byte {
	b := make([]byte, 64)
	binary.LittleEndian.PutUint32(b[offNR:], nr)
	binary.LittleEndian.PutUint32(b[offArg0:], domain)
	binary.LittleEndian.PutUint32(b[offArg1:], typ)
	binary.LittleEndian.PutUint32(b[offArg2:], proto)
	return b
}

// evalDatagramFilter walks the production seccomp-BPF program with a
// userspace interpreter over the supplied seccomp_data and returns the
// action the kernel would apply. It implements exactly the instruction
// classes the filter uses (LD|W|ABS, JMP|JEQ|K, ALU|AND|K, RET|K) so it is
// a faithful trace of the kernel's classic-BPF evaluation for this program.
func evalDatagramFilter(t *testing.T, data []byte) uint32 {
	t.Helper()
	filter := datagramSeccompFilter()
	var a uint32
	for pc := 0; pc < len(filter); {
		ins := filter[pc]
		switch ins.Code {
		case bpfLD:
			off := int(ins.K)
			if off+4 > len(data) {
				t.Fatalf("BPF load out of bounds: pc=%d off=%d len=%d", pc, off, len(data))
			}
			a = binary.LittleEndian.Uint32(data[off : off+4])
			pc++
		case bpfJEQ:
			if a == ins.K {
				pc += 1 + int(ins.Jt)
			} else {
				pc += 1 + int(ins.Jf)
			}
		case unix.BPF_ALU | unix.BPF_AND | unix.BPF_K:
			a &= ins.K
			pc++
		case bpfRET:
			return ins.K
		default:
			t.Fatalf("unsupported BPF instruction 0x%x at pc=%d", ins.Code, pc)
		}
	}
	t.Fatalf("BPF program ran off the end without returning")
	return 0
}

// TestSecurityDatagramSeccompFilterMatrix asserts the production seccomp-BPF
// program returns the intended action for the full (domain, type, protocol)
// matrix the allowlist must cover, the three io_uring syscall numbers, and
// an unrelated syscall. This is a userspace BPF-interpreter simulation over
// the exact program applyDatagramSeccomp installs, so it pins every dispatch
// branch — including the AF_INET6 allow path, AF_UNIX/AF_NETLINK allow and
// deny paths, the SOCK_CLOEXEC/SOCK_NONBLOCK flag-stripping logic, and the
// non-socket pass-through — without depending on a kernel-enforced sandbox
// being runnable in CI. A regression in any branch flips at least one case.
func TestSecurityDatagramSeccompFilterMatrix(t *testing.T) {
	type c struct {
		name                   string
		nr, domain, typ, proto uint32
		wantAllow              bool
	}
	socket := uint32(unix.SYS_SOCKET)
	cases := []c{
		// io_uring family — denied at the head of the program.
		{"iouring-setup", unix.SYS_IO_URING_SETUP, 0, 0, 0, false},
		{"iouring-enter", unix.SYS_IO_URING_ENTER, 0, 0, 0, false},
		{"iouring-register", unix.SYS_IO_URING_REGISTER, 0, 0, 0, false},

		// Unrelated syscall — must pass through to ALLOW.
		{"read-passthrough", unix.SYS_READ, 0, 0, 0, true},

		// AF_UNIX — unrestricted.
		{"unix-stream", socket, syscall.AF_UNIX, syscall.SOCK_STREAM, 0, true},
		{"unix-dgram", socket, syscall.AF_UNIX, syscall.SOCK_DGRAM, 0, true},
		{"unix-seqpacket", socket, syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0, true},

		// AF_NETLINK — only SOCK_RAW/NETLINK_ROUTE allowed.
		{"netlink-raw-route", socket, syscall.AF_NETLINK, syscall.SOCK_RAW, unix.NETLINK_ROUTE, true},
		{"netlink-raw-nonroute", socket, syscall.AF_NETLINK, syscall.SOCK_RAW, unix.NETLINK_USERSOCK, false},
		{"netlink-dgram-route", socket, syscall.AF_NETLINK, syscall.SOCK_DGRAM, unix.NETLINK_ROUTE, false},
		{"netlink-stream-route", socket, syscall.AF_NETLINK, syscall.SOCK_STREAM, unix.NETLINK_ROUTE, false},

		// AF_INET — SOCK_STREAM with protocol 0/IPPROTO_TCP/IPPROTO_MPTCP only.
		{"inet-stream-default", socket, syscall.AF_INET, syscall.SOCK_STREAM, 0, true},
		{"inet-stream-tcp", socket, syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP, true},
		{"inet-stream-mptcp", socket, syscall.AF_INET, syscall.SOCK_STREAM, unix.IPPROTO_MPTCP, true},
		{"inet-stream-sctp", socket, syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_SCTP, false},
		{"inet-dgram-default", socket, syscall.AF_INET, syscall.SOCK_DGRAM, 0, false},
		{"inet-seqpacket-sctp", socket, syscall.AF_INET, syscall.SOCK_SEQPACKET, syscall.IPPROTO_SCTP, false},
		{"inet-raw", socket, syscall.AF_INET, syscall.SOCK_RAW, 0, false},

		// AF_INET6 — same allowlist as AF_INET; this branch had no pin before.
		{"inet6-stream-default", socket, syscall.AF_INET6, syscall.SOCK_STREAM, 0, true},
		{"inet6-stream-tcp", socket, syscall.AF_INET6, syscall.SOCK_STREAM, syscall.IPPROTO_TCP, true},
		{"inet6-stream-mptcp", socket, syscall.AF_INET6, syscall.SOCK_STREAM, unix.IPPROTO_MPTCP, true},
		{"inet6-stream-sctp", socket, syscall.AF_INET6, syscall.SOCK_STREAM, syscall.IPPROTO_SCTP, false},
		{"inet6-dgram-default", socket, syscall.AF_INET6, syscall.SOCK_DGRAM, 0, false},
		{"inet6-seqpacket-sctp", socket, syscall.AF_INET6, syscall.SOCK_SEQPACKET, syscall.IPPROTO_SCTP, false},
		{"inet6-raw", socket, syscall.AF_INET6, syscall.SOCK_RAW, 0, false},

		// SOCK_CLOEXEC / SOCK_NONBLOCK must be stripped before the type compare
		// on both the inet and netlink paths.
		{"inet-stream-cloexec", socket, syscall.AF_INET, syscall.SOCK_STREAM | syscall.SOCK_CLOEXEC, syscall.IPPROTO_TCP, true},
		{"inet-stream-nonblock", socket, syscall.AF_INET, syscall.SOCK_STREAM | syscall.SOCK_NONBLOCK, 0, true},
		{"inet6-stream-cloexec-nonblock", socket, syscall.AF_INET6, syscall.SOCK_STREAM | syscall.SOCK_CLOEXEC | syscall.SOCK_NONBLOCK, syscall.IPPROTO_TCP, true},
		{"netlink-raw-route-cloexec", socket, syscall.AF_NETLINK, syscall.SOCK_RAW | syscall.SOCK_CLOEXEC, unix.NETLINK_ROUTE, true},

		// AF_PACKET and an unhandled domain — denied.
		{"af-packet-raw", socket, syscall.AF_PACKET, syscall.SOCK_RAW, 0, false},
		{"af-bluetooth", socket, syscall.AF_BLUETOOTH, syscall.SOCK_STREAM, 0, false},
	}
	for _, k := range cases {
		data := seccompData(k.nr, k.domain, k.typ, k.proto)
		got := evalDatagramFilter(t, data)
		gotAllow := got == retAllow
		switch {
		case gotAllow && !k.wantAllow:
			t.Errorf("%s: filter allowed (action=0x%x), want EPERM", k.name, got)
		case !gotAllow && k.wantAllow:
			t.Errorf("%s: filter returned action=0x%x, want ALLOW", k.name, got)
		}
	}
}
