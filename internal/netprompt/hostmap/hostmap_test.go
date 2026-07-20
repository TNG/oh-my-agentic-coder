package hostmap

import "testing"

func TestForOpencodeLoads(t *testing.T) {
	m := For("opencode")
	if m == nil {
		t.Fatal("For(opencode) returned nil; embedded JSON should parse")
	}
	if _, ok := m.Lookup("raw.githubusercontent.com"); !ok {
		t.Error("expected raw.githubusercontent.com in the opencode map")
	}
}

func TestForCaseInsensitiveAndTrimmed(t *testing.T) {
	if For("  OpenCode ") == nil {
		t.Error("harness name should be trimmed and lowercased")
	}
}

func TestForUnknownHarnessNil(t *testing.T) {
	for _, h := range []string{"codex", "copilot", "claude", ""} {
		if m := For(h); m != nil {
			t.Errorf("For(%q) = non-nil; only opencode has a map", h)
		}
	}
}

func TestLookupCauseNonEmpty(t *testing.T) {
	m := For("opencode")
	e, ok := m.Lookup("models.dev")
	if !ok {
		t.Fatal("models.dev not found")
	}
	if e.Cause == "" {
		t.Error("entry cause is empty")
	}
}

func TestLookupNormalizesHost(t *testing.T) {
	m := For("opencode")
	// Trailing dot + uppercase must still match.
	if _, ok := m.Lookup("RAW.GithubUserContent.com."); !ok {
		t.Error("lookup should normalize case and trailing dot")
	}
}

func TestLookupMiss(t *testing.T) {
	m := For("opencode")
	if _, ok := m.Lookup("not-a-host.invalid"); ok {
		t.Error("unknown host should miss")
	}
	if _, ok := m.Lookup(""); ok {
		t.Error("empty host should miss")
	}
}

func TestNilMapLookupSafe(t *testing.T) {
	var m *Map
	if _, ok := m.Lookup("raw.githubusercontent.com"); ok {
		t.Error("nil map Lookup must return false, not panic")
	}
}

func TestOptInAndNotEgressNotMatched(t *testing.T) {
	m := For("opencode")
	// 1.1.1.1 is recorded in not_egress, never as an egress entry.
	if _, ok := m.Lookup("1.1.1.1"); ok {
		t.Error("not_egress host must not resolve a cause")
	}
}
