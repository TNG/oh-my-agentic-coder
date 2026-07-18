package diagnose

import (
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
)

// realMatch is the production matcher, injected exactly as cli does, so the
// tests exercise the same semantics the sandbox enforces.
var realMatch DomainMatcher = netproxy.MatchDomainList

func dec(host string, allowed bool, source string) Decision {
	return Decision{Host: host, Port: 443, Allowed: allowed, Source: source, When: time.Unix(0, 0)}
}

func hintTitles(hs []Hint) string {
	var b strings.Builder
	for _, h := range hs {
		b.WriteString(string(h.Severity))
		b.WriteString(": ")
		b.WriteString(h.Title)
		b.WriteString("\n")
	}
	return b.String()
}

func findHint(hs []Hint, substr string) *Hint {
	for i := range hs {
		if strings.Contains(hs[i].Title, substr) {
			return &hs[i]
		}
	}
	return nil
}

func TestBlockedGroupsAndSortsByCount(t *testing.T) {
	decisions := []Decision{
		dec("a.example", false, "blocklist"),
		dec("b.example", false, "allowlist"),
		dec("b.example", false, "allowlist"),
		dec("c.example", true, "allowlist"),
	}
	got := Blocked(decisions)
	if len(got) != 2 {
		t.Fatalf("want 2 blocked hosts, got %d (%v)", len(got), got)
	}
	if got[0].Host != "b.example" || got[0].Count != 2 {
		t.Fatalf("most-blocked-first broken: %+v", got[0])
	}
	if got[1].Host != "a.example" {
		t.Fatalf("want a.example second, got %q", got[1].Host)
	}
}

// The headline case: a host the user put in allow_domain is still denied.
// The analysis must call out the clash, not just say "blocked".
func TestAllowlistedButDeniedIsFlaggedAsClash(t *testing.T) {
	pol := Policy{Mode: "filtered", AllowDomains: []string{"repo.corp"}}
	decisions := []Decision{dec("repo.corp", false, "blocklist")}

	hints := Analyze(pol, decisions, realMatch)
	h := findHint(hints, "repo.corp is in allow_domain but was still DENIED")
	if h == nil {
		t.Fatalf("clash hint missing.\n%s", hintTitles(hints))
	}
	if h.Severity != SevWarn {
		t.Fatalf("clash must be a warning, got %s", h.Severity)
	}
}

// A host in BOTH allow_domain and deny_domain is a direct contradiction:
// the clash warning must win over the plain deny_domain note and name the
// deny_domain overlap as the cause.
func TestAllowAndDenyOverlapReportsClashNamingDenyDomain(t *testing.T) {
	pol := Policy{AllowDomains: []string{"repo.corp"}, DenyDomains: []string{"repo.corp"}}
	decisions := []Decision{dec("repo.corp", false, "blocklist")}

	hints := Analyze(pol, decisions, realMatch)
	h := findHint(hints, "repo.corp is in allow_domain but was still DENIED")
	if h == nil {
		t.Fatalf("clash must outrank the deny_domain note.\n%s", hintTitles(hints))
	}
	if h.Severity != SevWarn {
		t.Fatalf("want warning, got %s", h.Severity)
	}
	if !strings.Contains(strings.Join(h.Detail, " "), "deny_domain") {
		t.Fatalf("clash should name deny_domain as the shadow: %v", h.Detail)
	}
}

func TestBlockedNotInAnyRuleSuggestsAllowlist(t *testing.T) {
	pol := Policy{Mode: "filtered", PromptEnabled: false}
	decisions := []Decision{dec("registry.npmjs.org", false, "allowlist")}

	hints := Analyze(pol, decisions, realMatch)
	h := findHint(hints, "registry.npmjs.org was blocked and is not in any allow rule")
	if h == nil {
		t.Fatalf("missing hint.\n%s", hintTitles(hints))
	}
	joined := strings.Join(h.Detail, " ")
	if !strings.Contains(joined, "allow_domain") {
		t.Fatalf("hint should point at allow_domain, got %q", joined)
	}
	if !strings.Contains(joined, "denied by default") {
		t.Fatalf("prompt-disabled hint should explain default-deny, got %q", joined)
	}
}

func TestWildcardAllowMatchesSubdomain(t *testing.T) {
	// A denial on a host covered by a "*.suffix" allow entry must be
	// treated as a clash, proving the injected matcher's wildcard
	// semantics are used (not a naive equality check).
	pol := Policy{AllowDomains: []string{"*.tngtech.com"}}
	decisions := []Decision{dec("chat.tngtech.com", false, "learned")}

	hints := Analyze(pol, decisions, realMatch)
	if findHint(hints, "is in allow_domain but was still DENIED") == nil {
		t.Fatalf("wildcard allow not honored.\n%s", hintTitles(hints))
	}
}

func TestDeadAllowRuleDetected(t *testing.T) {
	pol := Policy{AllowDomains: []string{"used.example", "typo.exmaple"}}
	decisions := []Decision{dec("used.example", true, "allowlist")}

	hints := Analyze(pol, decisions, realMatch)
	if findHint(hints, `allow_domain "typo.exmaple" matched no traffic`) == nil {
		t.Fatalf("dead-rule hint missing.\n%s", hintTitles(hints))
	}
	if findHint(hints, `allow_domain "used.example" matched no traffic`) != nil {
		t.Fatalf("used rule must not be flagged as dead.\n%s", hintTitles(hints))
	}
}

func TestNoDeadRuleHintWithoutObservedTraffic(t *testing.T) {
	// With no decisions we cannot know a rule is unused; stay silent.
	pol := Policy{AllowDomains: []string{"whatever.example"}}
	if h := findHint(Analyze(pol, nil, realMatch), "matched no traffic"); h != nil {
		t.Fatalf("should not flag dead rules with zero observations: %q", h.Title)
	}
}

func TestDNSDenialSurfaced(t *testing.T) {
	pol := Policy{}
	decisions := []Decision{dec("nope.invalid", false, "dns")}
	if findHint(Analyze(pol, decisions, realMatch), "failed DNS resolution") == nil {
		t.Fatal("dns-source denial should surface a hint")
	}
}

func TestEnvironmentHintsAreOrderedAfterWarnings(t *testing.T) {
	pol := Policy{
		AllowDomains:  nil,
		UpstreamProxy: "http://proxy:8080",
		AllowVars:     []string{"HOME"},
	}
	decisions := []Decision{dec("blocked.example", false, "allowlist")}
	hints := Analyze(pol, decisions, realMatch)
	if len(hints) < 2 {
		t.Fatalf("expected warning + info hints, got %d", len(hints))
	}
	if hints[0].Severity != SevWarn {
		t.Fatalf("warnings must sort first, got %s first:\n%s", hints[0].Severity, hintTitles(hints))
	}
	if findHint(hints, "upstream proxy is configured") == nil || findHint(hints, "Environment variables are allow-listed") == nil {
		t.Fatalf("generic env/proxy hints missing.\n%s", hintTitles(hints))
	}
}

func TestBuildCountsDenied(t *testing.T) {
	decisions := []Decision{
		dec("a", false, "allowlist"),
		dec("b", true, "allowlist"),
		dec("c", false, "blocklist"),
	}
	r := Build(Policy{}, decisions, realMatch)
	if r.Total != 3 || r.Denied != 2 {
		t.Fatalf("Build counts wrong: total=%d denied=%d", r.Total, r.Denied)
	}
}
