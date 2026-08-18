package keychain

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/osinfo"
)

// TestWrapUnavailableOnlyHintsBackendFailures ensures the hint is attached
// only for missing-backend errors (keychain.IsUnavailable), not for
// genuine per-secret failures — a wrong hint on the latter would mislead a
// user whose Secret Service is actually running fine.
func TestWrapUnavailableOnlyHintsBackendFailures(t *testing.T) {
	unavailable := errors.New(`keychain set omac/slack/TOKEN: The name org.freedesktop.secrets was not provided by any .service files`)
	if got := WrapUnavailable(unavailable); !strings.Contains(got.Error(), "Secret Service") {
		t.Errorf("WrapUnavailable(unavailable) = %q, want it to contain a hint", got)
	}

	genuine := errors.New("permission denied")
	if got := WrapUnavailable(genuine); got.Error() != genuine.Error() {
		t.Errorf("WrapUnavailable(genuine) = %q, want unchanged %q", got, genuine)
	}

	if got := WrapUnavailable(nil); got != nil {
		t.Errorf("WrapUnavailable(nil) = %v, want nil", got)
	}
}

// TestWrapUnavailableHintsClassifiedReadErrors covers the read path, whose
// errors no longer carry the raw backend text IsUnavailable sniffs for — they
// carry the ErrUnavailable sentinel instead (see GetScoped). Without this the
// hint would silently stop appearing on the launch path, which is the
// regression issue #174 called Failure 4.
func TestWrapUnavailableHintsClassifiedReadErrors(t *testing.T) {
	readErr := fmt.Errorf("%w: %w: %v", ErrNotFound, ErrUnavailable, errors.New("dbus: no session bus"))
	got := WrapUnavailable(readErr)
	if !strings.Contains(got.Error(), "Secret Service") {
		t.Errorf("WrapUnavailable(classified) = %q, want it to contain a hint", got)
	}
	// Wrapping must not destroy either sentinel for downstream callers.
	if !errors.Is(got, ErrUnavailable) || !errors.Is(got, ErrNotFound) {
		t.Errorf("WrapUnavailable(classified) = %q, want both sentinels preserved", got)
	}
	if got := WrapUnavailable(ErrNotFound); got.Error() != ErrNotFound.Error() {
		t.Errorf("WrapUnavailable(ErrNotFound) = %q, want unchanged — a plain absent secret is not a broken backend", got)
	}
}

// TestUnavailableHintMentionsWSLSetup checks the WSL hint actually
// names the fix (gnome-keyring) rather than just restating the symptom.
func TestUnavailableHintMentionsWSLSetup(t *testing.T) {
	if got := UnavailableHint(osinfo.WSL); !strings.Contains(got, "gnome-keyring") {
		t.Errorf("WSL hint = %q, want it to mention gnome-keyring", got)
	}
	if got := UnavailableHint(osinfo.Linux); strings.Contains(got, "WSL") {
		t.Errorf("Linux hint = %q, should not claim to be WSL-specific", got)
	}
}
