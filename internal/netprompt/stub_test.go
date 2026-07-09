package netprompt

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStubBackendDecisionToLabel(t *testing.T) {
	suffix := "example.com"
	cases := []struct {
		d    stubDecision
		want string
	}{
		{stubDecision{Allow: true}, "Allow once"},
		{stubDecision{Allow: true, Persist: true, Scope: "host"}, "Allow permanently (this host)"},
		{stubDecision{Allow: true, Persist: true, Scope: "suffix"}, "Allow permanently (*.example.com)"},
		{stubDecision{Allow: false}, "Deny once"},
		{stubDecision{Allow: false, Persist: true, Scope: "host"}, "Deny permanently (this host)"},
		{stubDecision{Allow: false, Persist: true, Scope: "suffix"}, "Deny permanently (*.example.com)"},
		{stubDecision{Allow: false, NeedsIntent: true}, "Explain more"},
	}
	for i, c := range cases {
		got := decisionToLabel(c.d, suffix)
		if got != c.want {
			t.Errorf("[%d] decisionToLabel(%+v) = %q; want %q", i, c.d, got, c.want)
		}
	}
}

func TestStubBackendShow(t *testing.T) {
	src := &fileDecisionSource{
		loaded: true,
		decisions: map[string]stubDecision{
			"allow.example":   {Allow: true},
			"deny.example":    {Allow: false},
			"explain.example": {Allow: false, NeedsIntent: true},
		},
	}
	logf := func(string, ...any) {}
	b := stubBackend{source: src, logf: logf}

	ctx := context.Background()

	// Allow
	label, err := b.show(ctx, "allow.example", 443, "example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if label != "Allow once" {
		t.Errorf("allow label = %q; want 'Allow once'", label)
	}

	// Deny
	label, _ = b.show(ctx, "deny.example", 443, "example.com", "")
	if label != "Deny once" {
		t.Errorf("deny label = %q; want 'Deny once'", label)
	}

	// Explain more
	label, _ = b.show(ctx, "explain.example", 443, "example.com", "fetch data")
	if label != "Explain more" {
		t.Errorf("explain label = %q; want 'Explain more'", label)
	}
}

func TestStubBackendNoDecisionDenies(t *testing.T) {
	src := &fileDecisionSource{loaded: true, decisions: map[string]stubDecision{}}
	b := stubBackend{source: src, logf: func(string, ...any) {}}
	label, _ := b.show(context.Background(), "unknown.example", 443, "example.com", "")
	if label != "Deny once" {
		t.Errorf("unknown host label = %q; want 'Deny once'", label)
	}
}

func TestStubBackendWildcard(t *testing.T) {
	src := &fileDecisionSource{loaded: true, decisions: map[string]stubDecision{
		"*": {Allow: true, Persist: true, Scope: "host"},
	}}
	b := stubBackend{source: src, logf: func(string, ...any) {}}
	label, _ := b.show(context.Background(), "anything.example", 443, "example.com", "")
	if label != "Allow permanently (this host)" {
		t.Errorf("wildcard label = %q; want 'Allow permanently (this host)'", label)
	}
}

func TestFileDecisionSourceLoadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "decisions.json")
	data := map[string]stubDecision{
		"api.example.com": {Allow: true, Persist: true, Scope: "suffix"},
		"*":               {Allow: false},
	}
	raw, _ := json.Marshal(data)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	src := newFileDecisionSource(path)
	d, ok := src.lookup("api.example.com")
	if !ok || !d.Allow || d.Scope != "suffix" {
		t.Errorf("lookup failed: %+v ok=%v", d, ok)
	}

	// Wildcard
	d2, ok2 := src.lookup("random.example")
	if !ok2 || d2.Allow {
		t.Errorf("wildcard lookup failed: %+v ok=%v", d2, ok2)
	}
}

func TestFileDecisionSourceMissingFile(t *testing.T) {
	src := newFileDecisionSource("/nonexistent/path/decisions.json")
	d, ok := src.lookup("anything.example")
	if ok {
		t.Errorf("missing file should return not-found: %+v", d)
	}
}

func TestFileDecisionSourceReloadOnFirstLookup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "decisions.json")

	// File doesn't exist yet → no decisions.
	src := newFileDecisionSource(path)
	if _, ok := src.lookup("test.example"); ok {
		t.Error("should be no decision before file exists")
	}

	// Write file, reset loaded flag to simulate a fresh source.
	data := map[string]stubDecision{"test.example": {Allow: true}}
	raw, _ := json.Marshal(data)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	src2 := newFileDecisionSource(path)
	d, ok := src2.lookup("test.example")
	if !ok || !d.Allow {
		t.Errorf("should load from disk: %+v ok=%v", d, ok)
	}
}

func TestStubBackendNilSource(t *testing.T) {
	b := stubBackend{source: nil, logf: func(string, ...any) {}}
	if b.available() {
		t.Error("nil source should not be available")
	}
}

func TestStubBackendIntentLogged(t *testing.T) {
	var logged []string
	src := &fileDecisionSource{loaded: true, decisions: map[string]stubDecision{
		"test.example": {Allow: true},
	}}
	b := stubBackend{source: src, logf: func(format string, args ...any) {
		logged = append(logged, format)
	}}
	b.show(context.Background(), "test.example", 443, "example.com", "fetch data")
	if len(logged) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(logged))
	}
	if !contains(logged[0], "intent") {
		t.Errorf("log line should mention intent: %q", logged[0])
	}
}

// TestStubBackendRoundTripThroughPrompter verifies the stub produces
// correct PromptResult when wired through the full prompter path
// (labelToToken → tokenToResult).
func TestStubBackendRoundTripThroughPrompter(t *testing.T) {
	suffix := "example.com"
	cases := []struct {
		name            string
		decision        stubDecision
		wantAllow       bool
		wantNeedsIntent bool
	}{
		{"allow once", stubDecision{Allow: true}, true, false},
		{"deny once", stubDecision{Allow: false}, false, false},
		{"explain more", stubDecision{Allow: false, NeedsIntent: true}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			label := decisionToLabel(c.decision, suffix)
			token := labelToToken(label, suffix)
			result := tokenToResult(token, "host.example", suffix)
			if result.Allow != c.wantAllow {
				t.Errorf("allow = %v; want %v", result.Allow, c.wantAllow)
			}
			if result.NeedsIntent != c.wantNeedsIntent {
				t.Errorf("needsIntent = %v; want %v", result.NeedsIntent, c.wantNeedsIntent)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Ensure the file compiles with the time import (used in future tests
// that may need timeouts).
var _ = time.Second
