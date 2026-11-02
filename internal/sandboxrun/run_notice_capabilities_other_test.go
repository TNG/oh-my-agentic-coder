//go:build !linux

package sandboxrun

import (
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func requireRunNoticeCapabilities(t *testing.T, _ *sandboxprofile.Profile) {
	t.Helper()
}
