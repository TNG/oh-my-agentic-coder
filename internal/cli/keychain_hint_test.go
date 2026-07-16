package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
)

// TestWrapKeychainErrOnlyHintsBackendFailures ensures the hint is attached
// only for missing-backend errors (keychain.IsUnavailable), not for
// genuine per-secret failures — a wrong hint on the latter would mislead a
// user whose Secret Service is actually running fine.
func TestWrapKeychainErrOnlyHintsBackendFailures(t *testing.T) {
	unavailable := errors.New(`keychain set omac/slack/TOKEN: The name org.freedesktop.secrets was not provided by any .service files`)
	if got := wrapKeychainErr(unavailable); !strings.Contains(got.Error(), "Secret Service") {
		t.Errorf("wrapKeychainErr(unavailable) = %q, want it to contain a hint", got)
	}

	genuine := errors.New("permission denied")
	if got := wrapKeychainErr(genuine); got.Error() != genuine.Error() {
		t.Errorf("wrapKeychainErr(genuine) = %q, want unchanged %q", got, genuine)
	}

	if got := wrapKeychainErr(nil); got != nil {
		t.Errorf("wrapKeychainErr(nil) = %v, want nil", got)
	}
}

// TestKeychainUnavailableHintMentionsWSLSetup checks the WSL hint actually
// names the fix (gnome-keyring) rather than just restating the symptom.
func TestKeychainUnavailableHintMentionsWSLSetup(t *testing.T) {
	if got := keychainUnavailableHint(osinfo.WSL); !strings.Contains(got, "gnome-keyring") {
		t.Errorf("WSL hint = %q, want it to mention gnome-keyring", got)
	}
	if got := keychainUnavailableHint(osinfo.Linux); strings.Contains(got, "WSL") {
		t.Errorf("Linux hint = %q, should not claim to be WSL-specific", got)
	}
}
