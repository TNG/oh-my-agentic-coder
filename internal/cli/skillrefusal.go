package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/skillstate"
)

// renderSkillRefusal prints one consolidated report of why a launch cannot
// proceed and returns the exit code to use. It is `omac start`'s presentation
// of skillstate's []Problem; serve renders the same problems as route states,
// reload as a warning plus a stub route, doctor as warnings.
//
// Problems are grouped by class so each hint is printed once with the affected
// skills under it, rather than repeating the same remedy N times. A skill that
// hits several classes appears in every relevant section — we deliberately do
// not short-circuit to "first failing class", because the user's complaint
// when start returned early was that fixing skill A revealed skill B revealed
// skill C: N invocations to fix N problems.
//
// Kept as a pure function over (writer, problems) so the sections and the
// exit-code priority rule below are directly testable. Before #174 this logic
// was inline in a 769-line runLaunch and reachable only by launching a harness,
// which is why it had no test at all.
func renderSkillRefusal(w io.Writer, prefix string, problems []skillstate.Problem) int {
	if len(problems) == 0 {
		return ExitOK
	}
	fmt.Fprintf(w, prefix+": refusing to start, found %d problem(s):\n", len(problems))
	s := stylerFor(w)

	// Section order is most-fundamental-first. A dead keychain leads because
	// the next section's remedy (`omac secrets set`) cannot work until it is
	// fixed — telling a headless user to store a secret in a keychain that
	// isn't running is issue #174's Failure 4.
	keychainSection(w, problems)

	perSkill(w, problems, skillstate.MetaBroken,
		config.MetaFileName+" broken:")

	// Bundle drift uses skillProblemLine so the skill name is labeled
	// separately from the copy-pasteable command (issue #227).
	labeledSkills(w, s, problems, skillstate.BundleDrift,
		"bundle changed since register (pass --accept-skill-changes to proceed, or re-register):",
		"re-register")

	perField(w, problems, skillstate.MissingSecret,
		"required secret missing:")

	perField(w, problems, skillstate.InvalidSecret,
		"secret from environment failed pattern validation (pass --skip-secret-pattern if the pattern is outdated):")

	// Config fields are grouped per skill: one `omac register <skill>
	// --reprompt-fields` run re-prompts all of a skill's fields, so listing
	// the command per field would be misleading.
	fieldsBySkill(w, s, problems, skillstate.MissingField,
		"required config field missing:")

	fmt.Fprintln(w)

	// Pick the most actionable exit code. Keychain-unavailable outranks
	// everything: no per-skill fix applies until the backend works. Then
	// secrets/fields refused, usually a one-command fix. Bundle drift and
	// broken metas are config-invalid — drift because the user has not
	// explicitly accepted the change yet.
	if skillstate.Has(problems, skillstate.KeychainUnavailable) {
		return ExitKeychainError
	}
	if skillstate.Has(problems, skillstate.MissingSecret, skillstate.InvalidSecret, skillstate.MissingField) {
		return ExitSecretRefused
	}
	return ExitConfigInvalid
}

// keychainSection renders the keychain-unavailable class. Unlike every other
// class its remedy is OS-derived rather than per-skill, so it is printed ONCE
// under the header instead of once per row — a dead backend on a host with a few
// skills would otherwise repeat the same ~250-character gnome-keyring/dbus
// instructions a dozen times and bury the list of affected secrets.
func keychainSection(w io.Writer, problems []skillstate.Problem) {
	var rows []skillstate.Problem
	var fixes []string
	seenFix := map[string]bool{}
	for _, p := range problems {
		if p.Kind != skillstate.KeychainUnavailable {
			continue
		}
		rows = append(rows, p)
		if p.Fix != "" && !seenFix[p.Fix] {
			seenFix[p.Fix] = true
			fixes = append(fixes, p.Fix)
		}
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(w, "\n  keychain unavailable — a required secret could not be read:")
	for _, p := range rows {
		fmt.Fprintf(w, "    %s/%s — %s\n", p.Skill, p.Field, p.Detail)
	}
	for _, fix := range fixes {
		fmt.Fprintf(w, "    → %s\n", fix)
	}
}

// stylerFor colors commands when w is a real terminal; tests writing to a
// buffer get the unstyled form so assertions stay stable.
func stylerFor(w io.Writer) styler {
	if f, ok := w.(*os.File); ok {
		return newStyler(f)
	}
	return styler{}
}

// perSkill renders a skill-level class: "<skill> — <detail/fix>".
func perSkill(w io.Writer, problems []skillstate.Problem, kind skillstate.ProblemKind, header string) {
	first := true
	for _, p := range problems {
		if p.Kind != kind {
			continue
		}
		if first {
			fmt.Fprintln(w, "\n  "+header)
			first = false
		}
		fmt.Fprintf(w, "    %s — %s\n", p.Skill, remedy(p))
	}
}

// labeledSkills renders a skill-level class through skillProblemLine so the
// remedy command is visually split from the skill name (issue #227).
func labeledSkills(w io.Writer, s styler, problems []skillstate.Problem, kind skillstate.ProblemKind, header, label string) {
	first := true
	for _, p := range problems {
		if p.Kind != kind {
			continue
		}
		if first {
			fmt.Fprintln(w, "\n  "+header)
			first = false
		}
		fmt.Fprintln(w, skillProblemLine(s, p.Skill, label, p.Fix))
	}
}

// perField renders a field-level class: "<skill>/<field> — <detail/fix>".
func perField(w io.Writer, problems []skillstate.Problem, kind skillstate.ProblemKind, header string) {
	first := true
	for _, p := range problems {
		if p.Kind != kind {
			continue
		}
		if first {
			fmt.Fprintln(w, "\n  "+header)
			first = false
		}
		fmt.Fprintf(w, "    %s/%s — %s\n", p.Skill, p.Field, remedy(p))
	}
}

// fieldsBySkill collapses a field-level class into one line per skill:
// "<skill> — fields: A, B — set them with: <fix>".
func fieldsBySkill(w io.Writer, s styler, problems []skillstate.Problem, kind skillstate.ProblemKind, header string) {
	var order []string
	fields := map[string][]string{}
	fix := map[string]string{}
	for _, p := range problems {
		if p.Kind != kind {
			continue
		}
		if _, seen := fields[p.Skill]; !seen {
			order = append(order, p.Skill)
		}
		fields[p.Skill] = append(fields[p.Skill], p.Field)
		fix[p.Skill] = p.Fix
	}
	if len(order) == 0 {
		return
	}
	fmt.Fprintln(w, "\n  "+header)
	for _, skill := range order {
		fmt.Fprintln(w, skillProblemLine(s, skill,
			"fields: "+strings.Join(fields[skill], ", ")+" — set them with",
			fix[skill]))
	}
}

// remedy renders a problem's actionable tail: the fix command when there is
// one, prefixed by the cause when the cause isn't already implied by the
// section header. A problem with no known fix (an opaque keychain failure)
// shows only its cause rather than inventing advice.
func remedy(p skillstate.Problem) string {
	switch {
	case p.Fix == "":
		return p.Detail
	case p.Kind == skillstate.MissingSecret:
		// The header already states the cause ("required secret missing");
		// repeating it adds no information. Bundle drift is rendered via
		// labeledSkills instead of remedy.
		return p.Fix
	default:
		return p.Detail + " — " + p.Fix
	}
}
