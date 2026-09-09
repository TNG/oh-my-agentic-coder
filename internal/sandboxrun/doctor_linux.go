//go:build linux

package sandboxrun

import (
	"fmt"

	"github.com/TNG/oh-my-agentic-coder/internal/osinfo"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// DoctorNotes returns extra platform diagnostics for `omac doctor`.
func DoctorNotes() []string {
	abi := LandlockABI()
	if abi >= landlockNetABI {
		return []string{fmt.Sprintf("[ok] Landlock ABI %d (network rules supported)", abi)}
	}
	envOnlyActive := false
	if p, _, err := sandboxprofile.Resolve(""); err == nil {
		envOnlyActive = p.Network.EffectiveEnforcement() == sandboxprofile.EnforceEnvOnly
	}
	if envOnlyActive {
		return []string{fmt.Sprintf(
			"[warn] Landlock ABI %d < %d (kernel < 6.7): network.enforcement is already \"env-only\" — advisory filtering active",
			abi, landlockNetABI)}
	}
	return []string{
		fmt.Sprintf(
			"[warn] Landlock ABI %d (%s): network enforcement needs ABI %d (Linux >= 6.7,"+
				" e.g. Ubuntu 24.04 LTS, Fedora 40+); omac start will fail with the default profile.",
			abi, kernelVersionString(), landlockNetABI),
		landlockFixA(osinfo.Detect()),
		"       Fix B: set enforcement to env-only in ~/.config/omac/sandbox-profiles/default.json:",
		`         {"network": {"enforcement": "env-only"}}`,
		"       (env-only: filtering via the omac proxy, not the kernel — advisory only)",
	}
}

func landlockFixA(host osinfo.OS) string {
	if host == osinfo.WSL {
		return "       Fix A: run `wsl --update` from Windows PowerShell to get a newer WSL kernel (>= 6.7),\n" +
			"       then restart your WSL session."
	}
	return "       Fix A: upgrade to a kernel >= 6.7."
}
