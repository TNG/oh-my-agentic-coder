package facade

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/sidecarauth"
)

type mcpUpstream struct {
	mu   sync.Mutex
	reqs []string // method + " " + body
	body [][]byte
	cl   []int64
}

func (u *mcpUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.reqs...)
}

func mcpFacade(t *testing.T, ap Approver, mutate func(*mcppolicy.Policy)) (*Facade, *recAuditor, *mcpUpstream) {
	t.Helper()
	u := &mcpUpstream{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, r.Method+" "+r.URL.Path)
		u.body = append(u.body, b)
		u.cl = append(u.cl, r.ContentLength)
		u.mu.Unlock()
		io.WriteString(w, "upstream")
	}))
	t.Cleanup(up.Close)
	pol := &mcppolicy.Policy{
		Server: "srv", Access: mcppolicy.ReadWriteDelete,
		Tools: map[string]mcppolicy.Class{
			"get": mcppolicy.Read, "put": mcppolicy.Write, "rm": mcppolicy.Delete, "hidden": mcppolicy.Off,
		},
		Approvals: mcppolicy.DefaultApprovals(),
	}
	if mutate != nil {
		mutate(pol)
	}
	port := up.Listener.Addr().(*net.TCPAddr).Port
	f := New("", "", []Route{{Mount: "srv", Upstream: sidecarauth.Upstream{Port: port}, Skill: "mcp:srv", MCP: pol}},
		1<<20, 30*time.Second, "", "test")
	f.Approver = ap
	rec := &recAuditor{}
	f.auditor = rec
	return f, rec, u
}

func mcpPost(f *Facade, path, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	f.handle(w, req)
	return w
}

func callBody(id, tool, args string) string {
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
}

type rpcErr struct {
	ID    json.RawMessage `json:"id"`
	Error struct {
		Code    int
		Message string
	} `json:"error"`
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) rpcErr {
	t.Helper()
	var e rpcErr
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return e
}

func TestMCPGate_UnparseableIsRefused400(t *testing.T) {
	f, _, up := mcpFacade(t, nil, nil)
	cases := map[string]string{
		"batch":         `[` + callBody("1", "get", `{}`) + `]`,
		"duplicate":     `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get","name":"rm"}}`,
		"case variant":  `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"Name":"rm","name":"get"}}`,
		"case variant2": `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"Name":"rm"}}`,
		"not json":      `nope`,
	}
	for name, body := range cases {
		w := mcpPost(f, "/srv/mcp", body)
		if w.Code != 400 || w.Header().Get("X-Omac-Reason") != "mcp-unparseable" {
			t.Errorf("%s: %d %q", name, w.Code, w.Header().Get("X-Omac-Reason"))
		}
		e := decodeErr(t, w)
		if e.ID != nil || e.Error.Code == 0 {
			t.Errorf("%s: error body %s", name, w.Body)
		}
	}
	if len(up.seen()) != 0 {
		t.Errorf("upstream reached: %v", up.seen())
	}
}

func TestMCPGate_GzipAndOversizeAreRefused(t *testing.T) {
	f, _, up := mcpFacade(t, nil, nil)
	var zb bytes.Buffer
	zw := gzip.NewWriter(&zb)
	zw.Write([]byte(callBody("1", "get", `{}`)))
	zw.Close()
	if w := mcpPost(f, "/srv/mcp", zb.String(), "Content-Encoding", "gzip"); w.Code != 415 {
		t.Errorf("gzip = %d", w.Code)
	}
	f.MaxBodyBytes = 100
	big := callBody("1", "get", `{"x":"`+strings.Repeat("a", 200)+`"}`)
	if w := mcpPost(f, "/srv/mcp", big); w.Code != 413 {
		t.Errorf("big = %d", w.Code)
	}
	if len(up.seen()) != 0 {
		t.Errorf("upstream reached: %v", up.seen())
	}
}

func TestMCPGate_PolicyDenyIsAJSONRPCErrorAndNeverForwarded(t *testing.T) {
	ap := &fakeApprover{d: ApprovalDecision{Allow: true, Source: "prompt"}}
	f, rec, up := mcpFacade(t, ap, func(p *mcppolicy.Policy) { p.Access = mcppolicy.ReadWrite })
	cases := []struct{ name, body, reason string }{
		{"unlisted", callBody("7", "nope", `{}`), mcppolicy.ReasonUnlisted},
		{"off", callBody("7", "hidden", `{}`), mcppolicy.ReasonUnlisted},
		{"access", callBody("7", "rm", `{}`), mcppolicy.ReasonAccess},
		{"method", `{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{}}`, mcppolicy.ReasonUnlisted},
	}
	for _, c := range cases {
		w := mcpPost(f, "/srv/mcp", c.body)
		if w.Code != 200 || w.Header().Get("X-Omac-Reason") != c.reason {
			t.Errorf("%s: %d %q", c.name, w.Code, w.Header().Get("X-Omac-Reason"))
		}
		e := decodeErr(t, w)
		if string(e.ID) != "7" || !strings.Contains(e.Error.Message, c.reason) {
			t.Errorf("%s: body %s", c.name, w.Body)
		}
	}
	if len(up.seen()) != 0 || len(ap.calls) != 0 {
		t.Errorf("upstream %v, approver %d", up.seen(), len(ap.calls))
	}
	if ev := rec.approvals(); len(ev) != 4 || ev[2].Source != "mcp-access" || ev[2].Tool != "rm" || ev[2].ToolClass != "delete" {
		t.Errorf("audit = %+v", ev)
	}
}

func TestMCPGate_ApprovalDenyReachesNoUpstream(t *testing.T) {
	for src, reason := range map[ApprovalSource]string{
		SourcePrompt: "approval-denied", SourceTimeout: "approval-timeout", SourceUnavailable: "approval-unavailable",
	} {
		ap := &fakeApprover{d: ApprovalDecision{Source: src}}
		f, rec, up := mcpFacade(t, ap, nil)
		w := mcpPost(f, "/srv/mcp", callBody("3", "put", `{"path":"/x"}`))
		if w.Code != 200 || w.Header().Get("X-Omac-Reason") != reason {
			t.Errorf("%s: %d %q", src, w.Code, w.Header().Get("X-Omac-Reason"))
		}
		if e := decodeErr(t, w); string(e.ID) != "3" || !strings.Contains(e.Error.Message, string(src)) {
			t.Errorf("%s: body %s", src, w.Body)
		}
		if len(up.seen()) != 0 {
			t.Errorf("%s: upstream reached", src)
		}
		if ev := rec.approvals(); len(ev) != 1 || ev[0].Result != "deny" || ev[0].Source != string(src) {
			t.Errorf("%s: audit %+v", src, ev)
		}
	}
	f, _, up := mcpFacade(t, nil, nil)
	if w := mcpPost(f, "/srv/mcp", callBody("3", "put", `{}`)); w.Header().Get("X-Omac-Reason") != "approval-unavailable" || len(up.seen()) != 0 {
		t.Errorf("no approver: %q %v", w.Header().Get("X-Omac-Reason"), up.seen())
	}
}

func TestMCPGate_ApprovalAllowForwardsCanonicalBytes(t *testing.T) {
	ap := &fakeApprover{d: ApprovalDecision{Allow: true, Scope: "once", Source: "prompt"}}
	f, rec, up := mcpFacade(t, ap, nil)
	body := "{ \"params\" : {\"arguments\":{\"token\":\"abcdefghijkl\",\"path\":\"/x\"}, \"name\":\"put\"},\n\"method\":\"tools/call\", \"id\":9, \"jsonrpc\":\"2.0\" }"
	_, canonical, err := mcppolicy.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	w := mcpPost(f, "/srv/mcp", body)
	if w.Code != 200 || w.Body.String() != "upstream" {
		t.Fatalf("code %d body %q", w.Code, w.Body)
	}
	if len(up.body) != 1 || !bytes.Equal(up.body[0], canonical) || bytes.Equal(up.body[0], []byte(body)) {
		t.Errorf("forwarded %q, canonical %q", up.body, canonical)
	}
	if up.cl[0] != int64(len(canonical)) {
		t.Errorf("content length %d, want %d", up.cl[0], len(canonical))
	}
	if len(ap.calls) != 1 {
		t.Fatalf("approver calls %d", len(ap.calls))
	}
	m := ap.calls[0].MCP
	if m == nil || m.Server != "srv" || m.Tool != "put" || m.Class != mcppolicy.Write ||
		strings.Contains(m.Args, "abcdefghijkl") || !strings.Contains(m.Args, "path=") {
		t.Errorf("approval request MCP = %+v", m)
	}
	ev := rec.approvals()
	if len(ev) != 1 || ev[0].Result != "allow" || ev[0].Tool != "put" || ev[0].ToolClass != "write" || ev[0].ToolArgs != m.Args {
		t.Errorf("approval audit = %+v", ev)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	last := rec.ev[len(rec.ev)-1]
	if last.Type != "facade.request" || last.Tool != "put" || last.ToolClass != "write" || strings.Contains(last.ToolArgs, "abcdefghijkl") {
		t.Errorf("request audit = %+v", last)
	}
	for _, e := range rec.ev {
		if strings.Contains(e.ToolArgs, "abcdefghijkl") {
			t.Errorf("secret value in audit: %+v", e)
		}
	}
}

func TestMCPGate_ReadToolNeedsNoApprovalAndResponsesPass(t *testing.T) {
	ap := &fakeApprover{}
	f, rec, up := mcpFacade(t, ap, nil)
	if w := mcpPost(f, "/srv/mcp", callBody("1", "get", `{}`)); w.Code != 200 || w.Body.String() != "upstream" {
		t.Errorf("read call: %d %q", w.Code, w.Body)
	}
	resp := `{"jsonrpc":"2.0","id":5,"result":{}}`
	if w := mcpPost(f, "/srv/mcp", resp); w.Body.String() != "upstream" {
		t.Errorf("response: %q", w.Body)
	}
	if len(ap.calls) != 0 || len(up.seen()) != 2 {
		t.Errorf("approver %d, upstream %v", len(ap.calls), up.seen())
	}
	if ev := rec.approvals(); len(ev) != 1 || ev[0].Source != "declared" || ev[0].Tool != "get" {
		t.Errorf("audit %+v", ev)
	}
}

func TestMCPGate_OnlyMCPPathAndSessionMethods(t *testing.T) {
	f, _, up := mcpFacade(t, nil, nil)
	for _, c := range [][2]string{{"POST", "/srv/other"}, {"GET", "/srv/mcp/x"}, {"GET", "/srv/"}, {"DELETE", "/srv/other"}} {
		if w := do(f, c[0], c[1]); w.Code != 404 {
			t.Errorf("%s %s = %d", c[0], c[1], w.Code)
		}
	}
	if w := do(f, "PUT", "/srv/mcp"); w.Code != 405 {
		t.Errorf("PUT = %d", w.Code)
	}
	for _, m := range []string{"GET", "DELETE"} {
		if w := do(f, m, "/srv/mcp"); w.Code != 200 {
			t.Errorf("%s = %d", m, w.Code)
		}
	}
	if got := up.seen(); len(got) != 2 || got[0] != "GET /mcp" || got[1] != "DELETE /mcp" {
		t.Errorf("upstream saw %v", got)
	}
}
