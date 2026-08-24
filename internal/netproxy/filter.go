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
	"time"

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
	// IntentReason is the agent-declared intent on file for this host at
	// decision time (empty if none). Carried on prompt-driven denials so the
	// deny response can echo it, letting the agent expand on its prior reason
	// without a follow-up GET /sandbox/intent. Body-only: never a header value
	// (it is agent-supplied and unsanitized).
	IntentReason string
	// PromptWaitMS is how long the dialog was open before the user answered
	// (prompt decisions only; 0 otherwise). It is what makes a *silent*
	// failure visible after the fact: the connection is held while the
	// dialog is up, so any client whose own timeout is shorter than this
	// had already given up by the time the allow was granted. See #257.
	PromptWaitMS int64
	// PromptAbandoned marks a verdict whose prompt had already been recorded
	// as abandoned (the proxy shut down while it was open). The answer
	// arrived too late to be acted on, so emitting it as an ordinary
	// decision would contradict the abandoned record for the same request.
	PromptAbandoned bool
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
	// persist=true means the decision was "permanently". ctx carries the
	// per-connection ClientSource (when set) so the prompt can attribute the
	// dial to a process; it does not gate the decision.
	Prompt(ctx context.Context, host string, port int) PromptResult
}

// PromptResult is the parsed outcome of an interactive prompt.
type PromptResult struct {
	Allow   bool
	Persist bool   // permanent (host or suffix scope) vs once
	Session bool   // session-scoped (host or suffix scope) vs once
	Scope   string // "host" or "suffix" when Persist or Session
	Suffix  string // populated when Scope == "suffix"
	// NeedsIntent signals that the user clicked "Explain more" — the
	// request is denied with a marker pointing the agent at the intent
	// endpoint. Never persisted.
	NeedsIntent bool
	// PriorReason is the agent-declared intent on file for this host at
	// prompt time (empty if none). The prompter already looks it up to render
	// the dialog; carrying it here lets a denial echo it back to the agent.
	PriorReason string
}

// DecisionStore keeps prompt decisions so later requests for the same
// host are answered without asking again. The two implementations differ
// only in how long a decision lives: the prompt package's policy file
// store keeps it across runs, SessionStore only until the sandbox exits.
type DecisionStore interface {
	// Lookup returns (verdict, found). Suffix entries match the host
	// itself and any subdomain.
	Lookup(host string) (allow bool, found bool)
	// Record stores a decision, or reports why it could not be kept.
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
	// Learned holds decisions the user made permanent; the prompt
	// package's policy file store keeps them across runs. nil disables
	// permanent decisions.
	Learned DecisionStore
	// Session holds decisions the user scoped to this sandbox session and
	// that are never written to disk. nil disables them. See checkRules
	// for how the two stores rank against the profile lists.
	Session DecisionStore
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
	// port and startedAt describe the prompt for the abandoned-prompt
	// audit event, which is emitted from DrainAbandonedPrompts long after
	// the requesting goroutine may have gone.
	port      int
	startedAt time.Time
	// drained is set by DrainAbandonedPrompts when this prompt was still
	// open at shutdown. Read by the resolution path so a late answer is
	// not also emitted as an ordinary decision. Guarded by promptMu.
	drained bool
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

// DrainAbandonedPrompts records every prompt that is still open, as an
// abandoned prompt, and forgets it.
//
// A prompt outlives the request that raised it on purpose: the dialog runs
// on a detached context (see internal/netprompt, which builds its timeout
// from context.Background()) so a transient client does not kill a dialog
// the user is already reading. The cost is that a tool with a short HTTP
// timeout — the opencode skainet plugin aborts model discovery after 3s,
// while answering a dialog measurably takes longer — gives up before the
// verdict exists. No net.decision is ever logged, so the run reads as
// clean while having silently failed.
//
// Called at proxy shutdown, when any remaining prompt is by definition
// unresolved. Returns the number of prompts drained. See #257.
func (f *Filter) DrainAbandonedPrompts() int {
	f.promptMu.Lock()
	abandoned := make(map[string]*promptWait, len(f.inflight))
	for host, w := range f.inflight {
		// Mark before releasing the lock: the dialog may still be up, and
		// its resolution path checks this to avoid emitting a decision
		// that would contradict the abandoned record.
		w.drained = true
		abandoned[host] = w
		delete(f.inflight, host)
	}
	f.promptMu.Unlock()

	for host, w := range abandoned {
		waited := time.Since(w.startedAt).Milliseconds()
		f.cfg.Logf("omac sandbox: net ABANDONED %s:%d (prompt raised %dms ago, no verdict — the requesting tool stopped waiting)",
			host, w.port, waited)
		f.cfg.Auditor.Emit(audit.NetPromptAbandoned(host, w.port, waited))
	}
	return len(abandoned)
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
	h := NormalizeHost(host)

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
		if v, ok := f.defaultDecision(ctx, h, port); ok {
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
	if v, ok := f.defaultDecision(ctx, h, port); ok {
		if v.Decision == Deny {
			return f.log(h, port, v), nil
		}
		return f.log(h, port, v), safe
	}
	return f.log(h, port, Verdict{Decision: Deny, Reason: "default deny"}), nil
}

// CheckHost runs the admission pipeline WITHOUT local DNS resolution, for
// hosts that will be tunneled through an upstream proxy (which performs
// its own DNS). All hostname rules apply — metadata hard-deny, deny_domain,
// allow_domain, learned rules, and the prompt/default decision. Anti-DNS-
// rebinding IP pinning does not: on the chained path omac never dials the
// pinned IPs, so the hostname the child requested is the admission
// boundary (the upstream proxy resolves it).
func (f *Filter) CheckHost(ctx context.Context, host string, port int) Verdict {
	h := NormalizeHost(host)
	if hardDenyHosts[h] {
		return f.log(h, port, Verdict{Decision: Deny, Reason: "hard-deny metadata host"})
	}
	if ip, err := netip.ParseAddr(h); err == nil && isLinkLocal(ip) {
		return f.log(h, port, Verdict{Decision: Deny, Reason: "hard-deny link-local address"})
	}
	if v := f.checkRules(h); v != nil {
		return f.log(h, port, *v)
	}
	if v, ok := f.defaultDecision(ctx, h, port); ok {
		return f.log(h, port, v)
	}
	return f.log(h, port, Verdict{Decision: Deny, Reason: "default deny"})
}

// checkRules evaluates the stored and static rules. A remembered deny is
// checked before the profile lists and a remembered allow after, so a
// deny the user gave at the prompt outranks allow_domain while an allow
// never overrides an explicit profile rule. Returns nil when no rule
// matches.
func (f *Filter) checkRules(host string) *Verdict {
	learnedAllow, learnedFound := lookupDecision(f.cfg.Learned, host)
	sessionAllow, sessionFound := lookupDecision(f.cfg.Session, host)

	switch {
	case learnedFound && !learnedAllow:
		return &Verdict{Decision: Deny, Reason: "learned permanent deny", Persisted: true}
	case sessionFound && !sessionAllow:
		return &Verdict{Decision: Deny, Reason: "session deny"}
	case MatchDomainList(host, f.cfg.DenyDomains):
		return &Verdict{Decision: Deny, Reason: "deny_domain"}
	case MatchDomainList(host, f.cfg.AllowDomains):
		return &Verdict{Decision: Allow, Reason: "allow_domain"}
	case sessionFound && sessionAllow:
		return &Verdict{Decision: Allow, Reason: "session allow"}
	case learnedFound && learnedAllow:
		return &Verdict{Decision: Allow, Reason: "learned permanent allow", Persisted: true}
	}
	return nil
}

// lookupDecision treats a nil store as "no decision".
func lookupDecision(s DecisionStore, host string) (allow, found bool) {
	if s == nil {
		return false, false
	}
	return s.Lookup(host)
}

// recordDecision stores a prompt result in s and reports whether it will
// actually be replayed. A nil store and a failed upsert are the same
// outcome for the caller: the decision applies to this request only.
func (f *Filter) recordDecision(s DecisionStore, host string, res PromptResult, what string) bool {
	if s == nil {
		return false
	}
	target := host
	if res.Scope == "suffix" && res.Suffix != "" {
		target = res.Suffix
	}
	if err := s.Record(target, res.Scope, res.Allow); err != nil {
		f.cfg.Logf("omac sandbox: warning: %s: %v", what, err)
		return false
	}
	return true
}

// defaultDecision handles step 5. ok=false means "no decision" (treat
// as deny).
func (f *Filter) defaultDecision(ctx context.Context, host string, port int) (Verdict, bool) {
	if f.cfg.PromptEnabled {
		res, waited, drained, prompted := f.promptCoalesced(ctx, host, port)
		if !prompted {
			if f.cfg.OnUnavailableAllow {
				return Verdict{Decision: Allow, Reason: "prompt unavailable: on_unavailable=allow"}, true
			}
			return Verdict{Decision: Deny, Reason: "prompt unavailable: on_unavailable=deny"}, true
		}
		if res.Persist {
			f.recordDecision(f.cfg.Learned, host, res, "persist learned decision")
		}
		// A session decision that was not stored applies once only; the
		// audit event must not claim a scope the filter will not honour.
		// (The learned path reports res.Persist regardless — see
		// TestPromptDecisionRecordsScopeAndPersisted.)
		kept := res.Session && f.recordDecision(f.cfg.Session, host, res, "record session decision")
		scope := res.Scope
		if !res.Persist && !kept {
			scope = "once"
		}
		if res.NeedsIntent {
			return Verdict{Decision: Deny, Reason: "prompt:needs_intent", Scope: scope, Persisted: res.Persist, IntentReason: res.PriorReason, PromptWaitMS: waited, PromptAbandoned: drained}, true
		}
		if res.Allow {
			return Verdict{Decision: Allow, Reason: "prompt:allow", Scope: scope, Persisted: res.Persist, PromptWaitMS: waited, PromptAbandoned: drained}, true
		}
		return Verdict{Decision: Deny, Reason: "prompt:deny", Scope: scope, Persisted: res.Persist, IntentReason: res.PriorReason, PromptWaitMS: waited, PromptAbandoned: drained}, true
	}
	if len(f.cfg.AllowDomains) > 0 {
		return Verdict{Decision: Deny, Reason: "not in allowlist"}, true
	}
	// Pure blocklist (or no rules at all): allow.
	return Verdict{Decision: Allow, Reason: "default allow (blocklist mode)"}, true
}

// promptCoalesced ensures concurrent requests for the same host share
// one dialog. prompted=false means no prompter is available.
//
// waitedMS is how long the dialog was open, and drained reports whether it
// had already been recorded as abandoned (see DrainAbandonedPrompts) by the
// time it was answered. Both travel onto the Verdict so the audit trail can
// distinguish "allowed promptly" from "allowed long after the requesting
// tool gave up".
func (f *Filter) promptCoalesced(ctx context.Context, host string, port int) (res PromptResult, waitedMS int64, drained bool, prompted bool) {
	if f.cfg.Prompter == nil {
		return PromptResult{}, 0, false, false
	}
	f.promptMu.Lock()
	if w, ok := f.inflight[host]; ok {
		f.promptMu.Unlock()
		if f.cfg.onCoalesceWait != nil {
			f.cfg.onCoalesceWait()
		}
		<-w.done
		f.promptMu.Lock()
		wasDrained := w.drained
		f.promptMu.Unlock()
		return w.res, time.Since(w.startedAt).Milliseconds(), wasDrained, true
	}
	w := &promptWait{done: make(chan struct{}), port: port, startedAt: time.Now()}
	f.inflight[host] = w
	f.promptMu.Unlock()

	w.res = f.cfg.Prompter.Prompt(ctx, host, port)

	f.promptMu.Lock()
	wasDrained := w.drained
	delete(f.inflight, host)
	f.promptMu.Unlock()
	close(w.done)
	return w.res, time.Since(w.startedAt).Milliseconds(), wasDrained, true
}

func (f *Filter) log(host string, port int, v Verdict) Verdict {
	word := "DENY"
	if v.Decision == Allow {
		word = "ALLOW"
	}
	f.cfg.Logf("omac sandbox: net %s %s:%d (%s)", word, host, port, v.Reason)
	if v.PromptAbandoned {
		// Already recorded as abandoned at shutdown. Emitting a decision now
		// would put two contradictory records in the trail for one request,
		// and the abandoned one is the accurate account: the requester was
		// gone before this answer existed.
		f.cfg.Logf("omac sandbox: net %s %s:%d answered %dms after the prompt was abandoned — not recorded as a decision",
			word, host, port, v.PromptWaitMS)
		return v
	}
	source := classifyReason(v.Reason)
	scope := v.Scope
	persisted := v.Persisted
	f.cfg.Auditor.Emit(audit.NetDecision(host, port, v.Decision == Allow, scope, source, persisted, v.PromptWaitMS))
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
	case strings.HasPrefix(reason, "session"):
		return "session"
	default:
		return "default"
	}
}

// MatchDomainList reports whether host matches any entry. Entries are
// exact hostnames or "*.suffix" wildcards; a wildcard matches the
// suffix itself and any subdomain. Case-insensitive.
func MatchDomainList(host string, list []string) bool {
	for _, raw := range list {
		entry := strings.ToLower(strings.TrimSpace(raw))
		if entry == "" {
			continue
		}
		suffix, wildcard := strings.CutPrefix(entry, "*.")
		if matchHostOrSuffix(host, suffix, wildcard) {
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
