package sandboxprofile_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

// Checking the path vm.InstanceRoot resolves to, not the literals in
// protectedCommon, catches a state root that moves without the baseline.
func TestSecurityVMStateRootIsProtected(t *testing.T) {
	for name, xdgState := range map[string]string{
		"XDG_STATE_HOME unset": "",
		"XDG_STATE_HOME set":   filepath.Join(t.TempDir(), "xdg-state"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_STATE_HOME", xdgState)
			for _, vmName := range []string{vm.DefaultVMName, "acme"} {
				root, err := vm.InstanceRoot(vmName)
				if err != nil {
					t.Fatal(err)
				}
				for platform, b := range map[string]sandboxprofile.Baseline{
					"linux":  sandboxprofile.LinuxBaselineForTest(),
					"darwin": sandboxprofile.DarwinBaselineForTest(),
				} {
					if !coveredBy(root, sandboxprofile.EffectiveProtectedPaths(b, nil)) {
						t.Errorf("%s baseline does not protect the state root %s of VM %q: the sandbox could read admin_key", platform, root, vmName)
					}
				}
			}
		})
	}
}

func coveredBy(path string, protected []string) bool {
	for _, p := range protected {
		if path == p || strings.HasPrefix(path, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
