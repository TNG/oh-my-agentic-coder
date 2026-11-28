package facade

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
)

func TestInternalHandlerUsesGateAndCannotBeReplaced(t *testing.T) {
	calls := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/stage" {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.WriteHeader(204)
	})
	set, err := approval.Compile([]approval.Entry{{Match: "POST /stage", Approval: approval.Required}})
	if err != nil {
		t.Fatal(err)
	}
	f := New("", "", []Route{{Mount: "git", Owner: "builtin.git", Handler: h, Approvals: set}}, 0, 0, "", "test")
	ap := &fakeApprover{d: ApprovalDecision{Allow: false, Source: SourcePrompt}}
	f.Approver = ap
	w := httptest.NewRecorder()
	f.handle(w, httptest.NewRequest("POST", "/git/stage", nil))
	if w.Code != 403 || calls != 0 {
		t.Fatalf("gate bypassed: code=%d calls=%d", w.Code, calls)
	}
	ap.d.Allow = true
	f.AddRoute(Route{Mount: "git", Owner: "builtin.git", UpstreamPort: 1})
	f.RemoveRoute("", "git")
	w = httptest.NewRecorder()
	f.handle(w, httptest.NewRequest("POST", "/git/stage", nil))
	if w.Code != 204 || calls != 1 {
		t.Fatalf("internal route was replaced/removed: code=%d calls=%d", w.Code, calls)
	}
	r := httptest.NewRequest("POST", "/git/stage", nil)
	r.Header.Set("Upgrade", "websocket")
	w = httptest.NewRecorder()
	f.handle(w, r)
	if w.Code != 400 || calls != 1 {
		t.Fatal("internal handler accepted an upgrade")
	}
}

func TestInternalHandlerRequiresExplicitPolicy(t *testing.T) {
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, r := range []Route{{Mount: "git", Handler: h}, {Mount: "git", Owner: "builtin.git", Handler: h, Approvals: approval.AskAll(), UpstreamPort: 123}} {
		f := New("", "127.0.0.1:0", []Route{r}, 0, 0, "", "test")
		if err := f.Start(context.Background()); err == nil {
			f.Close()
			t.Fatal("invalid internal route accepted")
		}
	}
}

func TestInternalHandlerAuthenticationPrecedesApproval(t *testing.T) {
	calls := 0
	f := New("", "", []Route{{Mount: "git", Owner: "builtin.git", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(204)
	}), Approvals: approval.AskAll()}}, 0, 0, "", "test")
	f.FacadeToken = "session-token"
	ap := &fakeApprover{d: ApprovalDecision{Allow: true, Source: SourcePrompt}}
	f.Approver = ap
	for _, token := range []string{"", "wrong", "session-token"} {
		r := httptest.NewRequest("POST", "/git/stage", nil)
		r = r.WithContext(context.WithValue(r.Context(), transportKey{}, transportTCP))
		r.Header.Set("X-Omac-Facade-Token", token)
		w := httptest.NewRecorder()
		f.handle(w, r)
		if token == "session-token" {
			if w.Code != 204 || calls != 1 {
				t.Fatalf("valid token: %d %d", w.Code, calls)
			}
		} else if w.Code != 401 || calls != 0 {
			t.Fatalf("invalid token reached handler: %d %d", w.Code, calls)
		}
	}
}
