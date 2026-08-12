package cli

// The consolidated refusal report and its exit-code priority rule had NO test
// before issue #174: they were inline in a 769-line runLaunch, reachable only
// by launching a harness, so `grep "refusing to start\|ExitSecretRefused"`
// across every _test.go returned nothing. Extracting renderSkillRefusal as a
// pure function over (writer, problems) is what makes these assertions possible.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/skillstate"
)

func render(problems []skillstate.Problem) (string, int) {
	var buf bytes.Buffer
	code := renderSkillRefusal(&buf, "omac start", problems)
	return buf.String(), code
}

// TestRenderSkillRefusalNoProblems: a clean preflight prints nothing and does
// not claim a refusal.
func TestRenderSkillRefusalNoProblems(t *testing.T) {
	out, code := render(nil)
	if out != "" {
		t.Errorf("output = %q, want empty", out)
	}
	if code != ExitOK {
		t.Errorf("code = %d, want ExitOK", code)
	}
}

// TestRenderSkillRefusalEveryKind checks that each problem class gets a section
// naming both the affected skill and its remedy. A class rendered without its
// fix string is the failure mode that sends a user to the docs.
func TestRenderSkillRefusalEveryKind(t *testing.T) {
	cases := []struct {
		name    string
		problem skillstate.Problem
		want    []string
	}{
		{
			name: "meta broken",
			problem: skillstate.Problem{Kind: skillstate.MetaBroken, Skill: "alpha",
				Detail: "yaml: line 3: bad", Fix: "omac register --force alpha"},
			want: []string{"omac.yaml broken", "alpha", "yaml: line 3: bad", "omac register --force alpha"},
		},
		{
			name: "bundle drift",
			problem: skillstate.Problem{Kind: skillstate.BundleDrift, Skill: "bravo",
				Detail: "bundle changed since register", Fix: "omac register --force bravo"},
			want: []string{"bundle changed since register", "--accept-skill-changes", "bravo", "omac register --force bravo"},
		},
		{
			name: "missing secret",
			problem: skillstate.Problem{Kind: skillstate.MissingSecret, Skill: "charlie", Field: "TOKEN",
				Detail: "required secret missing", Fix: "omac secrets set charlie TOKEN"},
			want: []string{"required secret missing", "charlie/TOKEN", "omac secrets set charlie TOKEN"},
		},
		{
			name: "invalid secret",
			problem: skillstate.Problem{Kind: skillstate.InvalidSecret, Skill: "delta", Field: "TOKEN",
				Detail: "value for TOKEN does not match /^tngai_/", Fix: "fix the exported value, or run omac secrets set delta TOKEN"},
			want: []string{"pattern validation", "--skip-secret-pattern", "delta/TOKEN",
				"does not match", "fix the exported value"},
		},
		{
			name: "missing field",
			problem: skillstate.Problem{Kind: skillstate.MissingField, Skill: "echo", Field: "API_BASE",
				Detail: "required config field missing", Fix: "omac register --reprompt-fields echo"},
			want: []string{"required config field missing", "echo", "fields: API_BASE", "omac register --reprompt-fields echo"},
		},
		{
			name: "keychain unavailable",
			problem: skillstate.Problem{Kind: skillstate.KeychainUnavailable, Skill: "foxtrot", Field: "TOKEN",
				Detail: "dbus: no session bus", Fix: "no Secret Service provider found — install gnome-keyring"},
			want: []string{"keychain unavailable", "foxtrot/TOKEN", "dbus: no session bus", "gnome-keyring"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _ := render([]skillstate.Problem{c.problem})
			if !strings.Contains(out, "refusing to start, found 1 problem(s)") {
				t.Errorf("missing the header:\n%s", out)
			}
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestRenderSkillRefusalExitCodePriority locks the ordering: a dead keychain
// outranks a missing secret because `omac secrets set` cannot work until the
// backend does, and both outrank the config-invalid classes.
func TestRenderSkillRefusalExitCodePriority(t *testing.T) {
	keychainDown := skillstate.Problem{Kind: skillstate.KeychainUnavailable, Skill: "a", Field: "T", Detail: "no bus"}
	missingSecret := skillstate.Problem{Kind: skillstate.MissingSecret, Skill: "b", Field: "T", Fix: "omac secrets set b T"}
	invalidSecret := skillstate.Problem{Kind: skillstate.InvalidSecret, Skill: "c", Field: "T", Detail: "bad", Fix: "fix"}
	missingField := skillstate.Problem{Kind: skillstate.MissingField, Skill: "d", Field: "F", Fix: "omac register --reprompt-fields d"}
	drift := skillstate.Problem{Kind: skillstate.BundleDrift, Skill: "e", Fix: "omac register --force e"}
	metaBroken := skillstate.Problem{Kind: skillstate.MetaBroken, Skill: "f", Detail: "bad yaml"}

	cases := []struct {
		name     string
		problems []skillstate.Problem
		want     int
	}{
		{"drift alone is config-invalid", []skillstate.Problem{drift}, ExitConfigInvalid},
		{"broken meta alone is config-invalid", []skillstate.Problem{metaBroken}, ExitConfigInvalid},
		{"missing secret is refused", []skillstate.Problem{missingSecret}, ExitSecretRefused},
		{"invalid secret is refused", []skillstate.Problem{invalidSecret}, ExitSecretRefused},
		{"missing field is refused", []skillstate.Problem{missingField}, ExitSecretRefused},
		{"refused outranks config-invalid", []skillstate.Problem{drift, missingField}, ExitSecretRefused},
		{"keychain outranks refused", []skillstate.Problem{missingSecret, keychainDown}, ExitKeychainError},
		{"keychain outranks everything", []skillstate.Problem{drift, metaBroken, missingField, keychainDown}, ExitKeychainError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, code := render(c.problems); code != c.want {
				t.Errorf("code = %d, want %d", code, c.want)
			}
		})
	}
}

// TestRenderSkillRefusalReportsEveryClassForOneSkill: start accumulates instead
// of returning on the first problem precisely so the user isn't sent round the
// loop N times for N problems. A skill hitting several classes must therefore
// appear in every relevant section, not just the first.
func TestRenderSkillRefusalReportsEveryClassForOneSkill(t *testing.T) {
	problems := []skillstate.Problem{
		{Kind: skillstate.BundleDrift, Skill: "alpha", Fix: "omac register --force alpha"},
		{Kind: skillstate.MissingSecret, Skill: "alpha", Field: "TOKEN", Fix: "omac secrets set alpha TOKEN"},
		{Kind: skillstate.MissingField, Skill: "alpha", Field: "API_BASE", Fix: "omac register --reprompt-fields alpha"},
	}
	out, code := render(problems)

	if !strings.Contains(out, "found 3 problem(s)") {
		t.Errorf("want all three counted:\n%s", out)
	}
	for _, want := range []string{
		"omac register --force alpha",
		"omac secrets set alpha TOKEN",
		"omac register --reprompt-fields alpha",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q — the user would need another round trip:\n%s", want, out)
		}
	}
	if code != ExitSecretRefused {
		t.Errorf("code = %d, want ExitSecretRefused", code)
	}
}

// TestRenderSkillRefusalGroupsFieldsPerSkill: one `omac register
// --reprompt-fields` run re-prompts ALL of a skill's fields, so printing the
// command once per field would imply it must be run repeatedly.
func TestRenderSkillRefusalGroupsFieldsPerSkill(t *testing.T) {
	problems := []skillstate.Problem{
		{Kind: skillstate.MissingField, Skill: "alpha", Field: "A", Fix: "omac register --reprompt-fields alpha"},
		{Kind: skillstate.MissingField, Skill: "alpha", Field: "B", Fix: "omac register --reprompt-fields alpha"},
		{Kind: skillstate.MissingField, Skill: "bravo", Field: "C", Fix: "omac register --reprompt-fields bravo"},
	}
	out, _ := render(problems)

	if !strings.Contains(out, "fields: A, B") {
		t.Errorf("want alpha's fields on one line:\n%s", out)
	}
	if n := strings.Count(out, "omac register --reprompt-fields alpha"); n != 1 {
		t.Errorf("alpha's fix printed %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "fields: C") {
		t.Errorf("want bravo's own line:\n%s", out)
	}
}

// TestRenderSkillRefusalOmitsAbsentSections: a preflight that hit one class must
// not print empty headers for the other five.
func TestRenderSkillRefusalOmitsAbsentSections(t *testing.T) {
	out, _ := render([]skillstate.Problem{
		{Kind: skillstate.MissingSecret, Skill: "alpha", Field: "TOKEN", Fix: "omac secrets set alpha TOKEN"},
	})
	for _, unwanted := range []string{
		"omac.yaml broken", "bundle changed", "pattern validation",
		"required config field missing", "keychain unavailable",
	} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output should not contain the empty section %q:\n%s", unwanted, out)
		}
	}
}

// TestRenderSkillRefusalNoFixNoInvention: an opaque keychain failure (a macOS
// authorization denial) has no known remedy, so the report shows the cause
// rather than inventing advice like "install gnome-keyring" on a Mac.
func TestRenderSkillRefusalNoFixNoInvention(t *testing.T) {
	out, code := render([]skillstate.Problem{{
		Kind: skillstate.KeychainUnavailable, Skill: "alpha", Field: "TOKEN",
		Detail: "keychain get omac/alpha/TOKEN: authorization denied",
	}})
	if !strings.Contains(out, "authorization denied") {
		t.Errorf("want the raw cause:\n%s", out)
	}
	if strings.Contains(out, "Secret Service") || strings.Contains(out, "gnome-keyring") {
		t.Errorf("must not invent a remedy for an opaque failure:\n%s", out)
	}
	if strings.Contains(out, "— \n") || strings.HasSuffix(strings.TrimSpace(out), "—") {
		t.Errorf("dangling em dash from an empty Fix:\n%s", out)
	}
	if code != ExitKeychainError {
		t.Errorf("code = %d, want ExitKeychainError", code)
	}
}

// TestRenderSkillRefusalUsesPrefix: the same renderer serves `omac start`,
// `omac continue` and `omac resume`, so a failure surfaced via continue must not
// be mislabeled as start.
func TestRenderSkillRefusalUsesPrefix(t *testing.T) {
	var buf bytes.Buffer
	renderSkillRefusal(&buf, "omac continue", []skillstate.Problem{
		{Kind: skillstate.BundleDrift, Skill: "alpha", Fix: "omac register --force alpha"},
	})
	if !strings.HasPrefix(buf.String(), "omac continue: refusing to start") {
		t.Errorf("want the caller's prefix:\n%s", buf.String())
	}
}
