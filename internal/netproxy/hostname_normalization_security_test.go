//go:build vuln

package netproxy

import (
	"context"
	"testing"
)

// Hostname normalization.
//
// Every rule the filter applies — the metadata hard-deny map, deny_domain,
// allow_domain, and both decision stores — is keyed on the string
// NormalizeHost returns. A spelling that survives normalization as a
// different string therefore matches no rule and falls through to the
// default decision, which in blocklist mode is "allow". Normalization is not
// a tidying step; it is the lookup key the whole policy hangs off, so it has
// to map every DNS-equivalent spelling of a name onto one key.

// TestSecurityDenyRulesSurviveTrailingDotVariants asserts that appending
// root-zone dots to a denied host does not evade its rule.
//
// Asserted through CheckHost rather than Check so DNS plays no part: this is
// about the rule lookup, not about whether a name resolves. CheckHost is also
// the admission boundary in its own right — it is what runs for every host
// routed through a chained upstream proxy, where omac never resolves anything
// and the hostname is the only thing policy ever sees.
func TestSecurityDenyRulesSurviveTrailingDotVariants(t *testing.T) {
	f := NewFilter(FilterConfig{DenyDomains: []string{"blocked.example"}})

	denied := func(host string) bool {
		return f.CheckHost(context.Background(), host, 443).Decision == Deny
	}

	// Control: the plain name and its single-dot form must already be denied.
	// The single-dot case is what normalization handles correctly today, so
	// it proves the rule and the fixture work before the evasions below.
	for _, host := range []string{"blocked.example", "blocked.example.", "BLOCKED.EXAMPLE"} {
		if !denied(host) {
			t.Fatalf("CheckHost(%q) was allowed: the deny rule itself is not matching, so this test proves nothing", host)
		}
	}

	for _, host := range []string{
		"blocked.example..",
		"BLOCKED.EXAMPLE..",
		"blocked.example...",
	} {
		if !denied(host) {
			t.Errorf("CheckHost(%q) was allowed: any deny rule can be evaded by padding the hostname with root-zone dots", host)
		}
	}
}

// TestSecurityMetadataHardDenySurvivesTrailingDotVariants asserts the same
// for the hard-deny list, which is the one set of destinations the user is
// never even offered a prompt for.
//
// It is kept separate from the deny_domain case because the consequence
// differs in kind: a deny_domain evasion reaches a host the user chose to
// block, while this one reaches the cloud instance-metadata service, whose
// whole purpose is handing out credentials to whoever asks.
func TestSecurityMetadataHardDenySurvivesTrailingDotVariants(t *testing.T) {
	// A prompter that says yes to everything: a hard deny must never consult
	// it, so if normalization lets the host through to the default decision
	// this allows it, exactly as an interactive user clicking "allow" would.
	p := &fakePrompter{res: PromptResult{Allow: true}}
	f := NewFilter(FilterConfig{PromptEnabled: true, Prompter: p})

	denied := func(host string) bool {
		return f.CheckHost(context.Background(), host, 80).Decision == Deny
	}

	// Control: the canonical spellings are hard-denied and never prompted.
	for _, host := range []string{"169.254.169.254", "metadata.google.internal"} {
		if !denied(host) {
			t.Fatalf("CheckHost(%q) was allowed: the metadata hard-deny is gone entirely", host)
		}
	}
	if p.calls != 0 {
		t.Fatalf("hard-denied hosts prompted %d times: a hard deny must never be promptable", p.calls)
	}

	for _, host := range []string{
		"169.254.169.254..",
		"metadata.google.internal..",
		"METADATA.GOOGLE.INTERNAL..",
	} {
		if !denied(host) {
			t.Errorf("CheckHost(%q) was allowed: the instance-metadata service, which hands out cloud credentials to any caller, is reachable by padding its name with root-zone dots", host)
		}
	}
}
