//go:build linux

package sandboxrun

import (
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/osinfo"
)

func TestLandlockFixA(t *testing.T) {
	if got := landlockFixA(osinfo.WSL); !strings.Contains(got, "wsl --update") {
		t.Errorf("WSL fix A = %q, want it to mention `wsl --update`", got)
	}
	if got := landlockFixA(osinfo.Linux); strings.Contains(got, "wsl") {
		t.Errorf("Linux fix A = %q, should not be WSL-specific", got)
	}
}
