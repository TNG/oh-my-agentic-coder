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
