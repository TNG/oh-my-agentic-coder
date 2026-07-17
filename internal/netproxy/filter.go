// Package netproxy implements omac's network guardrail for the
// built-in sandbox: a token-authenticated HTTP CONNECT/forward proxy
// that runs unsandboxed in the supervisor and filters by hostname.
//
// Design notes (mirrors nono's nono-proxy semantics):
//   - TLS is never terminated; CONNECT is a raw byte tunnel.
//   - DNS is resolved once per request and the upstream connection is
//     made to the resolved IPs (anti DNS-rebinding TOCTOU).
//   - Cloud metadata endpoints and link-local destinations are denied
//     unconditionally and are never promptable.
package netproxy

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
)

// Decision is the outcome of a filter check.
type Decision int

const (
	// Deny blocks the request.
	Deny Decision = iota
	// Allow permits the request.
	Allow
)

// Verdict carries the decision, the reason for logging, and the scope +
// persisted flag for audit. Scope and Persisted are only meaningful for
// prompt decisions; non-prompt verdicts leave them empty.
type Verdict struct {
	Decision  Decision
	Reason    string // e.g. "hard-deny metadata", "deny_domain", "allowlist", "prompt:allow_once"
	Scope     string // once|host|suffix (prompt decisions only)
	Persisted bool   // true when the decision was persisted (prompt decisions only)
}

// hardDenyHosts can never be allowed, even interactively (nono parity).
var hardDenyHosts = map[string]bool{
	"169.254.169.254":          true,
	"metadata.google.internal": true,
	"metadata.azure.internal":  true,
}

// Prompter asks the user about a host:port that no static rule covers.
// Implemented by the interactive dialog in the prompt package; nil
// disables prompting.
type Prompter interface {
	// Prompt blocks until a decision is made (or times out). scopeHost
	// and scopeSuffix report what was decided for persistence handling;
	// persist=true means the decision was "permanently".
	Prompt(host string, port int) PromptResult
}

// PromptResult is the parsed outcome of an interactive prompt.
type PromptResult struct {
	Allow   bool
	Persist bool   // permanent (host or suffix scope) vs once
	Scope   string // "host" or "suffix" when Persist
	Suffix  string // populated when Scope == "suffix"
	// NeedsIntent signals that the user clicked "Explain more" — the
	// request is denied with a marker pointing the agent at the intent
	// endpoint. Never persisted.
	NeedsIntent bool
}

// LearnedStore persists permanent prompt decisions. Implemented by the
// prompt package's policy file store.
type LearnedStore interface {
	// Lookup returns (verdict, found). Suffix entries match the host
	// itself and any subdomain.
	Lookup(host string) (allow bool, found bool)
	// Record persists a permanent decision.
	Record(host, scope string, allow bool) error
}

// FilterConfig configures a Filter.
type FilterConfig struct {
	AllowDomains []string
	DenyDomains  []string
	// PromptEnabled gates interactive prompting (the Prompter may still
	// be nil, in which case OnUnavailableAllow decides).
	PromptEnabled bool
	// OnUnavailableAllow: what to do when prompting is enabled but no
	// prompter/dialog is available or it times out. False = deny.
	OnUnavailableAllow bool
	Prompter           Prompter
	Learned            LearnedStore
	// Resolve overrides DNS resolution in tests. Defaults to net.DefaultResolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Logf receives one line per decision; nil discards.
	Logf func(format string, args ...any)

	// Auditor receives a net.decision event per decision. nil => no-op.
	Auditor audit.Auditor

	// onCoalesceWait, when non-nil, is called just before a request blocks
	// on an in-flight prompt for the same host (the coalescing path). It is
	// a test seam that lets a test deterministically observe that all
	// followers have parked before releasing the leader's prompt; it is
	// never set in production.
	onCoalesceWait func()
}

// Filter decides host admission and pins DNS results.
type Filter struct {
	cfg FilterConfig

	// promptMu coalesces concurrent prompts for the same host.
	promptMu sync.Mutex
	inflight map[string]*promptWait
}

type promptWait struct {
	done chan struct{}
	res  PromptResult
}

// NewFilter builds a Filter.
func NewFilter(cfg FilterConfig) *Filter {
	if cfg.Resolve == nil {
		cfg.Resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			return addrs, err
		}
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Auditor == nil {
		cfg.Auditor = audit.Nop()
	}
	return &Filter{cfg: cfg, inflight: map[string]*promptWait{}}
}

// Check runs the full decision pipeline for host:port and returns the
// verdict plus the pinned addresses to dial (only meaningful on Allow).
//
// Pipeline order (spec: sandbox-network "Filter decision order"):
//  1. hard deny: metadata hostnames + link-local resolved IPs
//  2. learned permanent deny
//  3. deny_domain blocklist
//  4. allow_domain allowlist / learned permanent allow
//  5. default: prompt if enabled; else deny when allowlist non-empty;
//     else allow (pure blocklist mode)
func (f *Filter) Check(ctx context.Context, host string, port int) (Verdict, []netip.Addr) {
	h := strings.ToLower(strings.TrimSuffix(host, "."))

	// 1. Hard denies. Never promptable.
	if hardDenyHosts[h] {
		return f.log(h, port, Verdict{Decision: Deny, Reason: "hard-deny metadata host"}), nil
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		if isLinkLocal(ip) {
			return f.log(h, port, Verdict{Decision: Deny, Reason: "hard-deny link-local address"}), nil
		}
		if v := f.checkRules(h); v != nil {
			return f.log(h, port, *v), []netip.Addr{ip}
		}
		if v, ok := f.defaultDecision(h, port); ok {
			return f.log(h, port, v), []netip.Addr{ip}
		}
		return f.log(h, port, Verdict{Decision: Deny, Reason: "default deny"}), nil
	}

	// Resolve once; pin results (anti-rebinding).
	addrs, err := f.cfg.Resolve(ctx, h)
	if err != nil || len(addrs) == 0 {
		return f.log(h, port, Verdict{Decision: Deny, Reason: "dns resolution failed"}), nil
	}
	safe := addrs[:0:0]
	for _, a := range addrs {
		if isLinkLocal(a) {
			return f.log(h, port, Verdict{Decision: Deny, Reason: "hard-deny: resolves to link-local"}), nil
		}
		safe = append(safe, a)
	}

	// 2-4. Static and learned rules.
	if v := f.checkRules(h); v != nil {
		if v.Decision == Deny {
			return f.log(h, port, *v), nil
		}
		return f.log(h, port, *v), safe
	}

	// 5. Default.
	if v, ok := f.defaultDecision(h, port); ok {
		if v.Decision == Deny {
			return f.log(h, port, v), nil
		}
		return f.log(h, port, v), safe
	}
	return f.log(h, port, Verdict{Decision: Deny, Reason: "default deny"}), nil
}

// checkRules evaluates learned-deny, deny_domain, allow_domain and
// learned-allow. Returns nil when no rule matches.
func (f *Filter) checkRules(host string) *Verdict {
	if f.cfg.Learned != nil {
		if allow, found := f.cfg.Learned.Lookup(host); found && !allow {
			return &Verdict{Decision: Deny, Reason: "learned permanent deny", Persisted: true}
		}
	}
	if matchDomainList(host, f.cfg.DenyDomains) {
		return &Verdict{Decision: Deny, Reason: "deny_domain"}
	}
	if matchDomainList(host, f.cfg.AllowDomains) {
		return &Verdict{Decision: Allow, Reason: "allow_domain"}
	}
	if f.cfg.Learned != nil {
		if allow, found := f.cfg.Learned.Lookup(host); found && allow {
			return &Verdict{Decision: Allow, Reason: "learned permanent allow", Persisted: true}
		}
	}
	return nil
}

// defaultDecision handles step 5. ok=false means "no decision" (treat
// as deny).
func (f *Filter) defaultDecision(host string, port int) (Verdict, bool) {
	if f.cfg.PromptEnabled {
		res, prompted := f.promptCoalesced(host, port)
		if !prompted {
			if f.cfg.OnUnavailableAllow {
				return Verdict{Decision: Allow, Reason: "prompt unavailable: on_unavailable=allow"}, true
			}
			return Verdict{Decision: Deny, Reason: "prompt unavailable: on_unavailable=deny"}, true
		}
		if res.Persist && f.cfg.Learned != nil {
			target := host
			if res.Scope == "suffix" && res.Suffix != "" {
				target = res.Suffix
			}
			if err := f.cfg.Learned.Record(target, res.Scope, res.Allow); err != nil {
				f.cfg.Logf("omac sandbox: warning: persist learned decision: %v", err)
			}
		}
		scope := res.Scope
		if !res.Persist {
			scope = "once"
		}
		if res.NeedsIntent {
			return Verdict{Decision: Deny, Reason: "prompt:needs_intent", Scope: scope, Persisted: res.Persist}, true
		}
		if res.Allow {
			return Verdict{Decision: Allow, Reason: "prompt:allow", Scope: scope, Persisted: res.Persist}, true
		}
		return Verdict{Decision: Deny, Reason: "prompt:deny", Scope: scope, Persisted: res.Persist}, true
	}
	if len(f.cfg.AllowDomains) > 0 {
		return Verdict{Decision: Deny, Reason: "not in allowlist"}, true
	}
	// Pure blocklist (or no rules at all): allow.
	return Verdict{Decision: Allow, Reason: "default allow (blocklist mode)"}, true
}

// promptCoalesced ensures concurrent requests for the same host share
// one dialog. prompted=false means no prompter is available.
func (f *Filter) promptCoalesced(host string, port int) (PromptResult, bool) {
	if f.cfg.Prompter == nil {
		return PromptResult{}, false
	}
	f.promptMu.Lock()
	if w, ok := f.inflight[host]; ok {
		f.promptMu.Unlock()
		if f.cfg.onCoalesceWait != nil {
			f.cfg.onCoalesceWait()
		}
		<-w.done
		return w.res, true
	}
	w := &promptWait{done: make(chan struct{})}
	f.inflight[host] = w
	f.promptMu.Unlock()

	w.res = f.cfg.Prompter.Prompt(host, port)

	f.promptMu.Lock()
	delete(f.inflight, host)
	f.promptMu.Unlock()
	close(w.done)
	return w.res, true
}

func (f *Filter) log(host string, port int, v Verdict) Verdict {
	word := "DENY"
	if v.Decision == Allow {
		word = "ALLOW"
	}
	f.cfg.Logf("omac sandbox: net %s %s:%d (%s)", word, host, port, v.Reason)
	source := classifyReason(v.Reason)
	scope := v.Scope
	persisted := v.Persisted
	f.cfg.Auditor.Emit(audit.NetDecision(host, port, v.Decision == Allow, scope, source, persisted))
	return v
}

// classifyReason maps a Verdict.Reason to the audit source field. Scope and
// persisted are taken from the Verdict (set by defaultDecision for prompt
// decisions) rather than derived here, so the prompt's actual scope/persist
// propagate to the audit event.
func classifyReason(reason string) (source string) {
	switch {
	case strings.HasPrefix(reason, "hard-deny"):
		return "hard-deny"
	case strings.HasPrefix(reason, "learned"):
		return "learned"
	case reason == "deny_domain":
		return "blocklist"
	case reason == "allow_domain":
		return "allowlist"
	case strings.HasPrefix(reason, "prompt unavailable"):
		return "unavailable"
	case strings.HasPrefix(reason, "prompt:"):
		return "prompt"
	case reason == "not in allowlist":
		return "allowlist"
	case strings.HasPrefix(reason, "dns"):
		return "dns"
	default:
		return "default"
	}
}

// matchDomainList reports whether host matches any entry. Entries are
// exact hostnames or "*.suffix" wildcards; a wildcard matches the
// suffix itself and any subdomain. Case-insensitive.
func matchDomainList(host string, list []string) bool {
	for _, raw := range list {
		entry := strings.ToLower(strings.TrimSpace(raw))
		if entry == "" {
			continue
		}
		if suffix, ok := strings.CutPrefix(entry, "*."); ok {
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}

// isLinkLocal covers 169.254.0.0/16, fe80::/10 and their IPv4-mapped
// IPv6 forms.
func isLinkLocal(ip netip.Addr) bool {
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// IsLoopback reports whether ip is a loopback address (after unmapping).
func IsLoopback(ip netip.Addr) bool {
	if ip.Is4In6() {
		ip = ip.Unmap()
	}
	return ip.IsLoopback()
}
