package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
)

const (
	internalIDPrefix   = "omac-gw-"
	reviewCheckTimeout = 30 * time.Second
	maxNamedTools      = 20
	errCodePolicy      = -32001

	reviewStateReviewed    = "reviewed"
	reviewStateNotReviewed = "not-reviewed"
	reviewStateChanged     = "changed"
)

// reviewCheck is the outcome of comparing one session's live tool list with the
// reviewed one.
type reviewCheck struct {
	state   string
	changes mcppolicy.Changes
}

func (c reviewCheck) ok() bool { return c.state == reviewStateReviewed }

// refusal is why the gateway will not pass a message on.
type refusal struct{ reason, detail string }

func (r refusal) message() string { return "omac: " + r.detail + " (" + r.reason + ")" }

func internalRequestID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return internalIDPrefix + hex.EncodeToString(b), nil
}

func isInternalID(id json.RawMessage) bool {
	var s string
	return json.Unmarshal(id, &s) == nil && strings.HasPrefix(s, internalIDPrefix)
}

// Call sends a request on the session under a fresh internal id and waits for
// its result. The response is routed to this caller only, never to the agent.
func (s *session) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id, err := internalRequestID()
	if err != nil {
		return nil, err
	}
	key := strconv.Quote(id)
	reply, err := s.register(key)
	if err != nil {
		return nil, err
	}
	defer s.unregister(key)
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if err := s.send(req); err != nil {
		return nil, err
	}
	select {
	case resp := <-reply:
		var m struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(resp, &m); err != nil {
			return nil, err
		}
		if m.Error != nil {
			return nil, errors.New(m.Error.Message)
		}
		return m.Result, nil
	case <-s.done:
		return nil, errors.New("server process exited")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// reviewStatus returns the review check of the session, fetching the live tool list
// when none is current: before the first use and after the server announced
// that its tools changed.
func (s *session) reviewStatus(ctx context.Context, p *mcppolicy.Policy) (reviewCheck, error) {
	s.checkMu.Lock()
	defer s.checkMu.Unlock()
	s.mu.Lock()
	cur, valid, gen := s.review, s.reviewValid, s.toolsGen
	s.mu.Unlock()
	if valid {
		return cur, nil
	}
	ctx, cancel := context.WithTimeout(ctx, reviewCheckTimeout)
	defer cancel()
	live, err := mcppolicy.ListTools(ctx, s)
	if err != nil {
		return reviewCheck{}, err
	}
	res := reviewCheck{state: reviewStateReviewed}
	switch changes := mcppolicy.Compare(p.Review, live); {
	case p.Review == nil:
		res = reviewCheck{state: reviewStateNotReviewed, changes: changes}
	case !changes.Empty():
		res = reviewCheck{state: reviewStateChanged, changes: changes}
	}
	s.mu.Lock()
	s.review = res
	s.reviewValid = s.toolsGen == gen
	s.mu.Unlock()
	return res, nil
}

func (s *session) toolsChanged() {
	s.mu.Lock()
	s.toolsGen++
	s.reviewValid = false
	s.mu.Unlock()
}

func (s *session) currentReviewState() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.review.state, s.reviewValid
}

func isToolsListChanged(line []byte) bool {
	var m message
	return json.Unmarshal(line, &m) == nil && m.Method == "notifications/tools/list_changed" && !m.hasID()
}

func quoteNames(names []string) string {
	shown := names
	if len(shown) > maxNamedTools {
		shown = shown[:maxNamedTools]
	}
	q := make([]string, len(shown))
	for i, n := range shown {
		q[i] = strconv.QuoteToASCII(n)
	}
	out := strings.Join(q, ", ")
	if len(names) > len(shown) {
		out += fmt.Sprintf(" and %d more", len(names)-len(shown))
	}
	return out
}

func (c reviewCheck) describe(server string) refusal {
	if c.state == reviewStateNotReviewed {
		return refusal{mcppolicy.ReasonUnreviewed, fmt.Sprintf("the tools of MCP server %s have not been reviewed yet, so no tool can be called. Run \"omac mcp tools %s --classify\" on the host.", server, server)}
	}
	var parts []string
	for _, p := range []struct {
		label string
		names []string
	}{{"added", c.changes.Added}, {"removed", c.changes.Removed}, {"changed", c.changes.Changed}} {
		if len(p.names) > 0 {
			parts = append(parts, p.label+": "+quoteNames(p.names))
		}
	}
	return refusal{mcppolicy.ReasonChanged, fmt.Sprintf("the tools of MCP server %s changed since they were last reviewed (%s), so no tool can be called. Run \"omac mcp review %s\" on the host to review them.", server, strings.Join(parts, "; "), server)}
}

// enforce is the gateway's own check, independent of the facade's: the
// method and tool policy for every message, and the review check before a tool
// is listed or called.
func (g *gateway) enforce(ctx context.Context, s *session, call mcppolicy.Call) *refusal {
	p := g.opts.Policy
	if d := p.Decide(call); !d.Allow {
		return &refusal{d.Reason, d.Detail}
	}
	if call.Method != "tools/call" && call.Method != "tools/list" {
		return nil
	}
	check, err := s.reviewStatus(ctx, p)
	if err != nil {
		if call.Method == "tools/list" {
			return &refusal{mcppolicy.ReasonChanged, "cannot read the tool list of MCP server " + p.Server + ": " + err.Error()}
		}
		return &refusal{mcppolicy.ReasonChanged, "cannot verify the tool list of MCP server " + p.Server + ": " + err.Error()}
	}
	if call.Method == "tools/call" && !check.ok() {
		r := check.describe(p.Server)
		return &r
	}
	return nil
}

func policyErrorReply(id json.RawMessage, r refusal) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": errCodePolicy, "message": r.message(), "data": map[string]string{"reason": r.reason}},
	})
	return b
}

// filterToolsList keeps the tools the agent may see and that still match the
// reviewed list. A reply it cannot parse is replaced by an error.
func (g *gateway) filterToolsList(id json.RawMessage, resp []byte) []byte {
	var env map[string]json.RawMessage
	if json.Unmarshal(resp, &env) != nil {
		return policyErrorReply(id, refusal{mcppolicy.ReasonUnparseable, "unreadable tools/list reply"})
	}
	if _, isError := env["error"]; isError {
		return resp
	}
	var result map[string]json.RawMessage
	var tools []json.RawMessage
	if json.Unmarshal(env["result"], &result) != nil || json.Unmarshal(result["tools"], &tools) != nil {
		return policyErrorReply(id, refusal{mcppolicy.ReasonUnparseable, "unreadable tools/list reply"})
	}
	p := g.opts.Policy
	kept := make([]json.RawMessage, 0, len(tools))
	for _, raw := range tools {
		var t struct {
			Name string `json:"name"`
		}
		hash, err := mcppolicy.ToolHash(raw)
		if err != nil || json.Unmarshal(raw, &t) != nil || p.Review == nil {
			continue
		}
		if p.Visible(t.Name) && p.Review.Tools[t.Name] == hash {
			kept = append(kept, raw)
		}
	}
	result["tools"], _ = json.Marshal(kept)
	env["result"], _ = json.Marshal(result)
	out, err := json.Marshal(env)
	if err != nil {
		return policyErrorReply(id, refusal{mcppolicy.ReasonUnparseable, "unreadable tools/list reply"})
	}
	return out
}

// reviewState summarizes the gateway for /status: not-reviewed when the definition
// has no review, changed when any open session saw the tools differ, else reviewed.
func (g *gateway) reviewState() string {
	if g.opts.Policy.Review == nil {
		return reviewStateNotReviewed
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.sessions {
		if state, valid := s.currentReviewState(); valid && state == reviewStateChanged {
			return reviewStateChanged
		}
	}
	return reviewStateReviewed
}

func refuseHTTP(w http.ResponseWriter, status int, reason, text string) {
	w.Header().Set("X-Omac-Reason", reason)
	http.Error(w, text, status)
}
