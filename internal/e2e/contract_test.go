//go:build e2e || e2e_fast

package e2e

import "testing"

// TestDeriveContractNonEmpty guards the invariant that every eligible harness
// yields a non-empty CLI contract wired to a binary name. If a refactor of the
// registry ever makes derivation return nothing, the smoke test would silently
// assert nothing — this fails on every PR (e2e_fast) so that can't happen.
func TestDeriveContractNonEmpty(t *testing.T) {
	for _, h := range allHarnesses() {
		c, ok := deriveContract(h.Name)
		if !ok {
			t.Errorf("%s: deriveContract returned ok=false", h.Name)
			continue
		}
		if c.BinaryName == "" {
			t.Errorf("%s: empty BinaryName", h.Name)
		}
		if len(c.Tokens) == 0 {
			t.Errorf("%s: derived contract has no tokens", h.Name)
		}
	}
}

// TestDeriveContractKnownTokens pins the specific flags/subcommands each harness
// is expected to depend on. This is the tripwire: if someone edits RunArgs or
// SystemContextArgs and the derivation stops producing the token the smoke test
// checks, this fails immediately (on every PR) rather than the smoke test
// quietly checking a smaller set.
func TestDeriveContractKnownTokens(t *testing.T) {
	want := map[string][]string{
		"opencode":    {"run", "-m", "--continue", "--session"},
		"claude-code": {"-p", "--model", "--dangerously-skip-permissions", "--append-system-prompt", "--continue", "--resume"},
		"codex":       {"exec", "-m", "--dangerously-bypass-approvals-and-sandbox", "-c", "resume", "--last"},
		"copilot":     {"-p", "--model", "--allow-all-tools", "--continue", "--session-id"},
		"pi":          {"-p", "--provider", "--model", "-c", "--session"},
	}
	for name, tokens := range want {
		c, ok := deriveContract(name)
		if !ok {
			// codex is excluded on darwin; skip rather than fail host-dependently.
			t.Logf("%s: not eligible on this host, skipping", name)
			continue
		}
		have := map[string]bool{}
		for _, tok := range c.Tokens {
			have[tok.Token] = true
		}
		for _, tok := range tokens {
			if !have[tok] {
				t.Errorf("%s: expected contract token %q not derived (have %v)", name, tok, tokenList(c))
			}
		}
	}
}

func tokenList(c harnessContract) []string {
	out := make([]string, 0, len(c.Tokens))
	for _, t := range c.Tokens {
		out = append(out, t.Token)
	}
	return out
}

func TestFlagsAndSub(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantSub  string
		wantFlag []string
	}{
		{"leading subcommand", []string{"run", "--print-logs", "-m", "model/x", "PROMPT"}, "run", []string{"--print-logs", "-m"}},
		{"leading flag no sub", []string{"-p", "PROMPT", "--model", "x", "--allow-all-tools"}, "", []string{"-p", "--model", "--allow-all-tools"}},
		{"config override value not a flag", []string{"-c", "instructions=BRIEFING"}, "", []string{"-c"}},
		{"continue with flag", []string{"resume", "--last"}, "resume", []string{"--last"}},
		{"empty", nil, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, flags := flagsAndSub(tc.args)
			if sub != tc.wantSub {
				t.Errorf("sub = %q, want %q", sub, tc.wantSub)
			}
			if !equalStrings(flags, tc.wantFlag) {
				t.Errorf("flags = %v, want %v", flags, tc.wantFlag)
			}
		})
	}
}

func TestTokenPresentBoundaries(t *testing.T) {
	corpus := `Usage: claude [options]
  -p, --print                  print mode
  --model <name>               model to use
  --append-system-prompt <s>   extra system prompt
  --continue                   continue latest
Commands:
  exec                         run non-interactively`
	present := []string{"-p", "--model", "--append-system-prompt", "--continue", "exec"}
	for _, tok := range present {
		if !tokenPresent(corpus, tok) {
			t.Errorf("expected %q present in corpus", tok)
		}
	}
	// Whole-token boundaries: a removed flag must not match a longer flag that
	// merely contains it as a substring.
	absent := []string{"--session-id", "--prin", "--continu", "xec"}
	for _, tok := range absent {
		if tokenPresent(corpus, tok) {
			t.Errorf("did not expect %q to match in corpus", tok)
		}
	}
}

func TestMissingTokens(t *testing.T) {
	corpus := "  -p, --print\n  --model <x>\n"
	tokens := []contractToken{
		{Token: "-p", Source: "RunArgs"},
		{Token: "--model", Source: "RunArgs"},
		{Token: "--gone", Source: "RunArgs"},
	}
	missing := missingTokens(corpus, tokens)
	if len(missing) != 1 || missing[0].Token != "--gone" {
		t.Fatalf("missing = %+v, want exactly [--gone]", missing)
	}
}

func TestCompatLine(t *testing.T) {
	got := compatLine("claude-code", "2.1.197", "linux", "contract", "PASS")
	want := "OMAC_COMPAT harness=claude-code version=2.1.197 os=linux stage=contract result=PASS"
	if got != want {
		t.Fatalf("compatLine = %q, want %q", got, want)
	}
	if got := compatLine("pi", "", "linux", "llm", "FAIL"); got != "OMAC_COMPAT harness=pi version=unknown os=linux stage=llm result=FAIL" {
		t.Fatalf("empty-version compatLine = %q", got)
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"opencode 1.17.12", "1.17.12"},
		{"v2.1.197", "2.1.197"},
		{"2.1.197 (Claude Code)", "2.1.197"},
		{"WARNING: refusing to create helper binaries under /tmp\ncodex-cli 0.142.5", "0.142.5"},
		{"1.0.68-beta.3", "1.0.68-beta.3"},
		{"copilot 1.0.68.", "1.0.68"},
		{"no version here", "no version here"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := parseVersion(tc.raw); got != tc.want {
			t.Errorf("parseVersion(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
