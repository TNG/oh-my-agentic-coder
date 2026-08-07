package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
)

// TestBuildProxyFilter_AllowsPublicMavenDeniesBuildScanAndPrivateUpstream
// asserts criteria 4 + 6: the tightened build-path filter (ticket 06)
// ALLOWS public Maven/Gradle endpoints (direct, no TLS interception),
// DENIES build-scan upload hosts, and DENIES private-registry upstream
// hosts (which must route through the credential-lift proxy, never the
// filtered proxy — allowing them would be a bypass path per spec.md:174).
// The filter is built with the public allowlist + the build-scan
// denylist; prompting is disabled (the manifest approval IS the prompt
// replacement), so the default decision is "not in allowlist" → deny
// for anything outside the allowlist.
func TestBuildProxyFilter_AllowsPublicMavenDeniesBuildScanAndPrivateUpstream(t *testing.T) {
	// The production filter config from startBuildProxy: ONLY public
	// endpoints on the allowlist. Private-registry upstreams are NOT
	// added (the credential-lift proxy is the sole served path for them).
	filter := netproxy.NewFilter(netproxy.FilterConfig{
		AllowDomains: publicGradleMavenAllowlist,
		DenyDomains:  buildScanDenylist,
		Logf:         func(string, ...any) {},
	})
	ctx := context.Background()
	// Public Maven/Gradle endpoints are ALLOWED (criterion 6: public
	// resolution uses the normal filtered path, no TLS interception).
	for _, h := range []string{
		"repo.maven.apache.org",
		"repo1.maven.org",
		"plugins.gradle.org",
		"services.gradle.org",
		"downloads.gradle.org",
		"jitpack.io",
	} {
		v := filter.CheckHost(ctx, h, 443)
		if v.Decision != netproxy.Allow {
			t.Errorf("public endpoint %q must be ALLOWED, got %s (%s)", h, decisionWord(v.Decision), v.Reason)
		}
	}
	// A private-registry upstream host is DENIED by the filtered proxy —
	// build code cannot reach a private upstream directly; it must go
	// through the credential-lift proxy (spec.md:174: direct external
	// networking remains denied). This closes the bypass path the
	// ticket-06 review flagged.
	v := filter.CheckHost(ctx, "maven.internal.example", 443)
	if v.Decision != netproxy.Deny {
		t.Errorf("private-registry upstream must be DENIED by the filtered proxy (route through credproxy), got %s (%s)", decisionWord(v.Decision), v.Reason)
	}
	// Build-scan upload hosts are DENIED (criterion 4: a --scan attempt
	// that would upload is blocked at the filter).
	for _, h := range buildScanDenylist {
		v := filter.CheckHost(ctx, h, 443)
		if v.Decision != netproxy.Deny {
			t.Errorf("build-scan host %q must be DENIED, got %s (%s)", h, decisionWord(v.Decision), v.Reason)
		}
	}
	// An unlisted host (e.g. a random egress) is DENIED fail-closed
	// (the allowlist is non-empty → default is "not in allowlist").
	v = filter.CheckHost(ctx, "evil.example.com", 443)
	if v.Decision != netproxy.Deny {
		t.Errorf("unlisted host must be DENIED fail-closed, got %s (%s)", decisionWord(v.Decision), v.Reason)
	}
	// The denial must not echo a credential (none is involved here, but
	// assert the reason text is policy-shaped, not credential-bearing).
	if strings.Contains(v.Reason, "password") || strings.Contains(v.Reason, "token") {
		t.Errorf("denial reason must not mention credentials: %s", v.Reason)
	}
}

func decisionWord(d netproxy.Decision) string {
	if d == netproxy.Allow {
		return "ALLOW"
	}
	return "DENY"
}
