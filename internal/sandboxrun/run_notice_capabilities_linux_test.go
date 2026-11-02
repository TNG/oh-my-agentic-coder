//go:build linux

package sandboxrun

import (
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func requireRunNoticeCapabilities(t *testing.T, profile *sandboxprofile.Profile) {
	t.Helper()
	requireBwrap(t)

	needsLandlock := profile.Network.EffectiveMode() == sandboxprofile.ModeBlocked ||
		(profile.Network.EffectiveMode() == sandboxprofile.ModeFiltered && profile.Network.EffectiveEnforcement() == sandboxprofile.EnforceKernel)
	if needsLandlock && !LandlockNetSupported() {
		skipOrFailCI(t, "Landlock ABI %d < 4: launch notice test requires kernel network enforcement", LandlockABI())
	}
}
