package netprompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	got := promptText("api.example.com", 443)
	want := "The sandboxed process is trying to reach:\n\n    api.example.com:443\n\nHow should omac handle this destination?"
	if got != want {
		t.Errorf("promptText = %q", got)
	}
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
