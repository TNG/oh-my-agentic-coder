package cli

import (
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/keychain"
	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
)

// keychainUnavailableHint returns an actionable, OS-specific tip for a
// missing Secret Service backend, appended to the raw D-Bus/keyring error
// so a user isn't left with just "org.freedesktop.secrets was not provided
// by any .service files". See docs/INSTALLATION.md#prerequisites.
func keychainUnavailableHint(host osinfo.OS) string {
	if host == osinfo.WSL {
		return "WSL has no Secret Service by default; install one and create a keyring once: " +
			"`sudo apt install -y gnome-keyring seahorse`, then run seahorse to create a " +
			"password keyring and set it as default " +
			"(see docs/INSTALLATION.md#prerequisites)"
	}
	return "no Secret Service provider found — install and start one (e.g. gnome-keyring or kwalletd), " +
		"or set DBUS_SESSION_BUS_ADDRESS if one is already running (see docs/INSTALLATION.md#prerequisites)"
}

// wrapKeychainErr appends keychainUnavailableHint to errors caused by a
// missing keychain backend (keychain.IsUnavailable), leaving per-secret
// errors (deleted item, permission denied, etc.) untouched.
func wrapKeychainErr(err error, host osinfo.OS) error {
	if err == nil || !keychain.IsUnavailable(err) {
		return err
	}
	return fmt.Errorf("%w — %s", err, keychainUnavailableHint(host))
}
