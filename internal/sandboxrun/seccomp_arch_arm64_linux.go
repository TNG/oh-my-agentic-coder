//go:build linux && arm64

package sandboxrun

import "golang.org/x/sys/unix"

const (
	nativeSyscallArch = unix.AUDIT_ARCH_AARCH64

	// aarch64 has no alternate syscall-number encoding.
	syscallNumberReservedMask uint32 = 0
)
