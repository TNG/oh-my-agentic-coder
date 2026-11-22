package approver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/TNG/oh-my-agentic-coder/internal/facade"
)

// Dialog labels, in display order. Deny is the preselected default.
const (
	LabelOnce       = "Allow once"
	LabelSession    = "Allow for this session"
	LabelAlways     = "Allow always (this endpoint of this skill)"
	LabelAlwaysTool = "Allow always (this tool of this server)"
	LabelDeny       = "Deny"
	LabelDenyAll    = "Deny everything from this skill for this session"
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
	// ScopeFor maps a route namespace to the scope answers are bound to. A
	// scope with an empty Key, or a nil ScopeFor, means the request cannot
	// be tied to a project: it is denied and nothing is stored.
	ScopeFor func(namespace string) Scope
	// Logf receives diagnostics; nil discards them.
	Logf func(format string, args ...any)
}

// Scope is what stored answers are bound to.
type Scope struct {
	// Key identifies the scope in the store, e.g. "wd-<hash of the
	// project directory>" or "global" for a skill every project shares.
	Key string
	// Dir is the project directory, for `omac approvals list`; "" for a
	// scope that is not one project.
	Dir string
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

func (a *Approver) scope(ns string) Scope {
	if a.cfg.ScopeFor == nil {
		return Scope{}
	}
	return a.cfg.ScopeFor(ns)
}

// known answers from stored state: a deny-everything for the skill, a session
// verdict or an always-verdict.
func (a *Approver) known(skillKey, id string) (facade.ApprovalDecision, bool) {
	a.mu.Lock()
	if a.denyAll[skillKey] {
		a.mu.Unlock()
		return facade.ApprovalDecision{Source: facade.SourceDenyAll}, true
	}
	if a.session[id] {
		a.mu.Unlock()
		return facade.ApprovalDecision{Allow: true, Scope: facade.ScopeSession, Source: facade.SourceSession}, true
	}
	a.mu.Unlock()
	if a.cfg.Store != nil {
		ok, err := a.cfg.Store.Has(id)
		if err != nil {
			a.cfg.Logf("omac approvals: %v (asking instead)", err)
		} else if ok {
			return facade.ApprovalDecision{Allow: true, Scope: facade.ScopeAlways, Source: facade.SourceAlways}, true
		}
	}
	return facade.ApprovalDecision{}, false
}

// Approve implements facade.Approver.
func (a *Approver) Approve(ctx context.Context, req facade.ApprovalRequest) facade.ApprovalDecision {
	scope := a.scope(req.Namespace)
	if scope.Key == "" {
		// The request cannot be tied to a project, so no answer may be
		// stored for it or read from another project's answers.
		a.cfg.Logf("omac approvals: cannot tell which project %s %s belongs to; denied", req.Method, req.Path)
		return facade.ApprovalDecision{Source: facade.SourceUnavailable}
	}
	skill, match := verdictSubject(req)
	skillKey := scope.Key + "\x00" + skill
	id := VerdictID(scope.Key, skill, match, ruleKind(req))
	if d, ok := a.known(skillKey, id); ok {
		return d
	}

	// Identical requests that are pending right now share the leader's
	// dialog: a denial, timeout or missing dialog is returned to all of them,
	// so one unanswered dialog is not shown again for each waiting copy. A
	// session or always allow is re-read from the stored state. "Allow once"
	// covers only the leader's request, so each waiting copy asks again, as
	// it does when the leader's own client went away.
	flight := flightKey(skillKey, req)
	a.mu.Lock()
	if p, ok := a.inflight[flight]; ok {
		a.mu.Unlock()
		select {
		case <-p.done:
		case <-ctx.Done():
			return facade.ApprovalDecision{Source: facade.SourceAbandoned}
		}
		if !p.dec.Allow && p.dec.Source != facade.SourceAbandoned {
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
		p.dec = facade.ApprovalDecision{Source: facade.SourceAbandoned}
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

func (a *Approver) ask(ctx context.Context, req facade.ApprovalRequest, skillKey, id string, scope Scope) facade.ApprovalDecision {
	if a.cfg.Chooser == nil {
		return facade.ApprovalDecision{Source: facade.SourceUnavailable}
	}
	skill, match := verdictSubject(req)
	title, notice, labels := "omac: approve sidecar request",
		fmt.Sprintf("%s wants to %s %s", req.Skill, printableASCII(req.Method), clip(printableASCII(req.Path), 160)),
		[]string{LabelOnce, LabelSession, LabelAlways, LabelDeny, LabelDenyAll}
	if req.MCP != nil {
		title = "omac: approve MCP tool call"
		notice = fmt.Sprintf("%s wants to call %s", oneLine(clip(req.MCP.Server, 80)), clip(printableASCII(req.MCP.Tool), 80))
		labels = []string{LabelOnce, LabelSession, LabelAlwaysTool, LabelDeny}
	}
	a.cfg.Chooser.Notify("omac: approval needed", notice)
	dctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	label, err := a.cfg.Chooser.Choose(dctx, title, DialogText(req), labels, LabelDeny)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return facade.ApprovalDecision{Source: facade.SourceAbandoned}
		case dctx.Err() != nil:
			return facade.ApprovalDecision{Source: facade.SourceTimeout}
		default:
			a.cfg.Logf("omac approvals: dialog failed: %v", err)
			return facade.ApprovalDecision{Source: facade.SourceUnavailable}
		}
	}
	switch label {
	case LabelOnce:
		return facade.ApprovalDecision{Allow: true, Scope: facade.ScopeOnce, Source: facade.SourcePrompt}
	case LabelSession:
		a.mu.Lock()
		a.session[id] = true
		a.mu.Unlock()
		return facade.ApprovalDecision{Allow: true, Scope: facade.ScopeSession, Source: facade.SourcePrompt}
	case LabelAlways, LabelAlwaysTool:
		v := Verdict{ID: id, Scope: scope.Key, Dir: scope.Dir, Skill: skill, Match: match, Approval: ruleKind(req), CreatedAt: time.Now().UTC()}
		err := errors.New("no verdict store configured")
		if a.cfg.Store != nil {
			err = a.cfg.Store.Add(v)
		}
		if err != nil {
			a.cfg.Logf("omac approvals: could not store the always-verdict for %s %s: %v; allowing for this session", skill, match, err)
			a.mu.Lock()
			a.session[id] = true
			a.mu.Unlock()
			return facade.ApprovalDecision{Allow: true, Scope: facade.ScopeSession, Source: facade.SourcePrompt}
		}
		return facade.ApprovalDecision{Allow: true, Scope: facade.ScopeAlways, Source: facade.SourcePrompt}
	case LabelDenyAll:
		a.mu.Lock()
		a.denyAll[skillKey] = true
		a.mu.Unlock()
		return facade.ApprovalDecision{Scope: facade.ScopeSession, Source: facade.SourceDenyAll}
	default: // Deny, Cancel, anything unexpected
		return facade.ApprovalDecision{Scope: facade.ScopeOnce, Source: facade.SourcePrompt}
	}
}

// The kinds of rule an answer is stored under, besides the rule text. An
// answer for one kind is never read as an answer for another.
const (
	KindRequired = "required" // a declared entry
	KindUnlisted = "unlisted" // an exact route the skill does not declare
	KindCatchAll = "catchall" // an exact route under a declared ANY or ** entry
	KindMCPTool  = "mcp-tool" // one tool of an MCP server, in one class
)

// verdictSubject is the skill and match a stored answer is keyed by. For an
// MCP call the server stands in for the skill, under a prefix a skill name
// cannot collide with in the same kind, and the class is part of the match so
// a reclassified tool asks again.
func verdictSubject(r facade.ApprovalRequest) (skill, match string) {
	if r.MCP == nil {
		return r.Skill, r.Match
	}
	return "mcp:" + r.MCP.Server, fmt.Sprintf("tool %s (%s)", r.MCP.Tool, r.MCP.Class)
}

// flightKey groups concurrent requests that one dialog may answer. An MCP
// call is grouped by tool, class and arguments, so a denial never reaches a
// call of another tool or with other arguments.
func flightKey(skillKey string, r facade.ApprovalRequest) string {
	if r.MCP != nil {
		args := sha256.Sum256([]byte(r.MCP.Args))
		return strings.Join([]string{skillKey, KindMCPTool, r.MCP.Tool, string(r.MCP.Class), hex.EncodeToString(args[:])}, "\x00")
	}
	return strings.Join([]string{skillKey, r.Method, r.Path, r.RawQuery}, "\x00")
}

func ruleKind(r facade.ApprovalRequest) string {
	switch {
	case r.MCP != nil:
		return KindMCPTool
	case r.Unlisted:
		return KindUnlisted
	case r.CatchAll:
		return KindCatchAll
	}
	return KindRequired
}

// DialogText is built only from what omac observed and declared; the
// description is the skill author's one sentence and is shown as such.
// Every value stays on its own line: the agent's method, path and query are
// shown as printable ASCII, and the skill's text has its line breaks
// flattened. Otherwise either could add a line that looks like omac's, such
// as a second "Declared:" or "Covers:".
func DialogText(r facade.ApprovalRequest) string {
	var b strings.Builder
	if r.MCP != nil {
		return mcpDialogText(r.MCP)
	}
	b.WriteString("The sandboxed agent wants a skill's sidecar to do this:\n\n")
	fmt.Fprintf(&b, "    Skill:    %s\n", oneLine(clip(r.Skill, 80)))
	fmt.Fprintf(&b, "    Request:  %s %s\n", clip(printableASCII(r.Method), 16), clip(printableASCII(r.Path), 160))
	if q := MaskQuery(printableASCII(r.RawQuery)); q != "" {
		fmt.Fprintf(&b, "    Query:    %s\n", q)
	}
	if r.Upgrade != "" {
		fmt.Fprintf(&b, "    Upgrade:  %s; the connection stays open and its messages are not checked\n", clip(printableASCII(r.Upgrade), 32))
	}
	switch {
	case r.Unlisted:
		b.WriteString("    Declared: no, the skill does not list this route\n")
	case r.Description != "":
		fmt.Fprintf(&b, "    Declared: %s\n", oneLine(clip(r.Description, 200)))
	}
	if !r.Unlisted && r.Match != r.Method+" "+r.Path {
		// "for the session" and "always" cover the whole declared entry,
		// not just the request above.
		fmt.Fprintf(&b, "    Covers:   %s\n", oneLine(clip(r.Match, 160)))
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

// printableASCII percent-escapes every byte outside "!" to "~", so text the
// agent chose has no spaces, line breaks or look-alike characters:
// "x=1\u3000Declared:" becomes "x=1%E3%80%80Declared:".
func printableASCII(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if c := s[i]; c > ' ' && c < 0x7f {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// oneLine replaces line breaks, other whitespace, control and format
// characters with a plain space.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// mcpDialogText shows the agent-chosen tool and arguments as printable ASCII
// for the same reason as the sidecar dialog.
func mcpDialogText(c *facade.MCPCall) string {
	var b strings.Builder
	b.WriteString("The sandboxed agent wants to call a tool of an MCP server:\n\n")
	fmt.Fprintf(&b, "    Server:   %s\n", oneLine(clip(c.Server, 80)))
	fmt.Fprintf(&b, "    Tool:     %s\n", clip(printableASCII(c.Tool), 120))
	fmt.Fprintf(&b, "    Class:    %s\n", clip(printableASCII(string(c.Class)), 16))
	fmt.Fprintf(&b, "    Args:     %s\n", clip(printableASCII(c.Args), 400))
	fmt.Fprintf(&b, "    Covers:   tool %s of server %s\n\n", clip(printableASCII(c.Tool), 120), oneLine(clip(c.Server, 80)))
	b.WriteString("Allow it?")
	return b.String()
}
