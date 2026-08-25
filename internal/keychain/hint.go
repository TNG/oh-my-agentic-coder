package keychain

import (
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
)

// UnavailableHint returns an actionable, OS-specific tip for a missing Secret
// Service backend, appended to the raw D-Bus/keyring error so a user isn't
// left with just "org.freedesktop.secrets was not provided by any .service
// files". See docs/INSTALLATION.md#prerequisites.
//
// It lives here rather than in the CLI so every path that can hit a dead
// backend — register and `secrets set` on the write side, `start`/`serve`/
// reload/`doctor` via internal/skillstate on the read side — renders the same
// remedy. Issue #174 was in part a bug about this text existing on one path
// and not the other.
func UnavailableHint(host osinfo.OS) string {
	if host == osinfo.WSL {
		return "no Secret Service provider found — WSL doesn't ship one by default; " +
			"install and start gnome-keyring once per session: " +
			`sudo apt install gnome-keyring dbus-x11 && eval "$(dbus-launch --sh-syntax)" && ` +
			"gnome-keyring-daemon --unlock --components=secrets (see docs/INSTALLATION.md#prerequisites)"
	}
	return "no Secret Service provider found — install and start one (e.g. gnome-keyring or kwalletd), " +
		"or set DBUS_SESSION_BUS_ADDRESS if one is already running (see docs/INSTALLATION.md#prerequisites)"
}

// WrapUnavailable appends UnavailableHint to errors caused by a missing
// keychain backend, leaving per-secret errors (deleted item, permission
// denied, etc.) untouched. It detects both a raw backend error (IsUnavailable,
// i.e. what the write path sees) and one already classified as ErrUnavailable
// by the read path.
func WrapUnavailable(err error) error {
	if err == nil || !isUnavailableErr(err) {
		return err
	}
	return fmt.Errorf("%w — %s", err, UnavailableHint(osinfo.Detect()))
}
