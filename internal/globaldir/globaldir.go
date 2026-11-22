// Package globaldir locates the user-global omac config directory, which the
// sandbox can neither read nor write.
package globaldir

import (
	"os"
	"path/filepath"
)

// Dir returns $XDG_CONFIG_HOME/omac, else ~/.config/omac, or "" when
// neither can be determined.
func Dir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "omac")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "omac")
}
