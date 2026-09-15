//go:build vuln

package sandboxrun

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestSecurityInvalidUpstreamProxyRedactsUserinfo asserts that a malformed
// upstream-proxy value never reaches the sandbox diagnostics stream with
// its credentials intact.
//
// resolveUpstreamProxy documents "Only proxyURL.Host is logged — never the
// userinfo (credentials)" as its own invariant, but the parse-failure /
// empty-host branch prints the raw input string verbatim, credentials
// included.
//
// Named outside the TestResolveUpstreamProxy* prefix deliberately: that
// prefix drives a real proxy server on loopback and is skipped in
// environments (including this one) that cannot bind a TCP listener. This
// property needs neither — it only inspects the bytes written to a
// io.Writer.
func TestSecurityInvalidUpstreamProxyRedactsUserinfo(t *testing.T) {
	clearProxyEnv(t)

	var stderr bytes.Buffer
	p := &sandboxprofile.Profile{Network: sandboxprofile.Network{UpstreamProxy: "http://corpuser:hunter2@"}}
	_ = resolveUpstreamProxy(p, &stderr, t.Logf)

	// Control: the warning is actually emitted (the fixture reaches the
	// vulnerable branch at all), so a value check that vacuously passes on
	// empty output can't be mistaken for the property holding.
	if stderr.Len() == 0 {
		t.Fatalf("control: resolveUpstreamProxy wrote nothing to stderr: the fixture is broken, not the security property")
	}

	if strings.Contains(stderr.String(), "hunter2") {
		t.Errorf("resolveUpstreamProxy logged the raw invalid proxy string with credentials intact: %q", stderr.String())
	}
}
