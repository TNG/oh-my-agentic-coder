package keychain

import (
	"errors"
	"fmt"
	"testing"
)

// TestScopedServiceNaming locks the keychain service-name scheme so the
// write side (register/secrets set) and the read side (start/serve) can
// never silently disagree about where a secret lives.
func TestScopedServiceNaming(t *testing.T) {
	cases := []struct {
		scope, skill, want string
	}{
		{"", "slack", "omac/slack"},                         // unscoped / global
		{"abc123", "slack", "omac/abc123/slack"},            // workdir-scoped
		{DefaultsScope, "slack", "omac/__defaults__/slack"}, // remembered defaults
	}
	for _, c := range cases {
		if got := ScopedService(c.scope, c.skill); got != c.want {
			t.Errorf("ScopedService(%q,%q) = %q, want %q", c.scope, c.skill, got, c.want)
		}
	}
}

// TestWorkdirIDDeterministicAndDistinct ensures the workdir-id used as the
// secret scope is stable per path and distinct across paths.
func TestWorkdirIDDeterministicAndDistinct(t *testing.T) {
	a1 := WorkdirID("/Users/me/projects/acme")
	a2 := WorkdirID("/Users/me/projects/acme")
	b := WorkdirID("/Users/me/clients/acme")
	if a1 != a2 {
		t.Errorf("WorkdirID not deterministic: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Error("WorkdirID collided for different paths sharing a basename")
	}
	if a1 == "" {
		t.Error("WorkdirID returned empty")
	}
}

// TestIsUnavailable locks the classification used to attach a WSL/headless
// hint on the write path (see WrapUnavailable) vs. leaving genuine
// per-secret errors (permission denied, corrupt entry, ...) untouched.
func TestIsUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dbus service unknown", errors.New(`keychain set omac/slack/TOKEN: The name org.freedesktop.secrets was not provided by any .service files`), true},
		{"no dbus session", errors.New("dbus: could not connect: no such file or directory"), true},
		{"D-Bus capitalized", errors.New("D-Bus connection failed"), true},
		// go-keyring's real error when DBUS_SESSION_BUS_ADDRESS names a
		// torn-down socket (WSL2 / Linger=no): a bare dial failure with no
		// dbus marker, so the checks above don't catch it.
		{"dead bus socket removed", errors.New("keychain get omac/slack/TOKEN: dial unix /run/user/1000/bus: connect: no such file or directory"), true},
		{"dead bus socket refused", errors.New("dial unix /run/user/1000/bus: connect: connection refused"), true},
		// go-keyring's real error on a fresh WSL2 install: the daemon is
		// running but no keyring collection exists yet, so GetLoginCollection
		// falls back to the alias path and Unlock returns zero unlocked paths.
		{"no keyring created", errors.New(`keychain set omac/skill-marketplace/ASML_BEARER_TOKEN: failed to unlock correct collection '/org/freedesktop/secrets/aliases/default'`), true},
		{"unrelated error", errors.New("permission denied"), false},
		// A generic "no such file" that is NOT a bus dial must not be masked.
		{"unrelated missing file", errors.New("open /some/config: no such file or directory"), false},
		{"not found", ErrNotFound, false},
	}
	for _, c := range cases {
		if got := IsUnavailable(c.err); got != c.want {
			t.Errorf("IsUnavailable(%q) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestBackendCauseStripsClassificationMarkers: the sentinels exist for
// errors.Is, not for humans. A message that opens with "secret not found" and
// then says "backend unavailable" contradicts itself, so callers rendering the
// problem show the cause alone.
func TestBackendCauseStripsClassificationMarkers(t *testing.T) {
	root := errors.New("dial unix /run/user/1000/bus: connect: no such file or directory")
	classified := fmt.Errorf("%w: %w: %w", ErrNotFound, ErrUnavailable, root)

	if got := BackendCause(classified); got.Error() != root.Error() {
		t.Errorf("BackendCause = %q, want %q", got, root)
	}
	// Both sentinels must survive on the original error.
	if !errors.Is(classified, ErrNotFound) || !errors.Is(classified, ErrUnavailable) {
		t.Error("wrapping lost a sentinel")
	}
	// Nothing to strip: returned unchanged.
	plain := errors.New("authorization denied")
	if got := BackendCause(plain); got != plain {
		t.Errorf("BackendCause(plain) = %v, want it unchanged", got)
	}
	if got := BackendCause(ErrNotFound); got != ErrNotFound {
		t.Errorf("BackendCause(ErrNotFound) = %v, want it unchanged", got)
	}
}
