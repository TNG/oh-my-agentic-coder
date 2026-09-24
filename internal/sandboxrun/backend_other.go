//go:build !darwin && !linux

package sandboxrun

import "fmt"

// CheckPlatform: the built-in sandbox supports macOS and Linux only.
func CheckPlatform() error {
	return fmt.Errorf("the built-in sandbox supports macOS (Seatbelt) and Linux (bubblewrap) only")
}

// BuildChildArgv is unsupported on this platform.
func BuildChildArgv(_ *Grants, _ []string) ([]string, error) {
	return nil, CheckPlatform()
}

// DoctorNotes returns extra platform diagnostics for `omac doctor`.
func DoctorNotes(_ string) []string { return nil }
