//go:build !darwin && !linux

package sandboxrun

// sampleOpenDirs is unsupported on this platform.
func sampleOpenDirs(_ int) []string { return nil }
