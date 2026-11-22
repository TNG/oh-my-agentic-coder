package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/audit"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
)

const (
	// mcpRest is the only path a gateway route serves.
	mcpRest = "mcp"
	// mcpDefaultBodyLimit applies when neither the route nor the facade sets one.
	mcpDefaultBodyLimit = 4 << 20
	// mcpArgsSummaryBytes bounds the argument summary for the dialog and the audit log.
	mcpArgsSummaryBytes = 1024
	// jsonRPCServerError is the implementation-defined error code for a refusal.
	jsonRPCServerError = -32000
)

// mcpToolInfo is what the gate learned about a tool call, carried in the
// request context so logAccess can record it.
type mcpToolInfo struct{ tool, class, args string }

type mcpToolKey struct{}

func mcpToolFrom(ctx context.Context) (mcpToolInfo, bool) {
	info, ok := ctx.Value(mcpToolKey{}).(mcpToolInfo)
	return info, ok
}

// handleMCP serves a route that fronts an MCP gateway. The facade is the
// authority for what the agent may call: it parses the message strictly,
// applies the route's policy and forwards the canonical bytes it checked.
func (f *Facade) handleMCP(w http.ResponseWriter, r *http.Request, route *Route, rest string, started time.Time) {
	if rest != mcpRest {
		w.Header().Set("X-Omac-Reason", "unknown-path")
		http.Error(w, "omac: unknown path", http.StatusNotFound)
		f.logAccess(r, route, rest, http.StatusNotFound, 0, time.Since(started))
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodDelete:
		f.proxyHTTP(w, r, route, rest, started)
	case http.MethodPost:
		f.gateMCPPost(w, r, route, started)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "omac: method not allowed", http.StatusMethodNotAllowed)
		f.logAccess(r, route, mcpRest, http.StatusMethodNotAllowed, 0, time.Since(started))
	}
}

func (f *Facade) gateMCPPost(w http.ResponseWriter, r *http.Request, route *Route, started time.Time) {
	reject := func(code int, reason string) {
		w.Header().Set("X-Omac-Reason", reason)
		http.Error(w, "omac: "+reason, code)
		f.logAccess(r, route, mcpRest, code, 0, time.Since(started))
	}
	if r.Header.Get("Content-Encoding") != "" {
		reject(http.StatusUnsupportedMediaType, "mcp-content-encoding")
		return
	}
	limit := route.MaxBodyBytes
	if limit <= 0 {
		limit = f.MaxBodyBytes
	}
	if limit <= 0 {
		limit = mcpDefaultBodyLimit
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		reject(http.StatusBadRequest, "mcp-unreadable-body")
		return
	}
	if int64(len(body)) > limit {
		reject(http.StatusRequestEntityTooLarge, "body-too-large")
		return
	}

	rec := audit.FacadeApprovalInfo{Skill: route.Skill, Method: r.Method, Mount: route.Mount, Namespace: route.Namespace, Path: "/" + mcpRest}
	call, canonical, err := mcppolicy.ParseRequest(body)
	if err != nil {
		rec.Result, rec.Source = "deny", string(SourceMCPUnparseable)
		f.auditApproval(rec)
		w.Header().Set("X-Omac-Reason", mcppolicy.ReasonUnparseable)
		writeJSONRPCError(w, http.StatusBadRequest, nil, mcppolicy.ReasonUnparseable+": "+err.Error())
		f.logAccess(r, route, mcpRest, http.StatusBadRequest, 0, time.Since(started))
		return
	}

	d := route.MCP.Decide(call)
	isTool := call.Method == "tools/call"
	var args string
	if isTool {
		args = mcppolicy.SummarizeArgs(call.Args, mcpArgsSummaryBytes)
		r = r.WithContext(context.WithValue(r.Context(), mcpToolKey{}, mcpToolInfo{tool: call.Tool, class: string(d.Class), args: args}))
		rec.Tool, rec.ToolClass, rec.ToolArgs = call.Tool, string(d.Class), args
	}
	refuse := func(reason, detail string, source ApprovalSource, scope ApprovalScope) {
		rec.Result, rec.Source, rec.Scope = "deny", string(source), string(scope)
		f.auditApproval(rec)
		w.Header().Set("X-Omac-Reason", reason)
		writeJSONRPCError(w, http.StatusOK, call.ID, reason+": "+detail)
		f.logAccess(r, route, mcpRest, http.StatusOK, 0, time.Since(started))
	}
	if !d.Allow {
		refuse(d.Reason, d.Detail, policySource(d.Reason), "")
		return
	}
	retryKey := mcpAllowanceKeyFor(route, call, canonical)
	// allowedUntil is the expiry of the allowance this request uses: the one
	// it took from the cache, or zero for a fresh approver answer.
	var allowedUntil time.Time
	remember := false
	if d.NeedsApproval && isTool {
		allowedUntil, remember = f.mcpAllowances.take(retryKey)
	}
	if remember {
		rec.Result, rec.Source = "allow", string(SourceRetry)
		f.auditApproval(rec)
	} else if d.NeedsApproval {
		verdict := f.askMCP(r, route, call, d.Class, args)
		if !verdict.decision.Allow {
			refuse(verdict.reason, "the call to tool "+call.Tool+" of server "+route.MCP.Server+" was not approved ("+string(verdict.decision.Source)+")",
				verdict.decision.Source, verdict.decision.Scope)
			return
		}
		rec.Result, rec.Source, rec.Scope = "allow", string(verdict.decision.Source), string(verdict.decision.Scope)
		f.auditApproval(rec)
		allowedUntil, remember = time.Time{}, isTool
	} else if isTool {
		rec.Result, rec.Source = "allow", string(SourceDeclared)
		f.auditApproval(rec)
	}

	// The server sees exactly the bytes that were checked.
	r.Body = io.NopCloser(bytes.NewReader(canonical))
	r.ContentLength = int64(len(canonical))
	r.TransferEncoding = nil
	r.Header.Set("Content-Length", strconv.Itoa(len(canonical)))
	forwarded := &statusCaptureWriter{ResponseWriter: w, status: http.StatusOK}
	if remember {
		// Give the allowance back before the client sees the failure, so
		// the bridge's immediate retry finds it.
		forwarded.onStatus = func(status int) {
			if status/100 != 2 {
				f.mcpAllowances.restore(retryKey, allowedUntil)
			}
		}
	}
	f.proxyHTTP(forwarded, r, route, mcpRest, started)
}

type mcpVerdict struct {
	decision ApprovalDecision
	reason   string
}

func (f *Facade) askMCP(r *http.Request, route *Route, call mcppolicy.Call, class mcppolicy.Class, args string) mcpVerdict {
	if f.Approver == nil {
		return mcpVerdict{ApprovalDecision{Source: SourceUnavailable}, "approval-unavailable"}
	}
	d := f.Approver.Approve(r.Context(), ApprovalRequest{
		Skill: route.Skill, Mount: route.Mount, Namespace: route.Namespace,
		Method: r.Method, Path: "/" + mcpRest,
		MCP: &MCPCall{Server: route.MCP.Server, Tool: call.Tool, Class: class, Args: args},
	})
	reason := "approval-denied"
	switch d.Source {
	case SourceTimeout:
		reason = "approval-timeout"
	case SourceUnavailable:
		reason = "approval-unavailable"
	case SourceAbandoned:
		reason = "approval-abandoned"
	}
	return mcpVerdict{d, reason}
}

func policySource(reason string) ApprovalSource {
	if reason == mcppolicy.ReasonAccess {
		return SourceMCPAccess
	}
	return SourceMCPUnlisted
}

// writeJSONRPCError answers with a JSON-RPC error object. The id is left out
// when the request had none.
func writeJSONRPCError(w http.ResponseWriter, status int, id json.RawMessage, message string) {
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id}
	resp.Error.Code = jsonRPCServerError
	resp.Error.Message = "omac: " + message
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
