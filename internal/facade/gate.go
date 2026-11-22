package facade

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
)

// ApprovalRequest is what the gate observed about a request that needs a
// human answer. Everything here comes from the request or from omac's own
// declaration; nothing is taken from the sidecar.
type ApprovalRequest struct {
	Skill string
	Mount string
	// Namespace is the route's namespace. In omac serve it is a project's
	// secret dir token: use it to find the project, never show or store it.
	Namespace string
	Method    string
	Path      string // cleaned, as forwarded
	RawQuery  string // as received; the approver must mask credentials
	// Upgrade is the protocol the request asks to switch to (its Upgrade
	// header, e.g. "websocket"), or "" for a plain request. An allowed
	// upgrade stays open, and its frames are not checked.
	Upgrade string
	// Match is the rule a stored answer is keyed on: the declared pattern,
	// or the exact method and cleaned path for a catch-all entry (ANY or a
	// ** path) and for an unlisted request.
	Match       string
	Description string // the declaration's one-line description
	// Unlisted is true when the declaration has no entry for the request.
	// The cleaned path percent-encodes `{`, `}` and `*`, so an exact Match
	// cannot spell a pattern today; the flag keeps the two apart without
	// relying on that.
	Unlisted bool
	// CatchAll is true when the matched entry is on ANY or a ** path, so
	// Match is the exact route rather than the entry.
	CatchAll bool
	// MCP is set when the request is a tools/call to an MCP gateway route.
	MCP *MCPCall
}

// MCPCall describes the MCP tool call an ApprovalRequest is about.
type MCPCall struct {
	Server, Tool string
	Class        mcppolicy.Class
	// Args is a masked, capped summary of the call's arguments.
	Args string
}

// ApprovalSource says where a decision came from. The gate picks the status
// code from it, so an Approver returns one of these constants.
type ApprovalSource string

const (
	SourceDeclared       ApprovalSource = "declared"        // approval: none
	SourcePrompt         ApprovalSource = "prompt"          // the user answered the dialog
	SourceSession        ApprovalSource = "session"         // an earlier "for this session" answer
	SourceAlways         ApprovalSource = "always"          // a stored "always" answer
	SourceDenyAll        ApprovalSource = "deny-all"        // "deny everything from this skill"
	SourceTimeout        ApprovalSource = "timeout"         // nobody answered in time
	SourceUnavailable    ApprovalSource = "unavailable"     // no dialog could be shown
	SourceAbandoned      ApprovalSource = "abandoned"       // the client went away while held
	SourceInvalidPath    ApprovalSource = "invalid-path"    // the path cannot be cleaned unambiguously
	SourceMCPUnparseable ApprovalSource = "mcp-unparseable" // the MCP message failed strict parsing
	SourceMCPUnlisted    ApprovalSource = "mcp-unlisted"    // the MCP method or tool is not allowed or not declared
	SourceMCPAccess      ApprovalSource = "mcp-access"      // the tool's class is not allowed by the access mode
)

// ApprovalScope is how far a user answer reaches.
type ApprovalScope string

const (
	ScopeOnce    ApprovalScope = "once"
	ScopeSession ApprovalScope = "session"
	ScopeAlways  ApprovalScope = "always"
)

// ApprovalDecision is the answer to an ApprovalRequest.
type ApprovalDecision struct {
	Allow bool
	// Scope is set for a user answer, "" otherwise.
	Scope  ApprovalScope
	Source ApprovalSource
}

// Approver answers the gate for requests that ask: routes declared
// approval: required, and routes the declaration does not list. It
// blocks until there is an answer; ctx ends when the client goes away.
type Approver interface {
	Approve(ctx context.Context, req ApprovalRequest) ApprovalDecision
}

// gate applies the skill's declaration to r, before anything is forwarded.
// On success it returns the cleaned path (without the leading slash) that
// must be forwarded, the same one that was matched. When it returns false it
// has already answered the client.
func (f *Facade) gate(w http.ResponseWriter, r *http.Request, route *Route, rawEscaped string, started time.Time) (string, bool) {
	rec := audit.FacadeApprovalInfo{Skill: route.Skill, Method: r.Method, Mount: route.Mount, Namespace: route.Namespace}
	// A header such as X-HTTP-Method-Override would let a framework in the
	// sidecar run a method other than the one that was approved.
	rec.StrippedHeaders = approval.StripOverrideHeaders(r.Header)
	deny := func(code int, reason string, d ApprovalDecision) (string, bool) {
		rec.Result, rec.Source, rec.Scope = "deny", string(d.Source), string(d.Scope)
		f.auditApproval(rec)
		w.Header().Set("X-Omac-Reason", reason)
		http.Error(w, "omac: "+reason, code)
		f.logAccess(r, route, strings.TrimPrefix(rec.Path, "/"), code, 0, time.Since(started))
		return "", false
	}
	invalidPath := ApprovalDecision{Source: SourceInvalidPath}

	rest, ok := escapedRest(rawEscaped, strings.Split(route.key(), "/"))
	if !ok {
		rec.Path = rawEscaped
		return deny(http.StatusBadRequest, "invalid-path", invalidPath)
	}
	segs, forward, err := approval.CleanPath(rest)
	if err != nil {
		rec.Path = rest
		return deny(http.StatusBadRequest, "invalid-path", invalidPath)
	}
	// The proxy puts rest into URL.Path, which holds the decoded form and is
	// escaped once more when the request is written. forward is already
	// escaped, so passing it would encode it twice ("a%20b" → "a%2520b").
	// CleanPath refuses an encoded "/", so joining the decoded segments
	// gives back the same path.
	rest = strings.Join(segs, "/")
	rec.Path = "/" + rest
	// With an approvals block, a route the block does not list asks too;
	// only an entry with approval: none lets a request through unasked.
	entry, matched := route.Approvals.Lookup(r.Method, segs)
	rec.Unlisted = !matched
	if entry.Approval == approval.None {
		rec.Result, rec.Source = "allow", string(SourceDeclared)
		f.auditApproval(rec)
		return rest, true
	}
	if !matched || entry.IsCatchAll() {
		// A stored answer is keyed on the rule. An unlisted route has none,
		// so it gets its own exact one: "always" for one route must not
		// cover every other route the block does not list. The same holds
		// for a catch-all entry (ANY or a ** path): the user saw one
		// request, so "always" must not stretch to DELETE because they
		// allowed a GET.
		entry.Match = r.Method + " " + forward
	}

	if f.Approver == nil {
		return deny(http.StatusServiceUnavailable, "approval-unavailable", ApprovalDecision{Source: SourceUnavailable})
	}
	upgrade := ""
	if isUpgrade(r) {
		upgrade = r.Header.Get("Upgrade")
	}
	d := f.Approver.Approve(r.Context(), ApprovalRequest{
		Skill: route.Skill, Mount: route.Mount, Namespace: route.Namespace,
		Method: r.Method, Path: forward, RawQuery: r.URL.RawQuery, Upgrade: upgrade,
		Match: entry.Match, Description: entry.Description, Unlisted: !matched, CatchAll: matched && entry.IsCatchAll(),
	})
	if !d.Allow {
		switch d.Source {
		case SourceTimeout:
			return deny(http.StatusForbidden, "approval-timeout", d)
		case SourceUnavailable:
			return deny(http.StatusServiceUnavailable, "approval-unavailable", d)
		case SourceAbandoned:
			// The client is gone; the response is moot but the audit is not.
			return deny(http.StatusForbidden, "approval-abandoned", d)
		default:
			return deny(http.StatusForbidden, "approval-denied", d)
		}
	}
	rec.Result, rec.Source, rec.Scope = "allow", string(d.Source), string(d.Scope)
	f.auditApproval(rec)
	return rest, true
}

func (f *Facade) auditApproval(rec audit.FacadeApprovalInfo) {
	if f.auditor == nil {
		return
	}
	f.auditor.Emit(audit.FacadeApproval(rec))
}

// escapedRest returns the escaped path after the route prefix (namespace
// and mount). ok is false when a prefix segment as received does not decode
// to exactly the expected one: "/echo%2Flive/x" resolves to the route "echo"
// on the decoded path, but its first raw segment is "echo%2Flive".
func escapedRest(rawEscaped string, prefix []string) (rest string, ok bool) {
	i := 0
	for _, want := range prefix {
		for i < len(rawEscaped) && rawEscaped[i] == '/' {
			i++
		}
		start := i
		for i < len(rawEscaped) && rawEscaped[i] != '/' {
			i++
		}
		if got, err := url.PathUnescape(rawEscaped[start:i]); err != nil || got != want {
			return "", false
		}
	}
	if i >= len(rawEscaped) {
		return "/", true
	}
	return rawEscaped[i:], true
}
