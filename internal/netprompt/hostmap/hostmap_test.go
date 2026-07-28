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

func TestForClaudeCodeLoads(t *testing.T) {
	m := For("claude-code")
	if m == nil {
		t.Fatal("For(claude-code) returned nil; embedded JSON should parse")
	}
	if _, ok := m.Lookup("downloads.claude.ai"); !ok {
		t.Error("expected downloads.claude.ai in the claude-code map")
	}
}

func TestForCanonicalOnly_AliasMisses(t *testing.T) {
	// For keys on canonical names; the alias "claude" is resolved to
	// "claude-code" upstream (config.LookupHarness), so For itself must not
	// accept it. Unknown/empty harnesses also miss.
	for _, h := range []string{"claude", "cc", "codex", "copilot", ""} {
		if m := For(h); m != nil {
			t.Errorf("For(%q) = non-nil; expected no map", h)
		}
	}
}

// TestSameHostDivergesByHarness locks the reason the map is harness-keyed:
// raw.githubusercontent.com is a tree-sitter grammar in opencode but a
// release-notes changelog in claude-code.
func TestSameHostDivergesByHarness(t *testing.T) {
	oc, _ := For("opencode").Lookup("raw.githubusercontent.com")
	cc, _ := For("claude-code").Lookup("raw.githubusercontent.com")
	if oc.Cause == "" || cc.Cause == "" {
		t.Fatal("both harnesses should map raw.githubusercontent.com")
	}
	if oc.Cause == cc.Cause {
		t.Errorf("expected divergent causes per harness, both = %q", oc.Cause)
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
