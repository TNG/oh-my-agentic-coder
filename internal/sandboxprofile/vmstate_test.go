package sandboxprofile_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/vm"
)

// Every VM state directory holds admin_key, root access to that VM. The
// check runs against vm.InstanceRoot, so a change of the state layout that
// the protected paths miss fails here.
func TestProtectedPathsCoverEveryVMStateDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		name, xdg string
		pin       bool
	}{
		{"XDG_STATE_HOME unset", "", false},
		{"XDG_STATE_HOME set", filepath.Join(t.TempDir(), "state"), false},
		{"pinned by serve --for-opencode-desktop", filepath.Join(t.TempDir(), "desktop"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", c.xdg)
			if c.pin {
				defer vm.PinStateHome("")()
			}
			var protected []string
			for _, p := range sandboxprofile.PlatformBaseline().ProtectedPaths {
				if e, err := sandboxprofile.ExpandPath(p); err == nil {
					protected = append(protected, e)
				}
			}
			for _, name := range []string{vm.DefaultVMName, "acme"} {
				root, err := vm.InstanceRoot(name)
				if err != nil {
					t.Fatal(err)
				}
				covered := slices.ContainsFunc(protected, func(p string) bool {
					return root == p || strings.HasPrefix(root, p+string(filepath.Separator))
				})
				if !covered {
					t.Errorf("state directory %s of VM %q is not protected", root, name)
				}
			}
		})
	}
}
