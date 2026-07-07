package facade

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/intent"
)

func TestIntentEndpointRecords(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}

	body := bytes.NewReader([]byte(`{"target":"example.com","reason":"fetch release notes"}`))
	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent", body)
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", w.Code)
	}
	e, ok := reg.Lookup("example.com")
	if !ok || e.Reason != "fetch release notes" {
		t.Errorf("registry = %+v", e)
	}
}

func TestIntentEndpointRejectsEmptyBody(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}

	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "target") || !strings.Contains(w.Body.String(), "reason") {
		t.Errorf("body should name required fields: %q", w.Body.String())
	}
}

func TestIntentEndpointNilRegistry(t *testing.T) {
	f := &Facade{}
	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent",
		bytes.NewReader([]byte(`{"target":"x","reason":"y"}`)))
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", w.Code)
	}
}

func TestIntentEndpointGetNotAllowed(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}
	req := httptest.NewRequest(http.MethodGet, "/sandbox/intent", nil)
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; want 405", w.Code)
	}
}

func TestIntentEndpointMalformedJSON(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}
	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent",
		strings.NewReader(`{not json`))
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
}
