//go:build vuln

// Package sectest holds the environment guards shared by omac's security
// regression suite (build tag "vuln"). The suite asserts properties omac is
// supposed to hold, so a test that cannot run must fail loudly: a silent skip
// in a suite where red means "vulnerable" reads as "property holds", which is
// the one outcome that must never be faked.
package sectest

import (
	"net"
	"os"
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

// RequireThrowawayContainer skips unless the suite is running in a container
// it is allowed to wreck. The tests it guards damage the machine they run on:
// they overwrite state under a live /tmp, poison the shared tool cache, or
// drive a real `omac serve` that other sessions may be using.
//
// This is the one guard in the suite that skips rather than fails. Refusing to
// run is the correct answer on a developer's machine, and scripts/
// security-suite.sh keeps the accounting honest by comparing the failing set
// against the pinned one: a skipped test shows up as "now passing" and has to
// be explained, so the gap cannot go unnoticed.
func RequireThrowawayContainer(tb testing.TB) {
	tb.Helper()
	if os.Getenv("OMAC_SECURITY_CONTAINER") != "1" {
		tb.Skip("destructive: set OMAC_SECURITY_CONTAINER=1 and run in a throwaway container (scripts/security-suite.sh handles this)")
	}
}
