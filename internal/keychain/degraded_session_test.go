//go:build linux

package keychain

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestReadsDegradeWhenSessionBusSocketIsDead reproduces the WSL2 /
// `Linger=no` field failure end-to-end through go-keyring, not just the
// IsUnavailable classifier: DBUS_SESSION_BUS_ADDRESS still names a
// /run/user/<uid>/bus socket that systemd tore down when the login session
// ended. Read paths (start/serve) must treat the resulting dial failure as
// "backend unavailable" — ErrNotFound / false — so optional secrets and
// env_passthrough fallbacks keep working, rather than surfacing a raw
// "dial unix ...: connect: no such file or directory" hard error.
func TestReadsDegradeWhenSessionBusSocketIsDead(t *testing.T) {
	deadSocket := filepath.Join(t.TempDir(), "bus")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+deadSocket)

	// errors.Is, not ==: the error also carries ErrUnavailable so a caller
	// that has exhausted its fallbacks can say "no Secret Service provider"
	// instead of "run omac secrets set" (issue #174, Failure 4). Every
	// fallback-relying caller still sees ErrNotFound, which is the point.
	_, err := Get("wsl2-probe-skill", "token")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on dead session bus = %v, want ErrNotFound", err)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("Get on dead session bus = %v, want it to also carry ErrUnavailable", err)
	}

	has, err := Has("wsl2-probe-skill", "token")
	if err != nil {
		t.Errorf("Has on dead session bus returned error %v, want nil", err)
	}
	if has {
		t.Error("Has on dead session bus = true, want false")
	}
}
