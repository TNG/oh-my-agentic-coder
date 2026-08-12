package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/skillstate"
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

	// Section order is most-fundamental-first. A dead keychain leads because
	// the next section's remedy (`omac secrets set`) cannot work until it is
	// fixed — telling a headless user to store a secret in a keychain that
	// isn't running is issue #174's Failure 4.
	perField(w, problems, skillstate.KeychainUnavailable,
		"keychain unavailable — a required secret could not be read:")

	perSkill(w, problems, skillstate.MetaBroken,
		config.MetaFileName+" broken:")

	perSkill(w, problems, skillstate.BundleDrift,
		"bundle changed since register (pass --accept-skill-changes to proceed, or re-register):")

	perField(w, problems, skillstate.MissingSecret,
		"required secret missing:")

	perField(w, problems, skillstate.InvalidSecret,
		"secret from environment failed pattern validation (pass --skip-secret-pattern if the pattern is outdated):")

	// Config fields are grouped per skill: one `omac register
	// --reprompt-fields` run re-prompts all of a skill's fields, so listing
	// the command per field would be misleading.
	fieldsBySkill(w, problems, skillstate.MissingField,
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
// "<skill> — fields: A, B — <fix>".
func fieldsBySkill(w io.Writer, problems []skillstate.Problem, kind skillstate.ProblemKind, header string) {
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
		fmt.Fprintf(w, "    %s — fields: %s — %s\n",
			skill, strings.Join(fields[skill], ", "), fix[skill])
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
	case p.Kind == skillstate.MissingSecret || p.Kind == skillstate.BundleDrift:
		// The header already states the cause ("required secret missing",
		// "bundle changed since register"); repeating it adds no information.
		return p.Fix
	default:
		return p.Detail + " — " + p.Fix
	}
}
