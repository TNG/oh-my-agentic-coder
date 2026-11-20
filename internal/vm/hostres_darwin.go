//go:build darwin

package vm

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// DetectHost reads hw.memsize; a failed sysctl leaves memory unknown (0).
func DetectHost() HostResources {
	h := HostResources{CPUs: runtime.NumCPU()}
	if mem, err := unix.SysctlUint64("hw.memsize"); err == nil {
		h.MemoryBytes = mem
	}
	return h
}
