package ephemeraldocker

import (
	"strings"
	"testing"
)

// The embedded ruleset is the production copy of the spike-measured
// omac-vmguard ruleset. It must carry the two deliberately-kept rules with
// their rationale comments, and a counter on the final drop.
func TestRulesetCarriesSpikeShape(t *testing.T) {
	rs := Ruleset()
	for _, want := range []string{
		"table inet omac-vmguard",
		"type filter hook forward priority filter + 10; policy drop;",
		"ct state established,related counter accept",
		`iifname "eth0" ct status dnat counter accept`,
		`iifname != "eth0" oifname != "eth0" counter accept`,
		"counter drop",
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("Ruleset() missing %q", want)
		}
	}
}

func TestRulesetDocumentsDeliberateRules(t *testing.T) {
	rs := Ruleset()
	// DNAT fallback rationale must name the condition that would make the
	// rule live (userland-proxy disabled), so nobody "cleans it up".
	if !strings.Contains(rs, "userland-proxy") {
		t.Error("Ruleset() DNAT-fallback comment must mention userland-proxy")
	}
	// Bridge acceptance rationale must name br_netfilter.
	if !strings.Contains(rs, "br_netfilter") {
		t.Error("Ruleset() bridge-acceptance comment must mention br_netfilter")
	}
}
