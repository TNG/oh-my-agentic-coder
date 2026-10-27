package approver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
)

// Dialog labels, in display order. Deny is the preselected default.
const (
	LabelOnce    = "Allow once"
	LabelSession = "Allow for this session"
	LabelAlways  = "Allow always (this endpoint of this skill)"
	LabelDeny    = "Deny"
	LabelDenyAll = "Deny everything from this skill for this session"
)

// DefaultTimeout is how long a dialog waits for an answer. No answer is deny.
const DefaultTimeout = 60 * time.Second

// Chooser is the native dialog (see netprompt.Chooser).
type Chooser interface {
	Choose(ctx context.Context, title, text string, labels []string, def string) (string, error)
	Notify(title, body string)
}

// Config wires an Approver.
type Config struct {
	Chooser Chooser
	Store   *Store
	// Timeout bounds one dialog; zero means DefaultTimeout.
	Timeout time.Duration
	// ScopeFor maps a route namespace to what verdicts are bound to (a
	// workdir hash for workdir-bound skills). Nil means one scope for all.
	ScopeFor func(namespace string) string
	// Logf receives diagnostics; nil discards them.
	Logf func(format string, args ...any)
}

// Approver implements facade.Approver with a native dialog, a session store
// and the persisted always-store. Dialogs are shown one at a time.
type Approver struct {
	cfg Config
	sem chan struct{} // one dialog at a time; a waiting request can still be abandoned

	mu       sync.Mutex
	session  map[string]bool     // verdict id -> allowed for this session
	denyAll  map[string]bool     // scope+skill -> everything denied for this session
	inflight map[string]*pending // identical requests share one dialog
}

type pending struct {
	done chan struct{}
	dec  facade.ApprovalDecision
}

// New returns an Approver.
func New(cfg Config) *Approver {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Approver{
		cfg:      cfg,
		sem:      make(chan struct{}, 1),
		session:  map[string]bool{},
		denyAll:  map[string]bool{},
		inflight: map[string]*pending{},
	}
}

func (a *Approver) scope(ns string) string {
	if a.cfg.ScopeFor == nil {
		return ""
	}
	return a.cfg.ScopeFor(ns)
}

// known answers from stored state: a deny-everything for the skill, a session
// verdict or an always-verdict.
func (a *Approver) known(skillKey, id string) (facade.ApprovalDecision, bool) {
	a.mu.Lock()
	if a.denyAll[skillKey] {
		a.mu.Unlock()
		return facade.ApprovalDecision{Source: "deny-all"}, true
	}
	if a.session[id] {
		a.mu.Unlock()
		return facade.ApprovalDecision{Allow: true, Scope: "session", Source: "session"}, true
	}
	a.mu.Unlock()
	if a.cfg.Store != nil {
		ok, err := a.cfg.Store.Has(id)
		if err != nil {
			a.cfg.Logf("omac approvals: %v (asking instead)", err)
		} else if ok {
			return facade.ApprovalDecision{Allow: true, Scope: "always", Source: "always"}, true
		}
	}
	return facade.ApprovalDecision{}, false
}

// Approve implements facade.Approver.
func (a *Approver) Approve(ctx context.Context, req facade.ApprovalRequest) facade.ApprovalDecision {
	scope := a.scope(req.Namespace)
	skillKey := scope + "\x00" + req.Skill
	id := VerdictID(scope, req.Skill, req.Match, "required")
	if d, ok := a.known(skillKey, id); ok {
		return d
	}

	// Identical requests that are pending right now share the leader's
	// dialog: a denial is returned to all of them, any other answer is
	// re-read from the stored state.
	flight := strings.Join([]string{skillKey, req.Method, req.Path, req.RawQuery}, "\x00")
	a.mu.Lock()
	if p, ok := a.inflight[flight]; ok {
		a.mu.Unlock()
		select {
		case <-p.done:
		case <-ctx.Done():
			return facade.ApprovalDecision{Source: "abandoned"}
		}
		if !p.dec.Allow && p.dec.Source == "prompt" {
			return p.dec
		}
		if d, ok := a.known(skillKey, id); ok {
			return d
		}
		return a.Approve(ctx, req)
	}
	p := &pending{done: make(chan struct{})}
	a.inflight[flight] = p
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.inflight, flight)
		a.mu.Unlock()
		close(p.done)
	}()

	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-ctx.Done():
		p.dec = facade.ApprovalDecision{Source: "abandoned"}
		return p.dec
	}
	// Answered while this request was queued behind another dialog.
	if d, ok := a.known(skillKey, id); ok {
		p.dec = d
		return d
	}
	p.dec = a.ask(ctx, req, skillKey, id, scope)
	return p.dec
}

func (a *Approver) ask(ctx context.Context, req facade.ApprovalRequest, skillKey, id, scope string) facade.ApprovalDecision {
	if a.cfg.Chooser == nil {
		return facade.ApprovalDecision{Source: "unavailable"}
	}
	a.cfg.Chooser.Notify("omac: approval needed", fmt.Sprintf("%s wants to %s %s", req.Skill, req.Method, req.Path))
	dctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	label, err := a.cfg.Chooser.Choose(dctx, "omac: approve sidecar request", DialogText(req),
		[]string{LabelOnce, LabelSession, LabelAlways, LabelDeny, LabelDenyAll}, LabelDeny)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return facade.ApprovalDecision{Source: "abandoned"}
		case dctx.Err() != nil:
			return facade.ApprovalDecision{Source: "timeout"}
		default:
			a.cfg.Logf("omac approvals: dialog failed: %v", err)
			return facade.ApprovalDecision{Source: "unavailable"}
		}
	}
	switch label {
	case LabelOnce:
		return facade.ApprovalDecision{Allow: true, Scope: "once", Source: "prompt"}
	case LabelSession:
		a.mu.Lock()
		a.session[id] = true
		a.mu.Unlock()
		return facade.ApprovalDecision{Allow: true, Scope: "session", Source: "prompt"}
	case LabelAlways:
		v := Verdict{ID: id, Scope: scope, Skill: req.Skill, Match: req.Match, Approval: "required", CreatedAt: time.Now().UTC()}
		if a.cfg.Store == nil || a.cfg.Store.Add(v) != nil {
			// Could not persist: honor the answer for this session only.
			a.cfg.Logf("omac approvals: could not store the always-verdict for %s %s; allowing for this session", req.Skill, req.Match)
			a.mu.Lock()
			a.session[id] = true
			a.mu.Unlock()
			return facade.ApprovalDecision{Allow: true, Scope: "session", Source: "prompt"}
		}
		return facade.ApprovalDecision{Allow: true, Scope: "always", Source: "prompt"}
	case LabelDenyAll:
		a.mu.Lock()
		a.denyAll[skillKey] = true
		a.mu.Unlock()
		return facade.ApprovalDecision{Scope: "session", Source: "deny-all"}
	default: // Deny, Cancel, anything unexpected
		return facade.ApprovalDecision{Scope: "once", Source: "prompt"}
	}
}

// DialogText is built only from what omac observed and declared; the
// description is the skill author's one sentence and is shown as such.
func DialogText(r facade.ApprovalRequest) string {
	var b strings.Builder
	b.WriteString("The sandboxed agent wants a skill's sidecar to do this:\n\n")
	fmt.Fprintf(&b, "    Skill:    %s\n", clip(r.Skill, 80))
	fmt.Fprintf(&b, "    Request:  %s %s\n", clip(r.Method, 16), clip(r.Path, 160))
	if q := MaskQuery(r.RawQuery); q != "" {
		fmt.Fprintf(&b, "    Query:    %s\n", q)
	}
	if r.Description != "" {
		fmt.Fprintf(&b, "    Declared: %s\n", clip(r.Description, 200))
	}
	b.WriteString("    Body:     not shown\n\n")
	b.WriteString("Allow it?")
	return b.String()
}

// MaskQuery renders a raw query string for the dialog: values of
// credential-like keys are replaced, long values are cut.
func MaskQuery(raw string) string {
	if raw == "" {
		return ""
	}
	var parts []string
	for _, kv := range strings.Split(raw, "&") {
		if kv == "" {
			continue
		}
		k, v, has := strings.Cut(kv, "=")
		if has && credentialLike(k) {
			v = "***"
		}
		if has {
			parts = append(parts, clip(k, 40)+"="+clip(v, 60))
		} else {
			parts = append(parts, clip(k, 60))
		}
	}
	return clip(strings.Join(parts, "&"), 300)
}

func credentialLike(key string) bool {
	k := strings.ToLower(key)
	for _, w := range []string{"token", "key", "secret", "pass", "auth", "sig", "cred", "session", "cookie", "bearer"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
