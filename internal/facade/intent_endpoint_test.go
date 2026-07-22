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

// TestIntentExplainEndpoint verifies the "Explain more" delivery channel:
// POST /sandbox/intent/explain marks the host, and the next GET lookup
// surfaces the explain-more hint — the re-ask an HTTPS/CONNECT denial body
// cannot carry. The flag is one-shot: a second GET reverts to the plain hint.
func TestIntentExplainEndpoint(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}

	hintOf := func(path string, wantStatus int) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		f.handleSandboxIntent(w, req)
		if w.Code != wantStatus {
			t.Fatalf("GET %s status = %d; want %d", path, w.Code, wantStatus)
		}
		var resp struct {
			Hint string `json:"hint"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		return resp.Hint
	}

	// Mark the host.
	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent/explain?target=example.com", nil)
	w := httptest.NewRecorder()
	f.handleSandboxIntentExplain(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("explain POST status = %d; want 204", w.Code)
	}

	// Undeclared + explain-more: 404, but the hint is the explain-more re-ask.
	if h := hintOf("/sandbox/intent?target=example.com", http.StatusNotFound); !strings.Contains(h, "Explain more") {
		t.Errorf("first GET hint should surface the explain-more re-ask: %q", h)
	}
	// One-shot: the second GET reverts to the generic undeclared hint.
	if h := hintOf("/sandbox/intent?target=example.com", http.StatusNotFound); strings.Contains(h, "Explain more") {
		t.Errorf("explain-more hint should be consumed after one read: %q", h)
	}
}

// TestIntentExplainOverridesDeclinedHint verifies that when an intent is
// already on file, an "Explain more" click still wins over the "user
// declined — do not retry" hint (the user asked for a fuller reason).
func TestIntentExplainOverridesDeclinedHint(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	reg.Record("example.com", "fetch notes")
	reg.MarkExplainMore("example.com")
	f := &Facade{IntentRegistry: reg}

	req := httptest.NewRequest(http.MethodGet, "/sandbox/intent?target=example.com", nil)
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	var resp struct {
		Declared bool   `json:"declared"`
		Hint     string `json:"hint"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Declared {
		t.Error("should still report declared=true")
	}
	if !strings.Contains(resp.Hint, "Explain more") {
		t.Errorf("explain-more must override the declined hint: %q", resp.Hint)
	}
}

// TestIntentPostClearsExplainMore guards the inline-recovery lifecycle: after
// an "Explain more" click, the agent reads the hint from the deny body/header
// and POSTs a fuller intent instead of hitting the GET lookup that would
// consume the one-shot flag. The POST must retire the flag itself — otherwise
// a later GET fallback (after the user declines the retry) would revive the
// "re-declare and retry" hint and invite a loop.
func TestIntentPostClearsExplainMore(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}

	// User clicked "Explain more"; the flag is live (never consumed by GET).
	reg.MarkExplainMore("example.com")

	// Inline recovery: the agent POSTs a fuller intent without a GET.
	body := bytes.NewReader([]byte(`{"target":"example.com","reason":"fetch signed release notes for verification"}`))
	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent", body)
	w := httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d; want 204", w.Code)
	}

	// A later GET fallback must now see the declared hint, not the explain-more
	// re-ask — the user's subsequent decline must not be overridden.
	req = httptest.NewRequest(http.MethodGet, "/sandbox/intent?target=example.com", nil)
	w = httptest.NewRecorder()
	f.handleSandboxIntent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d; want 200", w.Code)
	}
	var resp struct {
		Declared bool   `json:"declared"`
		Hint     string `json:"hint"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Declared {
		t.Error("should report declared=true after POST")
	}
	if strings.Contains(resp.Hint, "Explain more") {
		t.Errorf("POST must clear the explain-more flag; GET revived it: %q", resp.Hint)
	}
}

func TestIntentExplainEndpointNilRegistry(t *testing.T) {
	f := &Facade{}
	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent/explain?target=x", nil)
	w := httptest.NewRecorder()
	f.handleSandboxIntentExplain(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", w.Code)
	}
}

func TestIntentExplainEndpointRequiresTarget(t *testing.T) {
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)
	f := &Facade{IntentRegistry: reg}
	req := httptest.NewRequest(http.MethodPost, "/sandbox/intent/explain", nil)
	w := httptest.NewRecorder()
	f.handleSandboxIntentExplain(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
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

func TestIntentEndpointRejectsRelativePath(t *testing.T) {
	cases := []struct {
		target string
		want   int
		desc   string
	}{
		{"./config", http.StatusBadRequest, "relative path with ./"},
		{"../etc/passwd", http.StatusBadRequest, "relative path with ../"},
		{"config/file", http.StatusBadRequest, "relative path with separator"},
		{"/abs/path", http.StatusNoContent, "absolute path accepted"},
		{"~/secrets", http.StatusNoContent, "tilde path accepted"},
		{"example.com", http.StatusNoContent, "bare hostname accepted"},
		{"example.com:443", http.StatusNoContent, "host:port accepted"},
		{"https://example.com/path", http.StatusNoContent, "URL accepted"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			reg := intent.New(time.Minute)
			t.Cleanup(reg.Close)
			f := &Facade{IntentRegistry: reg}
			body := bytes.NewReader([]byte(`{"target":"` + tc.target + `","reason":"test"}`))
			req := httptest.NewRequest(http.MethodPost, "/sandbox/intent", body)
			w := httptest.NewRecorder()
			f.handleSandboxIntent(w, req)
			if w.Code != tc.want {
				t.Errorf("target %q: status = %d; want %d", tc.target, w.Code, tc.want)
			}
		})
	}
}
