//go:build !darwin && !linux

package vm

import "runtime"

// DetectHost knows only the CPU count here; memory stays unknown (0).
func DetectHost() HostResources {
	return HostResources{CPUs: runtime.NumCPU()}
}
