package cli

import (
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
)

// keychainUnavailableHint returns an actionable, OS-specific tip for a
// missing Secret Service backend, appended to the raw D-Bus/keyring error
// so a user isn't left with just "org.freedesktop.secrets was not provided
// by any .service files". See README.md#prerequisites.
func keychainUnavailableHint(host osinfo.OS) string {
	if host == osinfo.WSL {
		return "no Secret Service provider found — WSL doesn't ship one by default; " +
			"install and start gnome-keyring once per session: " +
			`sudo apt install gnome-keyring dbus-x11 && eval "$(dbus-launch --sh-syntax)" && ` +
			"gnome-keyring-daemon --unlock --components=secrets (see README.md#prerequisites)"
	}
	return "no Secret Service provider found — install and start one (e.g. gnome-keyring or kwalletd), " +
		"or set DBUS_SESSION_BUS_ADDRESS if one is already running (see README.md#prerequisites)"
}

// wrapKeychainErr appends keychainUnavailableHint to errors caused by a
// missing keychain backend (keychain.IsUnavailable), leaving per-secret
// errors (deleted item, permission denied, etc.) untouched.
func wrapKeychainErr(err error) error {
	if err == nil || !keychain.IsUnavailable(err) {
		return err
	}
	return fmt.Errorf("%w — %s", err, keychainUnavailableHint(osinfo.Detect()))
}
