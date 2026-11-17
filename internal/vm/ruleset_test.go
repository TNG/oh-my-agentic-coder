package vm

import (
	"strings"
	"testing"
)

func TestRulesetShape(t *testing.T) {
	rs := Ruleset()
	for _, want := range []string{
		"table inet omac-vmguard",
		"chain output",
		"chain forward",
		`oifname "lo" accept`,
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("ruleset lacks %q", want)
		}
	}
	for want, n := range map[string]int{
		"counter reject":                          1,
		"counter drop":                            1,
		"priority filter + 10; policy drop;":      2,
		"ct state established,related accept":     2,
		"type filter hook output priority filter": 1,
	} {
		if got := strings.Count(rs, want); got != n {
			t.Errorf("ruleset contains %q %d times, want %d", want, got, n)
		}
	}
}

func TestRulesetNormalizesWindowsCheckout(t *testing.T) {
	saved := vmguardRuleset
	vmguardRuleset = strings.ReplaceAll(saved, "\n", "\r\n")
	t.Cleanup(func() { vmguardRuleset = saved })

	got := Ruleset()
	if strings.Contains(got, "\r") {
		t.Error("Ruleset kept CR from a CRLF checkout; the guest would get broken nft syntax")
	}
	if !strings.Contains(got, "table inet omac-vmguard") {
		t.Error("Ruleset lost its content while normalizing")
	}
}
