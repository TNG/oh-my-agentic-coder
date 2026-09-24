//go:build darwin

package sandboxrun

// DoctorNotes returns extra platform diagnostics for `omac doctor`.
func DoctorNotes(_ string) []string {
	return nil // sandbox-exec presence is covered by CheckPlatform
}
