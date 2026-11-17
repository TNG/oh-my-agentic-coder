package sandboxprofile

import (
	"slices"
	"testing"
)

// The omac VM state directory holds admin_key, root access to the VM. Both
// spellings must stay protected on every platform.
func TestProtectedCommonCoversVMState(t *testing.T) {
	for _, want := range []string{"~/.local/state/omac/vm", "$XDG_STATE_HOME/omac/vm"} {
		if !slices.Contains(protectedCommon(), want) {
			t.Errorf("protectedCommon() lacks %q", want)
		}
		if !slices.Contains(PlatformBaseline().ProtectedPaths, want) {
			t.Errorf("PlatformBaseline().ProtectedPaths lacks %q", want)
		}
	}
}
