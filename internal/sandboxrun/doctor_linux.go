//go:build linux

package sandboxrun

import "fmt"

// DoctorNotes returns extra platform diagnostics for `omac doctor`.
func DoctorNotes() []string {
	abi := LandlockABI()
	if abi >= landlockNetABI {
		return []string{fmt.Sprintf("[ok] Landlock ABI %d (network rules supported)", abi)}
	}
	return []string{
		fmt.Sprintf(
			"[warn] Landlock ABI %d (%s): network enforcement needs ABI %d (Linux >= 6.7,"+
				" e.g. Ubuntu 24.04 LTS, Fedora 40+); omac start will fail with the default profile.",
			abi, kernelVersionString(), landlockNetABI),
		"       Fix A: upgrade to a kernel >= 6.7.",
		"       Fix B: set enforcement to env-only in ~/.config/omac/sandbox-profiles/default.json:",
		`         {"network": {"enforcement": "env-only"}}`,
		"       (env-only: filtering via the omac proxy, not the kernel — advisory only)",
	}
}
