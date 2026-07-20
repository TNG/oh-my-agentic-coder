package netprompt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// signalBackend is a dialogBackend stub that returns a fixed label without
// showing any GUI, for exercising Prompt's instrumentation path.
type signalBackend struct{ label string }

func (signalBackend) name() string    { return "signal-test" }
func (signalBackend) available() bool { return true }
func (b signalBackend) show(ctx context.Context, host string, port int, suffix, intent, cause, originLine string) (string, error) {
	return b.label, nil
}

// TestPromptEmitsIntentSignal locks the machine-parseable behavioral signal
// that lets the pre-declaration rate be computed from a session's diag log:
// every network prompt logs exactly one "intent-signal:" line tagged
// declared|missing depending on whether the agent pre-declared.
func TestPromptEmitsIntentSignal(t *testing.T) {
	cases := []struct {
		name       string
		lookup     func(string) (string, bool)
		wantSignal string
	}{
		{"declared", func(string) (string, bool) { return "fetch release notes", true }, "intent=declared"},
		{"missing", func(string) (string, bool) { return "", false }, "intent=missing"},
		{"no-registry", nil, "intent=missing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logs []string
			p := &Prompter{
				timeout:      time.Second,
				logf:         func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
				backends:     []dialogBackend{signalBackend{label: "Allow once"}},
				lookupIntent: c.lookup,
			}
			p.Prompt(context.Background(), "api.example.com", 443)
			var signals []string
			for _, l := range logs {
				if strings.Contains(l, "intent-signal:") {
					signals = append(signals, l)
				}
			}
			if len(signals) != 1 {
				t.Fatalf("want exactly 1 intent-signal line, got %d: %v", len(signals), signals)
			}
			if !strings.Contains(signals[0], c.wantSignal) || !strings.Contains(signals[0], "host=api.example.com") {
				t.Errorf("signal = %q; want %q + host=api.example.com", signals[0], c.wantSignal)
			}
		})
	}
}

// TestPromptRecordsExplainMore verifies the popup records the "Explain more"
// click via the recordExplainMore seam (so it lands on the GET channel an
// HTTPS/CONNECT denial cannot reach), and only for that click.
func TestPromptRecordsExplainMore(t *testing.T) {
	cases := []struct {
		name       string
		label      string
		wantRecord bool
		wantIntent bool
	}{
		{"explain-more records", "Explain more", true, true},
		{"deny-once does not record", "Deny once", false, false},
		{"allow-once does not record", "Allow once", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			recorded := false
			p := &Prompter{
				timeout:  time.Second,
				logf:     func(string, ...any) {},
				backends: []dialogBackend{signalBackend{label: c.label}},
				recordExplainMore: func(host string) {
					got = host
					recorded = true
				},
			}
			res := p.Prompt(context.Background(), "api.example.com", 443)
			if res.NeedsIntent != c.wantIntent {
				t.Errorf("NeedsIntent = %v; want %v", res.NeedsIntent, c.wantIntent)
			}
			if recorded != c.wantRecord {
				t.Errorf("recordExplainMore called = %v; want %v", recorded, c.wantRecord)
			}
			if c.wantRecord && got != "api.example.com" {
				t.Errorf("recorded host = %q; want api.example.com", got)
			}
		})
	}
}

func TestRegisteredSuffixHint(t *testing.T) {
	cases := map[string]string{
		"api.example.com":    "example.com",
		"a.b.example.com":    "b.example.com",
		"example.com":        "example.com", // 2 labels: unchanged
		"localhost":          "localhost",
		"192.168.1.1":        "192.168.1.1", // IP literal: unchanged
		"2001:db8::1":        "2001:db8::1",
		"registry.npmjs.org": "npmjs.org",
		"deep.sub.host.tld":  "sub.host.tld",
	}
	for in, want := range cases {
		if got := RegisteredSuffixHint(in); got != want {
			t.Errorf("RegisteredSuffixHint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLabelTokenRoundTrip(t *testing.T) {
	suffix := "example.com"
	cases := map[string]string{
		"Allow once":                        tokenAllowOnce,
		"Allow permanently (this host)":     tokenAllowPermanentHost,
		"Allow permanently (*.example.com)": tokenAllowPermanentSuffix,
		"Deny once":                         tokenDenyOnce,
		"Deny permanently (this host)":      tokenDenyPermanentHost,
		"Deny permanently (*.example.com)":  tokenDenyPermanentSuffix,
		"Explain more":                      tokenNeedsIntent,
		"":                                  tokenDenyOnce, // cancel
		"garbage":                           tokenDenyOnce,
	}
	for label, want := range cases {
		if got := labelToToken(label, suffix); got != want {
			t.Errorf("labelToToken(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestTokenToResult(t *testing.T) {
	host, suffix := "api.example.com", "example.com"
	r := tokenToResult(tokenAllowOnce, host, suffix)
	if !r.Allow || r.Persist {
		t.Errorf("allow_once: %+v", r)
	}
	r = tokenToResult(tokenAllowPermanentHost, host, suffix)
	if !r.Allow || !r.Persist || r.Scope != "host" {
		t.Errorf("allow_permanent_host: %+v", r)
	}
	r = tokenToResult(tokenAllowPermanentSuffix, host, suffix)
	if !r.Allow || !r.Persist || r.Scope != "suffix" || r.Suffix != suffix {
		t.Errorf("allow_permanent_suffix: %+v", r)
	}
	r = tokenToResult(tokenDenyOnce, host, suffix)
	if r.Allow || r.Persist {
		t.Errorf("deny_once: %+v", r)
	}
	r = tokenToResult(tokenDenyPermanentSuffix, host, suffix)
	if r.Allow || !r.Persist || r.Scope != "suffix" {
		t.Errorf("deny_permanent_suffix: %+v", r)
	}
	r = tokenToResult(tokenNeedsIntent, host, suffix)
	if r.Allow || r.Persist || !r.NeedsIntent {
		t.Errorf("needs_intent: %+v", r)
	}
}

func TestOptionLabelsExactAndDefault(t *testing.T) {
	opts := optionLabels("example.com")
	want := []string{
		"Allow once",
		"Allow permanently (this host)",
		"Allow permanently (*.example.com)",
		"Deny once",
		"Deny permanently (this host)",
		"Deny permanently (*.example.com)",
		"Explain more",
	}
	if len(opts) != len(want) {
		t.Fatalf("got %d options", len(opts))
	}
	for i := range want {
		if opts[i] != want[i] {
			t.Errorf("option[%d] = %q, want %q", i, opts[i], want[i])
		}
	}
}

func TestPromptTextParity(t *testing.T) {
	got := promptText("api.example.com", 443, "", "", "")
	want := "The sandboxed process is trying to reach:\n\n    api.example.com:443\n\nAgent intent: (not declared)\n\nHow should omac handle this destination?"
	if got != want {
		t.Errorf("promptText (no intent) = %q", got)
	}
}

func TestPromptTextWithIntent(t *testing.T) {
	got := promptText("api.example.com", 443, "fetch release notes", "", "")
	want := "The sandboxed process is trying to reach:\n\n    api.example.com:443\n\nAgent intent: \"fetch release notes\"\n\nHow should omac handle this destination?"
	if got != want {
		t.Errorf("promptText (with intent) = %q", got)
	}
}

func TestPromptTextIntentOnly(t *testing.T) {
	got := promptText("api.example.com", 443, "verify the version", "", "")
	if !strings.Contains(got, "api.example.com:443") {
		t.Errorf("missing host:port: %q", got)
	}
	if !strings.Contains(got, `"verify the version"`) {
		t.Errorf("missing intent: %q", got)
	}
}

func TestPromptTextWithCause(t *testing.T) {
	got := promptText("raw.githubusercontent.com", 443, "", "syntax-highlighting grammar", "")
	if !strings.Contains(got, "Likely cause: syntax-highlighting grammar\n") {
		t.Errorf("missing cause line: %q", got)
	}
	if !strings.Contains(got, "Agent intent: (not declared)") {
		t.Errorf("cause must not replace intent line: %q", got)
	}
	// Cause precedes intent.
	if strings.Index(got, "Likely cause") > strings.Index(got, "Agent intent") {
		t.Errorf("cause should precede intent: %q", got)
	}
}

func TestPromptTextCauseOmittedWhenEmpty(t *testing.T) {
	if strings.Contains(promptText("api.example.com", 443, "", "", ""), "Likely cause") {
		t.Error("empty cause must omit the line")
	}
}

func TestNotificationText(t *testing.T) {
	n := notificationText("api.example.com", 443)
	if !strings.Contains(n, "api.example.com:443") || !strings.Contains(n, "decision dialog is waiting") {
		t.Errorf("notificationText = %q", n)
	}
}

func TestLearnedPolicyPersistsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "p.json")
	lp, err := LoadLearnedPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lp.Record("npmjs.org", "suffix", true); err != nil {
		t.Fatal(err)
	}
	if err := lp.Record("evil.example", "host", false); err != nil {
		t.Fatal(err)
	}

	// Reload from disk and verify nono-compatible shape.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Schema  int `json:"schema"`
		Entries []struct {
			Host     string `json:"host"`
			Scope    string `json:"scope"`
			Decision string `json:"decision"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Schema != 1 || len(f.Entries) != 2 {
		t.Fatalf("file = %s", raw)
	}

	lp2, err := LoadLearnedPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if allow, found := lp2.Lookup("registry.npmjs.org"); !found || !allow {
		t.Error("suffix allow should match subdomain after reload")
	}
	if allow, found := lp2.Lookup("npmjs.org"); !found || !allow {
		t.Error("suffix allow should match suffix itself")
	}
	if allow, found := lp2.Lookup("evil.example"); !found || allow {
		t.Error("host deny should match")
	}
	if _, found := lp2.Lookup("other.example"); found {
		t.Error("unrelated host should not match")
	}
}

func TestLearnedPolicyDenyWins(t *testing.T) {
	lp := &LearnedPolicy{}
	_ = lp.Record("example.com", "suffix", true)
	_ = lp.Record("bad.example.com", "host", false)
	if allow, found := lp.Lookup("bad.example.com"); !found || allow {
		t.Error("host deny must win over suffix allow")
	}
	if allow, found := lp.Lookup("ok.example.com"); !found || !allow {
		t.Error("suffix allow should still apply elsewhere")
	}
}

func TestLearnedPolicyUpsert(t *testing.T) {
	lp := &LearnedPolicy{}
	_ = lp.Record("host.example", "host", true)
	_ = lp.Record("host.example", "host", false)
	if allow, found := lp.Lookup("host.example"); !found || allow {
		t.Error("second record should overwrite the first")
	}
	lp.mu.Lock()
	n := len(lp.entries)
	lp.mu.Unlock()
	if n != 1 {
		t.Errorf("entries = %d, want 1 (upsert)", n)
	}
}

func TestLearnedPolicyRejectsBadSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(path, []byte(`{"schema":2,"entries":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLearnedPolicy(path); err == nil {
		t.Error("schema 2 should be rejected")
	}
}

func TestLearnedPolicyNonoFixture(t *testing.T) {
	// A file shaped exactly like nono writes it must load unchanged.
	path := filepath.Join(t.TempDir(), "tng-sandbox.learned.json")
	fixture := `{"schema":1,"entries":[{"host":"tngtech.com","scope":"suffix","decision":"allow"},{"host":"ads.example","scope":"host","decision":"deny"}]}`
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	lp, err := LoadLearnedPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if allow, found := lp.Lookup("www.tngtech.com"); !found || !allow {
		t.Error("nono fixture suffix allow should work")
	}
	if allow, found := lp.Lookup("ads.example"); !found || allow {
		t.Error("nono fixture host deny should work")
	}
}

func TestLearnedPolicyFileIsPrettyPrinted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "default.pages.json")
	lp, err := LoadLearnedPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lp.Record("npmjs.org", "suffix", true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "\n  ") {
		t.Errorf("pages file not indented: %q", s)
	}
	if !strings.HasSuffix(s, "\n") {
		t.Error("pages file missing trailing newline")
	}
}
