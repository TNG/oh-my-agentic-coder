// Package diagnose contains the pure analysis behind `omac diagnose`: it
// correlates the network decisions the sandbox actually made (observed at
// runtime) against the effective network policy, to surface blocked hosts
// and non-obvious misconfigurations that are otherwise invisible to the
// user.
//
// It depends on nothing outside the standard library. The concrete,
// change-prone omac types — audit events, the sandbox profile, and the
// domain-matching rules — are adapted into the DTOs below by the cli
// composition layer and injected here (see DomainMatcher). This keeps the
// analysis insulated from omac's frequent schema churn: when a schema
// changes, only the thin cli adapter changes, never this package.
package diagnose

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Decision is one normalized network admission decision observed at
// runtime, decoupled from the audit wire format.
type Decision struct {
	Host    string
	Port    int
	Allowed bool
	// Source is why the decision was made, as classified by the sandbox:
	// deny_domain | allowlist | hard-deny | prompt | learned | default |
	// dns | timeout | unavailable. Treated as an opaque contract string.
	Source string
	When   time.Time
}

// Policy is the effective network policy in force, decoupled from
// sandboxprofile. Only the fields the analysis needs are carried.
type Policy struct {
	Mode          string // filtered | blocked | open
	AllowDomains  []string
	DenyDomains   []string
	PromptEnabled bool
	UpstreamProxy string
	AllowVars     []string
}

// DomainMatcher reports whether host matches any entry in list, using the
// sandbox's own matching semantics. It is injected so this package never
// re-implements (and drifts from) netproxy's rule matching.
type DomainMatcher func(host string, list []string) bool

// BlockedHost aggregates the denied decisions for one host across a run.
type BlockedHost struct {
	Host     string
	Count    int
	Ports    []int
	Sources  []string
	LastSeen time.Time
}

// Severity ranks a hint for ordering and display.
type Severity string

const (
	SevWarn Severity = "warn"
	SevInfo Severity = "info"
)

// Hint is a human-actionable diagnostic derived from correlating observed
// decisions against the effective policy.
type Hint struct {
	Severity Severity
	Title    string
	Detail   []string
}

// Report is the full result of analyzing one set of decisions.
type Report struct {
	Total   int
	Denied  int
	Blocked []BlockedHost
	Hints   []Hint
}

// Build analyzes decisions against the policy and returns a full Report.
func Build(p Policy, decisions []Decision, match DomainMatcher) Report {
	denied := 0
	for _, d := range decisions {
		if !d.Allowed {
			denied++
		}
	}
	return Report{
		Total:   len(decisions),
		Denied:  denied,
		Blocked: Blocked(decisions),
		Hints:   Analyze(p, decisions, match),
	}
}

// Blocked groups denied decisions by host, most-blocked first.
func Blocked(decisions []Decision) []BlockedHost {
	idx := map[string]*BlockedHost{}
	var order []string
	for _, d := range decisions {
		if d.Allowed {
			continue
		}
		b := idx[d.Host]
		if b == nil {
			b = &BlockedHost{Host: d.Host}
			idx[d.Host] = b
			order = append(order, d.Host)
		}
		b.Count++
		b.Ports = addInt(b.Ports, d.Port)
		b.Sources = addStr(b.Sources, d.Source)
		if d.When.After(b.LastSeen) {
			b.LastSeen = d.When
		}
	}
	out := make([]BlockedHost, 0, len(order))
	for _, h := range order {
		out = append(out, *idx[h])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// Analyze correlates observed decisions against the policy and returns
// actionable hints, warnings before informational notes.
func Analyze(p Policy, decisions []Decision, match DomainMatcher) []Hint {
	if match == nil {
		match = func(string, []string) bool { return false }
	}
	var hints []Hint

	for _, b := range Blocked(decisions) {
		hints = append(hints, blockedHostHint(p, b, match))
	}

	hints = append(hints, deadAllowRuleHints(p, decisions, match)...)
	hints = append(hints, overBroadAllowHints(p)...)
	hints = append(hints, sourceHints(decisions)...)
	hints = append(hints, environmentHints(p)...)

	sort.SliceStable(hints, func(i, j int) bool {
		return sevRank(hints[i].Severity) < sevRank(hints[j].Severity)
	})
	return hints
}

// blockedHostHint classifies a single blocked host. The key value is the
// clash case: a host that IS allow-listed yet was denied anyway, which
// means a deny rule silently shadows the allow.
func blockedHostHint(p Policy, b BlockedHost, match DomainMatcher) Hint {
	inAllow := match(b.Host, p.AllowDomains)
	inDeny := match(b.Host, p.DenyDomains)
	switch {
	case inAllow:
		// The surprising case: allow-listed yet denied. Whatever else is
		// true, this is what the user did not expect, so it outranks the
		// plain deny_domain note even when the host is in both lists.
		return Hint{
			Severity: SevWarn,
			Title:    fmt.Sprintf("%s is in allow_domain but was still DENIED", b.Host),
			Detail: []string{
				fmt.Sprintf("Denied %d time(s) with source=%s.", b.Count, strings.Join(b.Sources, ",")),
				shadowExplanation(inDeny, b.Sources),
			},
		}
	case inDeny:
		return Hint{
			Severity: SevInfo,
			Title:    fmt.Sprintf("%s is blocked by network.deny_domain", b.Host),
			Detail: []string{
				fmt.Sprintf("Denied %d time(s). If this host is actually needed, remove the matching deny_domain entry.", b.Count),
			},
		}
	default:
		detail := []string{fmt.Sprintf("Denied %d time(s); no network.allow_domain entry matches.", b.Count)}
		fix := "If it is a required dependency, allow the exact host (not a broad wildcard). If you don't recognize it, leave it denied — the sandbox is correctly blocking unexpected egress."
		if p.PromptEnabled {
			fix = "If it is a required dependency, answer \"Allow\" for the exact host or add it to network.allow_domain. If you don't recognize it, deny it — the sandbox is correctly blocking unexpected egress."
		}
		return Hint{
			Severity: SevWarn,
			Title:    fmt.Sprintf("%s was blocked and is not in any allow rule", b.Host),
			Detail:   []string{detail[0], fix},
		}
	}
}

// shadowExplanation names the deny rule that most likely overrode the
// allow, using the direct deny_domain overlap when present and otherwise
// the observed decision source (learned deny / built-in hard-deny).
func shadowExplanation(inDeny bool, sources []string) string {
	if inDeny {
		return "It is also listed in network.deny_domain, which wins over allow_domain. Remove the deny_domain entry."
	}
	for _, s := range sources {
		switch s {
		case "learned":
			return "A learned \"deny\" in the <profile>.pages.json file shadows it. Remove that entry to re-enable the allow."
		case "hard-deny":
			return "A built-in metadata/link-local hard-deny blocks it; this cannot be overridden by allow_domain."
		}
	}
	return "A deny rule shadows the allow. Check network.deny_domain, a learned \"deny\" in the <profile>.pages.json file, or the built-in hard-deny."
}

// deadAllowRuleHints flags allow_domain entries that no observed traffic
// matched — a common sign of a typo or an obsolete rule. Only meaningful
// when there were observed decisions to compare against.
func deadAllowRuleHints(p Policy, decisions []Decision, match DomainMatcher) []Hint {
	if len(decisions) == 0 {
		return nil
	}
	var hints []Hint
	for _, entry := range p.AllowDomains {
		used := false
		for _, d := range decisions {
			if match(d.Host, []string{entry}) {
				used = true
				break
			}
		}
		if !used {
			hints = append(hints, Hint{
				Severity: SevInfo,
				Title:    fmt.Sprintf("allow_domain %q matched no traffic this run", entry),
				Detail:   []string{"No connection used it. Consider removing it to keep the allowlist minimal (least privilege), or check for a typo (wildcards use \"*.example.com\")."},
			})
		}
	}
	return hints
}

// overBroadAllowHints flags allow_domain entries that open far more than a
// specific host — a whole-TLD wildcard like "*.com". Surfacing these steers
// the profile toward least privilege rather than blanket allow.
func overBroadAllowHints(p Policy) []Hint {
	var hints []Hint
	for _, e := range p.AllowDomains {
		suffix, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(e)), "*.")
		if ok && suffix != "" && !strings.Contains(suffix, ".") {
			hints = append(hints, Hint{
				Severity: SevWarn,
				Title:    fmt.Sprintf("allow_domain %q is very broad", e),
				Detail:   []string{fmt.Sprintf("It matches every host under .%s. Narrow it to the specific hosts you actually use (least privilege).", suffix)},
			})
		}
	}
	return hints
}

// sourceHints surfaces denials whose source points at a specific class of
// problem rather than a plain policy miss.
func sourceHints(decisions []Decision) []Hint {
	var hints []Hint
	if anyDeniedSource(decisions, "dns") {
		hints = append(hints, Hint{
			Severity: SevWarn,
			Title:    "Some hosts failed DNS resolution",
			Detail:   []string{"source=dns: the name did not resolve. Check for typos, or that the sandbox has egress for DNS."},
		})
	}
	if anyDeniedSource(decisions, "unavailable") {
		hints = append(hints, Hint{
			Severity: SevInfo,
			Title:    "Some prompts fell back to deny",
			Detail:   []string{"source=unavailable: the prompt timed out or no dialog backend was available. Install a dialog backend, or pre-populate network.allow_domain."},
		})
	}
	return hints
}

// environmentHints emits tool-agnostic notes about the environment the
// sandbox imposes — the two settings most likely to break a tool in a way
// that is not obvious from the tool's own error message.
func environmentHints(p Policy) []Hint {
	var hints []Hint
	if p.UpstreamProxy != "" {
		hints = append(hints, Hint{
			Severity: SevInfo,
			Title:    "An upstream proxy is configured",
			Detail:   []string{"omac injects HTTP(S)_PROXY into the sandbox. A proxy-unaware tool that reads its own config instead of the environment may bypass it and fail."},
		})
	}
	if len(p.AllowVars) > 0 {
		hints = append(hints, Hint{
			Severity: SevInfo,
			Title:    "Environment variables are allow-listed",
			Detail:   []string{"Ambient variables not in environment.allow_vars are stripped inside the sandbox. If a tool needs one, add it there."},
		})
	}
	return hints
}

func anyDeniedSource(decisions []Decision, source string) bool {
	for _, d := range decisions {
		if !d.Allowed && d.Source == source {
			return true
		}
	}
	return false
}

func sevRank(s Severity) int {
	if s == SevWarn {
		return 0
	}
	return 1
}

func addInt(xs []int, x int) []int {
	for _, v := range xs {
		if v == x {
			return xs
		}
	}
	return append(xs, x)
}

func addStr(xs []string, x string) []string {
	if x == "" {
		return xs
	}
	for _, v := range xs {
		if v == x {
			return xs
		}
	}
	return append(xs, x)
}
