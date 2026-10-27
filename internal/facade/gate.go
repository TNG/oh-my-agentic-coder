package facade

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/audit"
)

// ApprovalRequest is what the gate observed about a request that needs a
// human answer. Everything here comes from the request or from omac's own
// declaration; nothing is taken from the sidecar.
type ApprovalRequest struct {
	Skill       string
	Mount       string
	Namespace   string
	Method      string
	Path        string // cleaned, as forwarded
	RawQuery    string // as received; the approver must mask credentials
	Match       string // the declared pattern that matched
	Description string // the declaration's one-line description
}

// ApprovalDecision is the answer to an ApprovalRequest.
type ApprovalDecision struct {
	Allow bool
	// Scope is once|session|always for a user answer, "" otherwise.
	Scope string
	// Source says where the answer came from: prompt, session, always,
	// deny-all, timeout, unavailable, abandoned.
	Source string
}

// Approver answers the gate for endpoints declared approval: required. It
// blocks until there is an answer; ctx ends when the client goes away.
type Approver interface {
	Approve(ctx context.Context, req ApprovalRequest) ApprovalDecision
}

// gate applies the skill's declaration to r, before anything is forwarded.
// On success it returns the cleaned path (without the leading slash) that
// must be forwarded, the same one that was matched. When it returns false it
// has already answered the client.
func (f *Facade) gate(w http.ResponseWriter, r *http.Request, route *Route, rawEscaped string, started time.Time) (string, bool) {
	deny := func(code int, reason, source, path string, decision *ApprovalDecision) (string, bool) {
		scope := ""
		if decision != nil {
			scope = decision.Scope
		}
		f.auditApproval(route, r.Method, path, "deny", source, scope)
		w.Header().Set("X-Omac-Reason", reason)
		http.Error(w, "omac: "+reason, code)
		f.logAccess(r, route, strings.TrimPrefix(path, "/"), code, 0, time.Since(started))
		return "", false
	}

	nPrefix := strings.Count(route.key(), "/") + 1
	segs, forward, err := approval.CleanPath(escapedRest(rawEscaped, nPrefix))
	if err != nil {
		return deny(http.StatusBadRequest, "invalid-path", "invalid-path", "/", nil)
	}
	// A header such as X-HTTP-Method-Override would let a framework in the
	// sidecar run a method other than the one that was approved.
	approval.StripOverrideHeaders(r.Header)

	entry, ok := route.Approvals.Lookup(r.Method, segs)
	if !ok {
		return deny(http.StatusForbidden, "undeclared-endpoint", "undeclared-endpoint", forward, nil)
	}
	rest := strings.TrimPrefix(forward, "/")
	if entry.Approval == approval.None {
		f.auditApproval(route, r.Method, forward, "allow", "declared", "")
		return rest, true
	}

	if f.Approver == nil {
		return deny(http.StatusServiceUnavailable, "approval-unavailable", "unavailable", forward, nil)
	}
	d := f.Approver.Approve(r.Context(), ApprovalRequest{
		Skill: route.Skill, Mount: route.Mount, Namespace: route.Namespace,
		Method: r.Method, Path: forward, RawQuery: r.URL.RawQuery,
		Match: entry.Match, Description: entry.Description,
	})
	if !d.Allow {
		reason := "approval-denied"
		switch d.Source {
		case "timeout":
			reason = "approval-timeout"
		case "unavailable":
			reason = "approval-unavailable"
		case "abandoned":
			// The client is gone; the response is moot but the audit is not.
			reason = "approval-abandoned"
		}
		code := http.StatusForbidden
		if d.Source == "unavailable" {
			code = http.StatusServiceUnavailable
		}
		return deny(code, reason, d.Source, forward, &d)
	}
	f.auditApproval(route, r.Method, forward, "allow", d.Source, d.Scope)
	return rest, true
}

func (f *Facade) auditApproval(route *Route, method, path, result, source, scope string) {
	if f.auditor == nil {
		return
	}
	f.auditor.Emit(audit.FacadeApproval(route.Skill, method, route.Mount, route.Namespace, path, result, source, scope))
}

// escapedRest returns the part of the escaped request path after the first
// nPrefix non-empty segments (the namespace and mount), starting with "/".
func escapedRest(rawEscaped string, nPrefix int) string {
	i := 0
	for n := 0; n < nPrefix; n++ {
		for i < len(rawEscaped) && rawEscaped[i] == '/' {
			i++
		}
		for i < len(rawEscaped) && rawEscaped[i] != '/' {
			i++
		}
	}
	if i >= len(rawEscaped) {
		return "/"
	}
	return rawEscaped[i:]
}
