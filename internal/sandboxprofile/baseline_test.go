package sandboxprofile

import (
	"slices"
	"testing"
)

func TestSharedOpenCodePTYDirIsProtected(t *testing.T) {
	dir := SharedOpenCodePTYDir()
	if !slices.Contains(EffectiveProtectedPaths(PlatformBaseline(), nil), dir) {
		t.Errorf("%s is not a protected path", dir)
	}
	if slices.Contains(EffectiveProtectedPaths(PlatformBaseline(), []string{dir}), dir) {
		t.Errorf("override_deny of %s does not remove the protection", dir)
	}
}
