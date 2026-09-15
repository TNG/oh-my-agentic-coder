//go:build vuln

// The suite asserts properties omac is supposed to hold, so a test that
// cannot run must fail loudly: a silent skip in a suite where red means
// "vulnerable" reads as "property holds", which is the one outcome that must
// never be faked. See doc.go for the package summary.
package sectest

import (
	"net"
	"testing"
)

// RequireLoopbackListener fails when the environment forbids binding a
// loopback port. Tests that drive a real facade, control plane, or sidecar
// need one, and net/http/httptest panics rather than reporting the problem.
func RequireLoopbackListener(tb testing.TB) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("cannot bind a loopback listener (%v): this test needs one; run the security suite outside a sandbox that forbids it", err)
	}
	ln.Close()
}
