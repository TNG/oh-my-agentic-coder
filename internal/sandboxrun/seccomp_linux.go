//go:build linux

package sandboxrun

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// applyDatagramSeccomp installs a seccomp-BPF filter that confines socket
// creation to a TCP-only allowlist and denies the io_uring syscall family,
// closing the non-TCP egress path that Landlock (TCP-only) cannot express.
// Must be called after no_new_privs is set (ApplyLandlockNet does this).
//
// The filter is an allowlist over socket(2): AF_UNIX is unrestricted;
// AF_NETLINK is limited to SOCK_RAW/NETLINK_ROUTE; AF_INET and AF_INET6
// are limited to SOCK_STREAM with protocol 0, IPPROTO_TCP or IPPROTO_MPTCP.
// Every other socket(2) combination (SCTP, SOCK_SEQPACKET, SOCK_RAW,
// AF_PACKET, unrestricted AF_NETLINK, ...) returns EPERM. The three
// io_uring syscalls are denied at the head of the program so a socket
// cannot be created via IORING_OP_SOCKET, which never reaches socket(2).
// All unrelated syscalls pass through.

// seccomp_data field offsets (kernel uapi/linux/seccomp.h):
//
//	0:  nr        (u32) — syscall number
//	4:  arch      (u32) — syscall ABI
//	16: args[0]   (u64) — socket domain
//	24: args[1]   (u64) — socket type
//	32: args[2]   (u64) — socket protocol
//
// BPF_LD|BPF_W|BPF_ABS reads a u32 at the given offset; on little-endian
// hosts the low 32 bits live at the field offset, which is sufficient
// since every value compared here fits in 16 bits.
const (
	offNR   = 0
	offArch = 4  // syscall ABI
	offArg0 = 16 // domain
	offArg1 = 24 // type
	offArg2 = 32 // protocol

	bpfLD  = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
	bpfJEQ = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
	bpfRET = unix.BPF_RET | unix.BPF_K

	// sockTypeMask strips SOCK_CLOEXEC (0x80000) and SOCK_NONBLOCK
	// (0x800) from the type argument so comparisons hit the base type.
	sockTypeMask = 0xf

	retAllow = unix.SECCOMP_RET_ALLOW
	retEPERM = unix.SECCOMP_RET_ERRNO | uint32(syscall.EPERM)
)

// datagramSeccompFilter returns the seccomp-BPF program installed by
// applyDatagramSeccomp. It is a separate function so the regression suite
// can load the exact production program into a userspace BPF walker and
// assert the action it returns for every (domain, type, protocol) the
// allowlist is supposed to cover, plus the io_uring numbers and unrelated
// syscalls — without depending on a kernel-enforced sandbox being runnable
// in CI.
func datagramSeccompFilter() []unix.SockFilter {
	// Program layout (jt/jf are skips from the next instruction):
	//  0: load arch
	//  1: arch == native → 2; else → deny (26)
	//  2: load nr
	//  3: nr carries a non-native encoding → deny (26); else fall through
	//  4-6: nr == io_uring_setup/enter/register → deny (26)
	//  7: nr == socket → 8; else → allow (25)
	//  8: load domain
	//  9: AF_UNIX → allow (25)
	// 10: AF_NETLINK → 13 (netlink check); else fall through
	// 11: AF_INET → 18 (inet check); else fall through
	// 12: AF_INET6 → 18 (inet check); else → deny (26)
	// 13-15: netlink: type==SOCK_RAW → 16; else → deny (26)
	// 16-17: protocol==NETLINK_ROUTE → allow (25); else → deny (26)
	// 18-20: inet: type==SOCK_STREAM → 21; else → deny (26)
	// 21-24: protocol in {0, IPPROTO_TCP, IPPROTO_MPTCP} → allow (25); else → deny (26)
	// 25: allow
	// 26: deny

	filter := []unix.SockFilter{
		// 0: load syscall ABI
		{Code: bpfLD, K: offArch},
		// 1: only the build's native ABI continues; every other value is denied
		{Code: bpfJEQ, Jt: 0, Jf: 24, K: nativeSyscallArch},
		// 2: load syscall number
		{Code: bpfLD, K: offNR},
		// 3: numbers with a non-native encoding are denied
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jt: 22, Jf: 0, K: syscallNumberReservedMask},
		// 4: io_uring_setup → deny
		{Code: bpfJEQ, Jt: 21, Jf: 0, K: uint32(unix.SYS_IO_URING_SETUP)},
		// 5: io_uring_enter → deny
		{Code: bpfJEQ, Jt: 20, Jf: 0, K: uint32(unix.SYS_IO_URING_ENTER)},
		// 6: io_uring_register → deny
		{Code: bpfJEQ, Jt: 19, Jf: 0, K: uint32(unix.SYS_IO_URING_REGISTER)},
		// 7: socket → continue; else allow
		{Code: bpfJEQ, Jt: 0, Jf: 17, K: uint32(unix.SYS_SOCKET)},
		// 8: load domain (args[0])
		{Code: bpfLD, K: offArg0},
		// 9: AF_UNIX → allow
		{Code: bpfJEQ, Jt: 15, Jf: 0, K: uint32(syscall.AF_UNIX)},
		// 10: AF_NETLINK → netlink check (13)
		{Code: bpfJEQ, Jt: 2, Jf: 0, K: uint32(syscall.AF_NETLINK)},
		// 11: AF_INET → inet check (18)
		{Code: bpfJEQ, Jt: 6, Jf: 0, K: uint32(syscall.AF_INET)},
		// 12: AF_INET6 → inet check (18); else deny
		{Code: bpfJEQ, Jt: 5, Jf: 13, K: uint32(syscall.AF_INET6)},
		// 13: load type (args[1])
		{Code: bpfLD, K: offArg1},
		// 14: strip SOCK_CLOEXEC / SOCK_NONBLOCK
		{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: sockTypeMask},
		// 15: SOCK_RAW → 16; else deny
		{Code: bpfJEQ, Jt: 0, Jf: 10, K: unix.SOCK_RAW},
		// 16: load protocol (args[2])
		{Code: bpfLD, K: offArg2},
		// 17: NETLINK_ROUTE → allow; else deny
		{Code: bpfJEQ, Jt: 7, Jf: 8, K: uint32(unix.NETLINK_ROUTE)},
		// 18: load type (args[1])
		{Code: bpfLD, K: offArg1},
		// 19: strip SOCK_CLOEXEC / SOCK_NONBLOCK
		{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: sockTypeMask},
		// 20: SOCK_STREAM → 21; else deny
		{Code: bpfJEQ, Jt: 0, Jf: 5, K: unix.SOCK_STREAM},
		// 21: load protocol (args[2])
		{Code: bpfLD, K: offArg2},
		// 22: protocol == 0 → allow
		{Code: bpfJEQ, Jt: 2, Jf: 0, K: 0},
		// 23: protocol == IPPROTO_TCP → allow
		{Code: bpfJEQ, Jt: 1, Jf: 0, K: unix.IPPROTO_TCP},
		// 24: protocol == IPPROTO_MPTCP → allow; else deny
		{Code: bpfJEQ, Jt: 0, Jf: 1, K: unix.IPPROTO_MPTCP},
		// 25: allow
		{Code: bpfRET, K: retAllow},
		// 26: deny
		{Code: bpfRET, K: retEPERM},
	}
	return filter
}

func applyDatagramSeccomp() error {
	filter := datagramSeccompFilter()

	prog := &unix.SockFprog{
		Len:    uint16(len(filter)),
		Filter: &filter[0],
	}

	// restrict_self already set no_new_privs via ApplyLandlockNet; the
	// prctl below only needs it set, which it already is.  Lock the OS
	// thread so the syscall is issued from the thread that will exec.
	runtime.LockOSThread()
	if _, _, errno := unix.Syscall(unix.SYS_PRCTL,
		unix.PR_SET_SECCOMP,
		unix.SECCOMP_MODE_FILTER,
		uintptr(unsafe.Pointer(prog)),
	); errno != 0 {
		return fmt.Errorf("seccomp(FILTER): %w", errno)
	}
	return nil
}
