package facade

import (
	"bytes"
	"encoding/json"
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

func TestIntentEndpointGetLookup(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	reg.Record("example.com", "fetch release notes")
	f := &Facade{IntentRegistry: reg}

	req := httptest.NewRequest(http.MethodGet, "/sandbox/intent?target=example.com", nil)
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	var resp struct {
		Target string `json:"target"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Reason != "fetch release notes" {
		t.Errorf("reason = %q; want 'fetch release notes'", resp.Reason)
	}
}

func TestIntentEndpointGetNotFound(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}

	req := httptest.NewRequest(http.MethodGet, "/sandbox/intent?target=unknown.example", nil)
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", w.Code)
	}
}

// TestIntentEndpointLookupHint verifies the reactive-recovery channel: the
// GET response carries a `declared` flag and an actionable `hint` so an
// HTTPS agent (whose CONNECT denial body was discarded by its client) can
// query out-of-band and learn whether to declare-and-retry or to stop.
func TestIntentEndpointLookupHint(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}

	type resp struct {
		Target   string `json:"target"`
		Declared bool   `json:"declared"`
		Reason   string `json:"reason"`
		Hint     string `json:"hint"`
	}
	decode := func(w *httptest.ResponseRecorder) resp {
		t.Helper()
		var r resp
		if err := json.NewDecoder(w.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Undeclared: 404, declared=false, hint tells the agent to declare + retry.
	req := httptest.NewRequest(http.MethodGet, "/sandbox/intent?target=fresh.example", nil)
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("undeclared status = %d; want 404", w.Code)
	}
	got := decode(w)
	if got.Declared {
		t.Error("undeclared: declared should be false")
	}
	if !strings.Contains(got.Hint, "/sandbox/intent") || !strings.Contains(got.Hint, "retry") {
		t.Errorf("undeclared hint should point at declaring + retrying: %q", got.Hint)
	}

	// Declared: 200, declared=true, reason preserved, hint warns against retry.
	reg.Record("fresh.example", "fetch the changelog")
	req = httptest.NewRequest(http.MethodGet, "/sandbox/intent?target=fresh.example", nil)
	w = httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("declared status = %d; want 200", w.Code)
	}
	got = decode(w)
	if !got.Declared || got.Reason != "fetch the changelog" {
		t.Errorf("declared resp = %+v", got)
	}
	if !strings.Contains(got.Hint, "declined") || !strings.Contains(got.Hint, "do not retry") {
		t.Errorf("declared hint should warn against retrying a user-declined host: %q", got.Hint)
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
