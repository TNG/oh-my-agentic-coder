// Package osinfo provides OS detection for selecting install scripts.
package osinfo

import (
	"os"
	"runtime"
	"strings"
)

// OS is the normalized operating-system identifier used by omac.
type OS string

const (
	MacOS   OS = "macos"
	Linux   OS = "linux"
	WSL     OS = "wsl"
	Unknown OS = "unknown"
)

// Detect returns the normalized OS of the host.
//
// WSL is detected by inspecting /proc/version for the string "microsoft";
// this matches install.sh's heuristic at the repository root.
func Detect() OS {
	switch runtime.GOOS {
	case "darwin":
		return MacOS
	case "linux":
		data, err := os.ReadFile("/proc/version")
		if err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft") {
			return WSL
		}
		return Linux
	default:
		return Unknown
	}
}

// String implements fmt.Stringer.
func (o OS) String() string { return string(o) }
