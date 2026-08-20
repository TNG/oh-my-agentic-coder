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
	if got := wrapKeychainErr(unavailable, osinfo.Linux); !strings.Contains(got.Error(), "Secret Service") {
		t.Errorf("wrapKeychainErr(unavailable) = %q, want it to contain a hint", got)
	}

	genuine := errors.New("permission denied")
	if got := wrapKeychainErr(genuine, osinfo.Linux); got.Error() != genuine.Error() {
		t.Errorf("wrapKeychainErr(genuine) = %q, want unchanged %q", got, genuine)
	}

	if got := wrapKeychainErr(nil, osinfo.Linux); got != nil {
		t.Errorf("wrapKeychainErr(nil) = %v, want nil", got)
	}
}

// TestWrapKeychainErrWSLHint verifies that the WSL-specific dispatch path in
// wrapKeychainErr fires correctly — the branch that osinfo.Detect() owned
// was untestable without a real WSL environment before this refactor.
func TestWrapKeychainErrWSLHint(t *testing.T) {
	unavailable := errors.New(`keychain set omac/slack/TOKEN: The name org.freedesktop.secrets was not provided by any .service files`)
	if got := wrapKeychainErr(unavailable, osinfo.WSL); !strings.Contains(got.Error(), "seahorse") {
		t.Errorf("WSL hint missing seahorse: %q", got)
	}
	if got := wrapKeychainErr(unavailable, osinfo.WSL); strings.Contains(got.Error(), "dbus-launch") {
		t.Errorf("WSL hint should not mention dbus-launch: %q", got)
	}
	if got := wrapKeychainErr(unavailable, osinfo.Linux); strings.Contains(got.Error(), "seahorse") {
		t.Errorf("Linux hint should not mention seahorse: %q", got)
	}
}

// TestWrapKeychainErrBootstrapHint verifies the hint fires for the
// "failed to unlock correct collection" error — the fresh-WSL2 bootstrap
// gap where the daemon is running but no keyring exists yet.
func TestWrapKeychainErrBootstrapHint(t *testing.T) {
	bootstrap := errors.New(`keychain set omac/skill-marketplace/ASML_BEARER_TOKEN: failed to unlock correct collection '/org/freedesktop/secrets/aliases/default'`)
	if got := wrapKeychainErr(bootstrap, osinfo.WSL); !strings.Contains(got.Error(), "seahorse") {
		t.Errorf("bootstrap hint missing seahorse: %q", got)
	}
	if got := wrapKeychainErr(bootstrap, osinfo.Linux); !strings.Contains(got.Error(), "Secret Service") {
		t.Errorf("non-WSL bootstrap hint should still mention Secret Service: %q", got)
	}
}

// TestKeychainUnavailableHintMentionsWSLSetup checks the WSL hint actually
// names the fix (seahorse) rather than just restating the symptom.
func TestKeychainUnavailableHintMentionsWSLSetup(t *testing.T) {
	if got := keychainUnavailableHint(osinfo.WSL); !strings.Contains(got, "seahorse") {
		t.Errorf("WSL hint = %q, want it to mention seahorse", got)
	}
	if got := keychainUnavailableHint(osinfo.Linux); strings.Contains(got, "WSL") {
		t.Errorf("Linux hint = %q, should not claim to be WSL-specific", got)
	}
}
