//go:build vuln

package facade

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSecurityUpgradeTriggerRequiresRecognizedProtocol asserts that
// isUpgrade does not take the hijack-and-splice path for an arbitrary,
// unrecognized Upgrade token.
//
// isUpgrade only checks that Connection carries the "upgrade" token and
// that Upgrade is non-empty — any non-empty value qualifies, not just
// "websocket" (or another protocol the facade actually knows how to
// proxy). Every protection the ordinary HTTP path applies — MaxBytesReader,
// timeouts — is bypassed once a request is classified as an upgrade, so
// the classification itself is a control point.
func TestSecurityUpgradeTriggerRequiresRecognizedProtocol(t *testing.T) {
	// Control: a genuine websocket upgrade request is still classified as
	// an upgrade, so a fix that rejects everything wouldn't make this test
	// pass for the wrong reason.
	genuine := httptest.NewRequest(http.MethodGet, "/", nil)
	genuine.Header.Set("Connection", "upgrade")
	genuine.Header.Set("Upgrade", "websocket")
	genuine.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	genuine.Header.Set("Sec-WebSocket-Version", "13")
	if !isUpgrade(genuine) {
		t.Fatalf("control: a genuine websocket upgrade request was not classified as an upgrade: the fixture is broken, not the security property")
	}

	hostile := httptest.NewRequest(http.MethodGet, "/", nil)
	hostile.Header.Set("Connection", "upgrade")
	hostile.Header.Set("Upgrade", "h2c")

	if isUpgrade(hostile) {
		t.Errorf("isUpgrade classified an Upgrade: h2c request (no websocket handshake headers) as an upgrade: any non-empty Upgrade token bypasses MaxBytesReader and timeouts through the hijack-and-splice path")
	}
}
