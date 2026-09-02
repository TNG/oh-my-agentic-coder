//go:build linux

package sandboxrun

import (
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// DoctorNotes returns extra platform diagnostics for `omac doctor`. profileRef
// is the policy profile the run would enforce (sandbox.profile_path, else the
// built-in "default"), so the network-enforcement note reflects the real config.
func DoctorNotes(profileRef string) []string {
	abi := LandlockABI()
	if abi >= landlockNetABI {
		return []string{fmt.Sprintf("[ok] Landlock ABI %d (network rules supported)", abi)}
	}
	envOnlyActive := false
	if p, _, err := sandboxprofile.Resolve(profileRef); err == nil {
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
		"       Fix A: upgrade to a kernel >= 6.7.",
		"       Fix B: set enforcement to env-only in ~/.config/omac/sandbox-profiles/default.json:",
		`         {"network": {"enforcement": "env-only"}}`,
		"       (env-only: filtering via the omac proxy, not the kernel — advisory only)",
	}
}
