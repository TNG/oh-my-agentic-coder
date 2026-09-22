//go:build linux

package sandboxrun

import (
	"fmt"

	"github.com/TNG/oh-my-agentic-coder/internal/osinfo"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// DoctorNotes returns extra platform diagnostics for `omac doctor`. profileRef
// is the policy profile the run would enforce (sandbox.profile_path, else the
// built-in "default"), so the network-enforcement note reflects the real config.
func DoctorNotes(profileRef string) []string {
	abi := LandlockABI()
	if abi >= landlockNetABI {
		notes := []string{fmt.Sprintf("[ok] Landlock ABI %d (TCP network rules supported; datagram blocked via seccomp)", abi)}
		// Report the TIOCSTI sysctl as informational: --new-session is the
		// primary fix, but older kernels also lack the sysctl fallback.
		if v, ok := procUint("/proc/sys/dev/tty/legacy_tiocsti"); ok {
			if v == 0 {
				notes = append(notes, "[ok] dev.tty.legacy_tiocsti=0 (TIOCSTI disabled by kernel; --new-session also enforced)")
			} else {
				notes = append(notes, "[warn] dev.tty.legacy_tiocsti=1 (sysctl allows TIOCSTI; --new-session prevents injection)")
			}
		} else {
			notes = append(notes, "[ok] dev.tty.legacy_tiocsti absent (kernel < 6.2 or not configurable; --new-session prevents injection)")
		}
		return notes
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
