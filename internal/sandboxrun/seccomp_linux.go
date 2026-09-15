//go:build linux

package sandboxrun

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// applyDatagramSeccomp installs a seccomp-BPF filter that denies
// socket(2) for AF_INET and AF_INET6 with SOCK_DGRAM or SOCK_RAW,
// blocking UDP/ICMP egress not coverable by Landlock TCP rules.
// Must be called after no_new_privs is set (ApplyLandlockNet does this).
func applyDatagramSeccomp() error {
	// seccomp_data offsets (kernel uapi/linux/seccomp.h):
	//   0:  nr   (u32) — syscall number
	//   16: args[0] (u64) — domain
	//   24: args[1] (u64) — type
	//
	// BPF_LD|BPF_W|BPF_ABS reads a u32 at the given offset.
	// For 64-bit args on little-endian hosts, offset 16 is the low 32
	// bits of args[0], offset 24 the low 32 bits of args[1] — sufficient
	// since AF_INET/AF_INET6 and SOCK_DGRAM/SOCK_RAW all fit in 16 bits.
	const (
		// seccomp_data field offsets.
		offNR   = 0
		offArg0 = 16 // domain
		offArg1 = 24 // type (SOCK_DGRAM=2, SOCK_RAW=3)

		// BPF instruction classes.
		bpfLD  = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		bpfJEQ = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		bpfRET = unix.BPF_RET | unix.BPF_K

		// SOCK_TYPE_MASK strips SOCK_CLOEXEC (0x80000) and SOCK_NONBLOCK
		// (0x800) from the type argument so comparisons hit the base type.
		sockTypeMask = 0xf

		retAllow = unix.SECCOMP_RET_ALLOW
		retEPERM = unix.SECCOMP_RET_ERRNO | uint32(syscall.EPERM)
	)

	af_inet := uint32(syscall.AF_INET)
	af_inet6 := uint32(syscall.AF_INET6)

	// Jump offsets: jt/jf are additional skips from the next instruction.
	// next = current+1+jN.
	//  0: load nr
	//  1: nr==SYS_SOCKET → 2; else → 10 (allow)
	//  2: load args[0] (domain)
	//  3: domain==AF_INET  → 5; else → 4
	//  4: domain==AF_INET6 → 5; else → 10 (allow)
	//  5: load args[1] (type)
	//  6: AND type with sockTypeMask
	//  7: type==SOCK_DGRAM → 9 (deny); else → 8
	//  8: type==SOCK_RAW   → 9 (deny); else → 10 (allow)
	//  9: return EPERM
	// 10: return ALLOW
	filter := []unix.SockFilter{
		// 0: load syscall number
		{Code: bpfLD, K: offNR},
		// 1: skip filter if not socket(); jf=8 → skip to 10
		{Code: bpfJEQ, Jt: 0, Jf: 8, K: uint32(unix.SYS_SOCKET)},
		// 2: load domain (args[0])
		{Code: bpfLD, K: offArg0},
		// 3: AF_INET → skip 1 to reach 5; else fall through to 4
		{Code: bpfJEQ, Jt: 1, Jf: 0, K: af_inet},
		// 4: AF_INET6 → skip 0 to reach 5; else skip 5 to reach 10
		{Code: bpfJEQ, Jt: 0, Jf: 5, K: af_inet6},
		// 5: load type (args[1])
		{Code: bpfLD, K: offArg1},
		// 6: AND with mask to strip SOCK_CLOEXEC / SOCK_NONBLOCK flags
		{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: sockTypeMask},
		// 7: SOCK_DGRAM → skip 1 to reach 9; else fall through to 8
		{Code: bpfJEQ, Jt: 1, Jf: 0, K: unix.SOCK_DGRAM},
		// 8: SOCK_RAW → skip 0 to reach 9; else skip 1 to reach 10
		{Code: bpfJEQ, Jt: 0, Jf: 1, K: unix.SOCK_RAW},
		// 9: deny
		{Code: bpfRET, K: retEPERM},
		// 10: allow
		{Code: bpfRET, K: retAllow},
	}

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
