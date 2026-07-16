//go:build e2e || e2e_fast

package e2e

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// contractToken is a single CLI flag or subcommand that omac relies on when it
// drives a harness. If a harness release removes or renames one, omac breaks in
// a way no unit test catches — the failure only shows up when a real agent run
// is attempted. The smoke tier greps every token out of the installed harness's
// own --help so version drift fails fast (and cheaply, before the billed model
// run) with a precise "this flag disappeared" signal.
type contractToken struct {
	// Token is the literal flag or subcommand as omac emits it
	// (e.g. "--append-system-prompt", "exec", "-c").
	Token string
	// Source names where in omac's registry the token comes from, so a failure
	// message points at the code to fix (e.g. "RunArgs", "continue",
	// "system-prompt").
	Source string
}

// harnessContract is the derived set of CLI tokens omac depends on for one
// harness, plus the subcommands whose --help pages must be searched to find
// them. Deriving this from the live registry (rather than a hand-maintained
// list) means a change to SystemContextArgs/RunArgs/Session in config.harness
// or e2e.harnesses automatically updates what the smoke test checks — the two
// can never silently drift apart.
type harnessContract struct {
	Name       string
	BinaryName string
	Tokens     []contractToken
	// Subcommands are the leading positionals omac invokes (e.g. codex "exec",
	// opencode "run", codex "resume"). Their --help pages are folded into the
	// search corpus so a flag documented only under a subcommand still resolves.
	Subcommands []string
}

// deriveContract builds the CLI contract for a harness by inspecting the same
// argv builders omac uses in production (config.Harness) and in the e2e driver
// (harnessConfig.RunArgs). Runtime values (the prompt, model id, session id,
// briefing text) are placeholder-substituted and then discarded — only literal
// flags and subcommands survive into the contract. Returns ok=false for an
// unknown harness name.
func deriveContract(name string) (harnessContract, bool) {
	hc, ok := harnessByName(name)
	if !ok {
		return harnessContract{}, false
	}
	reg, ok := config.LookupHarness(name)
	if !ok {
		return harnessContract{}, false
	}

	c := harnessContract{Name: name, BinaryName: hc.BinaryName}
	subs := map[string]bool{}
	seen := map[string]bool{}

	add := func(source string, args []string) {
		sub, flags := flagsAndSub(args)
		if sub != "" {
			subs[sub] = true
			if key := sub + "\x00" + source; !seen[key] {
				seen[key] = true
				c.Tokens = append(c.Tokens, contractToken{Token: sub, Source: source})
			}
		}
		for _, f := range flags {
			if key := f + "\x00" + source; !seen[key] {
				seen[key] = true
				c.Tokens = append(c.Tokens, contractToken{Token: f, Source: source})
			}
		}
	}

	add("RunArgs", hc.RunArgs("PROMPT"))
	if reg.SystemContextArgs != nil {
		add("system-prompt", reg.SystemContextArgs("BRIEFING"))
	}
	if reg.Session != nil {
		add("continue", reg.Session.ContinueArgs)
		if reg.Session.ResumeByIDArgs != nil {
			add("resume", reg.Session.ResumeByIDArgs("SESSION_ID"))
		}
	}

	for s := range subs {
		c.Subcommands = append(c.Subcommands, s)
	}
	return c, true
}

// flagsAndSub splits an argv into its leading subcommand (a leading non-flag
// positional, if any) and its flag tokens. Non-leading positionals are runtime
// values (prompt, model id, session id, "instructions=<briefing>") and are
// dropped. A token counts as a flag iff it begins with "-".
func flagsAndSub(args []string) (sub string, flags []string) {
	for i, a := range args {
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			continue
		}
		if i == 0 {
			sub = a
		}
	}
	return sub, flags
}

// missingTokens returns the contract tokens that do NOT appear as whole tokens
// anywhere in the combined --help corpus. A token matches when it is bounded by
// start/end-of-text or a non-flag-character (whitespace, "=", ","), so "-p"
// does not spuriously match inside "--print" and "--session" does not match
// "--session-id". An empty return means the installed version still honors every
// assumption omac makes.
func missingTokens(corpus string, tokens []contractToken) []contractToken {
	var missing []contractToken
	for _, t := range tokens {
		if !tokenPresent(corpus, t.Token) {
			missing = append(missing, t)
		}
	}
	return missing
}

// tokenPresent reports whether token appears as a whole token in corpus.
func tokenPresent(corpus, token string) bool {
	// Left boundary: start or a whitespace/'['/'(' (help renders flags after
	// spaces, commas, and inside usage brackets). Right boundary: end or a
	// non-flag character (a flag/subcommand never continues into a word char,
	// '-', or '/').
	re := regexp.MustCompile(`(^|[\s,\[(<|/"'` + "`" + `])` + regexp.QuoteMeta(token) + `($|[\s,.\])>=|/"'` + "`" + `])`)
	return re.MatchString(corpus)
}

// versionRe matches a semver-ish token (optionally v-prefixed) anywhere in a
// --version banner. Harnesses interleave the version with warnings, build
// metadata, or a product name, so a positional "first line" heuristic is
// unreliable (codex, for one, prints a /tmp PATH-alias warning first).
var versionRe = regexp.MustCompile(`\bv?\d+\.\d+\.\d+[\w.\-+]*`)

// parseVersion extracts the harness version from raw `--version` output: the
// first semver-ish token if present, else the first non-empty line.
func parseVersion(raw string) string {
	if m := versionRe.FindString(raw); m != "" {
		// A trailing '.' is punctuation from the banner ("copilot 1.0.68."),
		// never part of a semver.
		return strings.TrimRight(strings.TrimPrefix(m, "v"), ".")
	}
	for _, line := range strings.Split(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return strings.TrimSpace(raw)
}

// compatLine renders the stable machine-readable line the smoke test prints so
// the workflow can parse it into the compatibility matrix without re-deriving
// anything. The date and omac version are stamped by the workflow (they are not
// known to, or stable within, the test process). Example:
//
//	OMAC_COMPAT harness=claude-code version=2.1.197 os=linux stage=contract result=PASS
func compatLine(harness, version, goos, stage, result string) string {
	if version == "" {
		version = "unknown"
	}
	return fmt.Sprintf("OMAC_COMPAT harness=%s version=%s os=%s stage=%s result=%s",
		harness, sanitizeField(version), goos, stage, result)
}

// sanitizeField collapses whitespace in a value so it stays on the single
// OMAC_COMPAT line the workflow greps for.
func sanitizeField(s string) string {
	return strings.Join(strings.Fields(s), "_")
}
