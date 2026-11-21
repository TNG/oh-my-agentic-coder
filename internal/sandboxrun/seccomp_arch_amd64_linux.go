//go:build linux && amd64

package sandboxrun

import "golang.org/x/sys/unix"

const (
	nativeSyscallArch = unix.AUDIT_ARCH_X86_64

	// syscallNumberReservedMask selects the alternate number encoding the
	// kernel still recognizes when seccomp_data.arch reports the native ABI.
	syscallNumberReservedMask = 0x40000000
)
