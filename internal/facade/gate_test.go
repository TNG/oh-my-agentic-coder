package facade

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/audit"
)

type recAuditor struct {
	mu sync.Mutex
	ev []audit.Event
}

func (a *recAuditor) Emit(e audit.Event) { a.mu.Lock(); a.ev = append(a.ev, e); a.mu.Unlock() }
func (a *recAuditor) Close() error       { return nil }
func (a *recAuditor) RunID() string      { return "t" }
func (a *recAuditor) NextSeq() uint64    { return 1 }
func (a *recAuditor) approvals() []audit.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []audit.Event
	for _, e := range a.ev {
		if e.Type == audit.TypeFacadeApproval {
			out = append(out, e)
		}
	}
	return out
}

type fakeApprover struct {
	mu    sync.Mutex
	calls []ApprovalRequest
	d     ApprovalDecision
	delay time.Duration
}

func (a *fakeApprover) Approve(_ context.Context, r ApprovalRequest) ApprovalDecision {
	a.mu.Lock()
	a.calls = append(a.calls, r)
	a.mu.Unlock()
	time.Sleep(a.delay)
	return a.d
}

func gateSet(t *testing.T) *approval.Set {
	t.Helper()
	s, err := approval.Compile([]approval.Entry{
		{Match: "GET /status", Approval: approval.None},
		{Match: "GET /tick", Approval: approval.None},
		{Match: "POST /echo", Approval: approval.Required, Description: "Echoes the body"},
		{Match: "GET /live/{id}", Approval: approval.None},
		{Match: "GET /live/start", Approval: approval.Required},
		{Match: "ANY /admin/**", Approval: approval.Required},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// gatedFacade returns a facade in front of an upstream that records the
// paths and methods it receives.
func gatedFacade(t *testing.T, ap Approver) (*Facade, *recAuditor, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.EscapedPath()+" ovr="+r.Header.Get("X-HTTP-Method-Override"))
		mu.Unlock()
		if r.URL.Path == "/tick" {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 0; i < 3; i++ {
				io.WriteString(w, "data: x\n\n")
				w.(http.Flusher).Flush()
				time.Sleep(100 * time.Millisecond)
			}
			return
		}
		io.WriteString(w, "ok")
	}))
	t.Cleanup(up.Close)
	port := up.Listener.Addr().(*net.TCPAddr).Port
	f := New("", "", []Route{{Mount: "echo", UpstreamPort: port, Skill: "echo-rest", Approvals: gateSet(t)}},
		1<<20, 30*time.Second, "", "test")
	f.Approver = ap
	rec := &recAuditor{}
	f.auditor = rec
	return f, rec, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func do(f *Facade, method, target string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	f.handle(w, req)
	return w
}

func TestGate_UndeclaredIsRejectedAndNeverForwarded(t *testing.T) {
	ap := &fakeApprover{d: ApprovalDecision{Allow: true}}
	f, rec, seen := gatedFacade(t, ap)
	for _, c := range [][2]string{
		{"GET", "/echo/new-route"}, {"POST", "/echo/status"}, {"GET", "/echo/status/"},
		{"DELETE", "/echo/echo"}, {"GET", "/echo/live/"},
	} {
		w := do(f, c[0], c[1])
		if w.Code != 403 || w.Header().Get("X-Omac-Reason") != "undeclared-endpoint" {
			t.Errorf("%s %s = %d %q", c[0], c[1], w.Code, w.Header().Get("X-Omac-Reason"))
		}
	}
	if len(seen()) != 0 {
		t.Errorf("upstream reached: %v", seen())
	}
	if len(ap.calls) != 0 {
		t.Errorf("dialog raised for undeclared requests: %v", ap.calls)
	}
	ev := rec.approvals()
	if len(ev) != 5 || ev[0].Source != "undeclared-endpoint" || ev[0].Result != "deny" || ev[0].Method != "GET" || ev[0].Path != "/new-route" {
		t.Errorf("audit = %+v", ev)
	}
}

func TestGate_NoneForwardsWithoutAsking(t *testing.T) {
	ap := &fakeApprover{}
	f, rec, seen := gatedFacade(t, ap)
	if w := do(f, "GET", "/echo/status"); w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if len(ap.calls) != 0 || len(seen()) != 1 {
		t.Errorf("calls=%d seen=%v", len(ap.calls), seen())
	}
	if ev := rec.approvals(); len(ev) != 1 || ev[0].Result != "allow" || ev[0].Source != "declared" {
		t.Errorf("audit = %+v", ev)
	}
}

func TestGate_RequiredWithoutApproverIsDenied(t *testing.T) {
	f, _, seen := gatedFacade(t, nil)
	w := do(f, "POST", "/echo/echo")
	if w.Code != 503 || w.Header().Get("X-Omac-Reason") != "approval-unavailable" || len(seen()) != 0 {
		t.Errorf("code=%d reason=%q seen=%v", w.Code, w.Header().Get("X-Omac-Reason"), seen())
	}
}

func TestGate_RequiredAskedWithObservedFacts(t *testing.T) {
	ap := &fakeApprover{d: ApprovalDecision{Allow: true, Scope: "once", Source: "prompt"}}
	f, rec, seen := gatedFacade(t, ap)
	w := do(f, "POST", "/echo/echo?token=abc&n=1")
	if w.Code != 200 || len(seen()) != 1 {
		t.Fatalf("code=%d seen=%v", w.Code, seen())
	}
	c := ap.calls[0]
	if c.Skill != "echo-rest" || c.Method != "POST" || c.Path != "/echo" || c.RawQuery != "token=abc&n=1" || c.Match != "POST /echo" || c.Description != "Echoes the body" {
		t.Errorf("request = %+v", c)
	}
	if ev := rec.approvals(); len(ev) != 1 || ev[0].Result != "allow" || ev[0].Source != "prompt" || ev[0].Scope != "once" {
		t.Errorf("audit = %+v", ev)
	}
	for _, e := range rec.approvals() {
		if strings.Contains(e.Path, "token") || strings.Contains(e.Detail, "token") {
			t.Errorf("query leaked into audit: %+v", e)
		}
	}
}

func TestGate_DenialsForwardNothing(t *testing.T) {
	for name, c := range map[string]struct {
		d      ApprovalDecision
		code   int
		reason string
	}{
		"deny":        {ApprovalDecision{Source: "prompt", Scope: "once"}, 403, "approval-denied"},
		"timeout":     {ApprovalDecision{Source: "timeout"}, 403, "approval-timeout"},
		"unavailable": {ApprovalDecision{Source: "unavailable"}, 503, "approval-unavailable"},
		"deny-all":    {ApprovalDecision{Source: "deny-all"}, 403, "approval-denied"},
	} {
		f, _, seen := gatedFacade(t, &fakeApprover{d: c.d})
		w := do(f, "POST", "/echo/echo")
		if w.Code != c.code || w.Header().Get("X-Omac-Reason") != c.reason || len(seen()) != 0 {
			t.Errorf("%s: code=%d reason=%q seen=%v", name, w.Code, w.Header().Get("X-Omac-Reason"), seen())
		}
	}
}

func TestGate_MostSpecificEntryWins(t *testing.T) {
	ap := &fakeApprover{d: ApprovalDecision{Allow: true, Source: "prompt", Scope: "once"}}
	f, _, _ := gatedFacade(t, ap)
	do(f, "GET", "/echo/live/abc")
	if len(ap.calls) != 0 {
		t.Fatal("/live/abc asked")
	}
	do(f, "GET", "/echo/live/start")
	if len(ap.calls) != 1 || ap.calls[0].Match != "GET /live/start" {
		t.Errorf("calls = %+v", ap.calls)
	}
}

func TestGate_NormalizationCannotBypassARule(t *testing.T) {
	ap := &fakeApprover{d: ApprovalDecision{}}
	f, _, seen := gatedFacade(t, ap)
	// Each spelling cleans to /admin/x, which requires approval (denied here).
	for _, target := range []string{
		"/echo/status/../admin/x", "/echo//admin/x", "/echo/%2e%2e/admin/x", "/echo/./admin/x", "/echo/live/../admin/x",
	} {
		if w := do(f, "GET", target); w.Code != 403 || w.Header().Get("X-Omac-Reason") != "approval-denied" {
			t.Errorf("%s = %d %q", target, w.Code, w.Header().Get("X-Omac-Reason"))
		}
	}
	if len(ap.calls) != 5 || ap.calls[0].Path != "/admin/x" {
		t.Errorf("calls = %+v", ap.calls)
	}
	// Ambiguous encodings are refused outright.
	for _, target := range []string{"/echo/status%2f..%2fadmin", "/echo/a%5cb", "/echo/a%00b"} {
		if w := do(f, "GET", target); w.Code != 400 || w.Header().Get("X-Omac-Reason") != "invalid-path" {
			t.Errorf("%s = %d %q", target, w.Code, w.Header().Get("X-Omac-Reason"))
		}
	}
	if len(seen()) != 0 {
		t.Errorf("upstream reached: %v", seen())
	}
}

func TestGate_ForwardsTheMatchedPathAndStripsOverride(t *testing.T) {
	f, _, seen := gatedFacade(t, nil)
	do(f, "GET", "/echo/live/../status", "X-HTTP-Method-Override", "DELETE")
	got := seen()
	if len(got) != 1 || got[0] != "GET /status ovr=" {
		t.Errorf("upstream saw %v", got)
	}
}

func TestGate_UngatedRouteIsUnchanged(t *testing.T) {
	var got string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.URL.Path }))
	defer up.Close()
	f := New("", "", []Route{{Mount: "echo", UpstreamPort: up.Listener.Addr().(*net.TCPAddr).Port, Skill: "s"}},
		1<<20, 30*time.Second, "", "test")
	do(f, "GET", "/echo/anything/at/all")
	if got != "/anything/at/all" {
		t.Errorf("upstream path = %q", got)
	}
}

// An approved stream is decided once, before the first byte, and then runs
// frame by frame.
func TestGate_ApprovedStreamIsNotBuffered(t *testing.T) {
	ap := &fakeApprover{d: ApprovalDecision{Allow: true, Source: "prompt", Scope: "once"}, delay: 300 * time.Millisecond}
	f, _, _ := gatedFacade(t, ap)
	// "GET /tick" is none; use a required rule for the streaming check.
	f.routes["echo"].Approvals, _ = approval.Compile([]approval.Entry{{Match: "GET /tick", Approval: approval.Required}})
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	defer srv.Close()
	start := time.Now()
	resp, err := http.Get(srv.URL + "/echo/tick")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("headers arrived before the answer")
	}
	var at []time.Duration
	buf := make([]byte, 64)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			at = append(at, time.Since(start))
		}
		if err != nil {
			break
		}
	}
	if len(at) < 3 || at[len(at)-1]-at[0] < 150*time.Millisecond {
		t.Errorf("frames buffered: %v", at)
	}
	if len(ap.calls) != 1 {
		t.Errorf("asked %d times", len(ap.calls))
	}
}
