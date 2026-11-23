package facade

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/sidecarauth"
)

type mcpUpstream struct {
	mu        sync.Mutex
	reqs      []string // method + " " + body
	body      [][]byte
	cl        []int64
	failFirst int // answer this many requests with 503 before succeeding
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
		fail := u.failFirst > 0
		if fail {
			u.failFirst--
		}
		u.mu.Unlock()
		if fail {
			http.Error(w, "gateway restarting", http.StatusServiceUnavailable)
			return
		}
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

func allowOnce() *fakeApprover {
	return &fakeApprover{d: ApprovalDecision{Allow: true, Scope: "once", Source: "prompt"}}
}

func TestMCPGate_RetryAfterAFailedAttemptAsksOnceEvenWithANewSession(t *testing.T) {
	ap := allowOnce()
	f, rec, up := mcpFacade(t, ap, nil)
	up.failFirst = 1
	body := callBody("7", "put", `{"path":"/x"}`)
	if w := mcpPost(f, "/srv/mcp", body, "Mcp-Session-Id", "s1"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("first attempt: %d", w.Code)
	}
	if w := mcpPost(f, "/srv/mcp", body, "Mcp-Session-Id", "s2"); w.Code != 200 || w.Body.String() != "upstream" {
		t.Fatalf("retry on a new session: %d %q", w.Code, w.Body)
	}
	if len(ap.calls) != 1 || len(up.seen()) != 2 {
		t.Fatalf("approver %d upstream %d", len(ap.calls), len(up.seen()))
	}
	ev := rec.approvals()
	if len(ev) != 2 || ev[0].Source != "prompt" || ev[1].Source != "retry" || ev[1].Result != "allow" || ev[1].Scope != "" || ev[1].Tool != "put" {
		t.Errorf("audit = %+v", ev)
	}
}

func TestMCPGate_ASuccessfulForwardConsumesTheAllowance(t *testing.T) {
	ap := allowOnce()
	f, _, up := mcpFacade(t, ap, nil)
	body := callBody("7", "put", `{"path":"/x"}`)
	for i := 0; i < 2; i++ {
		if w := mcpPost(f, "/srv/mcp", body, "Mcp-Session-Id", "s1"); w.Code != 200 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if len(ap.calls) != 2 || len(up.seen()) != 2 {
		t.Errorf("a repeat after a success must ask again: approver %d upstream %d", len(ap.calls), len(up.seen()))
	}
}

func TestMCPGate_RetryNeverMatchesAChangedCall(t *testing.T) {
	ap := allowOnce()
	f, _, up := mcpFacade(t, ap, nil)
	up.failFirst = 100
	mcpPost(f, "/srv/mcp", callBody("7", "put", `{"path":"/x"}`))
	for name, body := range map[string]string{
		"id":        callBody("8", "put", `{"path":"/x"}`),
		"tool":      callBody("7", "rm", `{"path":"/x"}`),
		"arguments": callBody("7", "put", `{"path":"/y"}`),
	} {
		before := len(ap.calls)
		mcpPost(f, "/srv/mcp", body)
		if len(ap.calls) != before+1 {
			t.Errorf("changed %s did not ask", name)
		}
	}
}

func TestMCPGate_AllowanceExpiresAfterTTL(t *testing.T) {
	ap := allowOnce()
	f, _, up := mcpFacade(t, ap, nil)
	up.failFirst = 100
	now := time.Unix(1000, 0)
	f.mcpAllowances.now = func() time.Time { return now }
	body := callBody("7", "put", `{}`)
	mcpPost(f, "/srv/mcp", body)
	now = now.Add(mcpAllowanceTTL - time.Second)
	mcpPost(f, "/srv/mcp", body)
	if len(ap.calls) != 1 {
		t.Fatalf("within ttl asked %d times", len(ap.calls))
	}
	now = now.Add(2 * time.Second)
	mcpPost(f, "/srv/mcp", body)
	if len(ap.calls) != 2 {
		t.Errorf("after ttl asked %d times", len(ap.calls))
	}
}

func TestMCPGate_DenyIsNotCached(t *testing.T) {
	ap := &fakeApprover{d: ApprovalDecision{Source: SourcePrompt}}
	f, _, up := mcpFacade(t, ap, nil)
	body := callBody("7", "put", `{}`)
	mcpPost(f, "/srv/mcp", body)
	ap.mu.Lock()
	ap.d = ApprovalDecision{Allow: true, Scope: "once", Source: "prompt"}
	ap.mu.Unlock()
	mcpPost(f, "/srv/mcp", body)
	if len(ap.calls) != 2 || len(up.seen()) != 1 {
		t.Errorf("approver %d upstream %d", len(ap.calls), len(up.seen()))
	}
}

func TestMCPGate_NotificationIsNotCached(t *testing.T) {
	ap := allowOnce()
	f, _, _ := mcpFacade(t, ap, nil)
	body := `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"put","arguments":{}}}`
	mcpPost(f, "/srv/mcp", body)
	mcpPost(f, "/srv/mcp", body)
	if len(ap.calls) != 2 {
		t.Errorf("approver calls %d, want 2", len(ap.calls))
	}
}

func TestMCPAllowanceCache_StaysBounded(t *testing.T) {
	var c mcpAllowanceCache
	now := time.Unix(1000, 0)
	c.now = func() time.Time { now = now.Add(time.Millisecond); return now }
	var first, last mcpAllowanceKey
	for i := 0; i < 2*mcpAllowanceMaxEntries; i++ {
		k := mcpAllowanceKey{id: strconv.Itoa(i)}
		if i == 0 {
			first = k
		}
		last = k
		c.restore(k, time.Time{})
		if len(c.until) > mcpAllowanceMaxEntries {
			t.Fatalf("size %d", len(c.until))
		}
	}
	_, firstKept := c.until[first]
	_, lastKept := c.until[last]
	if firstKept || !lastKept {
		t.Errorf("oldest kept %v, newest kept %v", firstKept, lastKept)
	}
}

func TestMCPGate_ConcurrentRetriesOnlyEverUseApproverAnswers(t *testing.T) {
	ap := allowOnce()
	f, _, up := mcpFacade(t, ap, nil)
	body := callBody("7", "put", `{}`)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); mcpPost(f, "/srv/mcp", body) }()
	}
	wg.Wait()
	if len(up.seen()) != 8 || len(ap.calls) != 8 {
		t.Errorf("every forwarded call needs its own answer: approver %d upstream %d", len(ap.calls), len(up.seen()))
	}
}

// One answer must never let more than one attempt of a call through: after a
// failed attempt, concurrent copies of the call (on new sessions) share one
// allowance, and the others ask.
func TestMCPGate_OneAllowanceAdmitsOneConcurrentRetry(t *testing.T) {
	ap := allowOnce()
	f, rec, up := mcpFacade(t, ap, nil)
	up.failFirst = 1
	body := callBody("7", "put", `{}`)
	if w := mcpPost(f, "/srv/mcp", body); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("first attempt: %d", w.Code)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); mcpPost(f, "/srv/mcp", body, "Mcp-Session-Id", strconv.Itoa(i)) }()
	}
	wg.Wait()
	retries := 0
	for _, ev := range rec.approvals() {
		if ev.Source == string(SourceRetry) {
			retries++
		}
	}
	if retries != 1 || len(ap.calls) != 8 || len(up.seen()) != 9 {
		t.Errorf("retries %d approver %d upstream %d; want 1, 8, 9", retries, len(ap.calls), len(up.seen()))
	}
}

// The allowance is back in place before the client sees the failure, so the
// bridge's immediate retry does not race it.
func TestMCPGate_AllowanceIsRestoredBeforeTheFailureIsWritten(t *testing.T) {
	ap := allowOnce()
	f, _, up := mcpFacade(t, ap, nil)
	up.failFirst = 1
	body := callBody("7", "put", `{}`)
	call, canonical := mustParse(t, body)
	key := mcpAllowanceKeyFor(f.routes[routeKey("", "srv")], call, canonical)
	probe := &statusProbe{ResponseRecorder: httptest.NewRecorder(), check: func() bool {
		f.mcpAllowances.mu.Lock()
		defer f.mcpAllowances.mu.Unlock()
		_, ok := f.mcpAllowances.until[key]
		return ok
	}}
	f.handle(probe, httptest.NewRequest("POST", "/srv/mcp", strings.NewReader(body)))
	if probe.Code != http.StatusServiceUnavailable || !probe.sawAllowance {
		t.Errorf("status %d, allowance present at write %v", probe.Code, probe.sawAllowance)
	}
}

type statusProbe struct {
	*httptest.ResponseRecorder
	check        func() bool
	sawAllowance bool
}

func (p *statusProbe) WriteHeader(code int) {
	p.sawAllowance = p.check()
	p.ResponseRecorder.WriteHeader(code)
}

func mustParse(t *testing.T, body string) (mcppolicy.Call, []byte) {
	t.Helper()
	c, canonical, err := mcppolicy.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return c, canonical
}

func TestMCPGate_AnyContentEncodingHeaderIsRefused(t *testing.T) {
	f, _, up := mcpFacade(t, nil, nil)
	req := httptest.NewRequest("POST", "/srv/mcp", strings.NewReader(callBody("1", "get", `{}`)))
	req.Header["Content-Encoding"] = []string{"", "gzip"}
	w := httptest.NewRecorder()
	f.handle(w, req)
	if w.Code != http.StatusUnsupportedMediaType || len(up.seen()) != 0 {
		t.Errorf("status %d, upstream %v", w.Code, up.seen())
	}
}
