package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/skillstate"
	"github.com/TNG/oh-my-agentic-coder/internal/skilltrust"
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

	// Skill-derived fields are stripped of display controls here, at the sink,
	// so a problem carrying hostile bytes cannot repaint or forge the report
	// regardless of which section renders it.
	problems = sanitizeProblems(problems)

	// Section order is most-fundamental-first. A dead keychain leads because
	// the next section's remedy (`omac secrets set`) cannot work until it is
	// fixed — telling a headless user to store a secret in a keychain that
	// isn't running is issue #174's Failure 4.
	keychainSection(w, problems)

	perSkill(w, problems, skillstate.MetaBroken,
		config.MetaFileName+" broken:")

	// Bundle drift uses skillProblemLine so the skill name is labeled
	// separately from the copy-pasteable command (issue #227). The remedy is
	// review-then-re-register, not the drift-acceptance flag: a change the
	// user did not make may have been written inside the sandbox, and the
	// flag must not be the recommended way past that.
	labeledSkills(w, s, problems, skillstate.BundleDrift,
		"files changed since register, so code and SKILL.md instructions are untrusted (review it on the host and re-register; a change you did not make may have been made inside the sandbox):",
		"re-register")

	perField(w, problems, skillstate.MissingSecret,
		"required secret missing:")

	perField(w, problems, skillstate.InvalidSecret,
		"secret from environment failed pattern validation (pass --skip-secret-pattern if the pattern is outdated):")

	perField(w, problems, skillstate.InvalidConfig,
		"config value is not one approved at register (re-register):")

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

// sanitizeProblems returns a copy of problems with every text field run through
// stripControlChars. The report is the operator's trusted explanation of why a
// launch was refused, so none of its inputs may carry display controls.
func sanitizeProblems(problems []skillstate.Problem) []skillstate.Problem {
	if len(problems) == 0 {
		return problems
	}
	out := make([]skillstate.Problem, len(problems))
	for i, p := range problems {
		p.Skill = stripControlChars(p.Skill)
		p.Field = stripControlChars(p.Field)
		p.Detail = stripControlChars(p.Detail)
		p.Fix = stripControlChars(p.Fix)
		out[i] = p
	}
	return out
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

// spawnRefusalClass is one class of spawn-gate refusal, described once for
// every place that shows it: the launch report's section header, the `omac
// doctor` warning line and the live-reload line (issue #514, F8). A new
// refusal kind gets a row here and nothing else.
type spawnRefusalClass struct {
	kind skillstate.ProblemKind
	// cause is the plain-words reason, lower-case, without a trailing colon.
	cause string
	// docs is an optional pointer appended to the cause in parentheses.
	docs string
	// label names what the per-skill command does ("rebuild with"); empty
	// when the class carries no command and the row shows the cause instead.
	label string
	// doctorTag is the `[warn] <tag>:` prefix doctor uses for the class.
	doctorTag string
}

// spawnRefusalClasses lists the classes in report order: the stale-snapshot
// class leads because it is the upgrade case every previously registered
// skill lands in after a copy-policy bump.
var spawnRefusalClasses = []spawnRefusalClass{
	{kind: skillstate.SnapshotStale,
		cause:     "registered before " + skilltrust.SnapshotPolicySince + ", snapshot must be rebuilt once for the new store format",
		docs:      "see docs/troubleshooting.md#upgrading",
		label:     "rebuild with",
		doctorTag: "skill snapshots"},
	{kind: skillstate.NotApproved,
		// NotApproved covers both a never-registered skill and one whose files
		// changed after registration (live reload and doctor skip the drift
		// check), so the wording must fit both.
		cause:     "current version not approved from a host terminal (new skill, or changed since it was registered)",
		label:     "register with",
		doctorTag: "skill registrations"},
	{kind: skillstate.SnapshotMissing,
		cause:     "snapshot missing from the host store",
		label:     "rebuild with",
		doctorTag: "skill snapshots"},
	{kind: skillstate.SpawnRefused,
		cause:     "could not be checked (fix the cause, then relaunch)",
		doctorTag: "skill approvals"},
}

// spawnRefusalClassOf returns the class for kind, or nil for a kind the spawn
// gate never produces.
func spawnRefusalClassOf(kind skillstate.ProblemKind) *spawnRefusalClass {
	for i := range spawnRefusalClasses {
		if spawnRefusalClasses[i].kind == kind {
			return &spawnRefusalClasses[i]
		}
	}
	return nil
}

// header is the launch report's section header: the cause with a capital
// first letter, the docs pointer, and a colon.
func (c spawnRefusalClass) header() string {
	h := c.cause
	if h != "" {
		h = strings.ToUpper(h[:1]) + h[1:]
	}
	if c.docs != "" {
		h += " (" + c.docs + ")"
	}
	return h + ":"
}

// doctorHeader is the `omac doctor` warning line for n skills of this class.
func (c spawnRefusalClass) doctorHeader(n int) string {
	h := fmt.Sprintf("[warn] %s: %d skill(s) %s", c.doctorTag, n, c.cause)
	if c.label != "" {
		h += "; run each command from a host terminal"
	}
	if c.docs != "" {
		h += " (" + c.docs + ")"
	}
	return h + ":"
}

// renderApprovalRefusals prints one grouped report of the skills the spawn
// gate refused at launch (issue #514). Unlike renderSkillRefusal the launch
// goes on: the refused skills are mounted as broken routes the agent can see,
// so there is no exit code, and the header says so instead of "refusing to
// start". The remedy sentence appears once in the header and each class
// states its cause once, with one skillProblemLine per skill under it.
func renderApprovalRefusals(w io.Writer, prefix string, problems []skillstate.Problem) {
	if len(problems) == 0 {
		return
	}
	problems = sanitizeProblems(problems)
	s := stylerFor(w)
	skills := map[string]bool{}
	for _, p := range problems {
		skills[p.Skill] = true
	}
	fmt.Fprintf(w, "%s: %d skill(s) will not run this session. Run each command from a host terminal, outside the sandbox:\n", prefix, len(skills))

	for _, c := range spawnRefusalClasses {
		if c.label != "" {
			labeledSkills(w, s, problems, c.kind, c.header(), c.label)
		} else {
			perSkill(w, problems, c.kind, c.header())
		}
	}

	fmt.Fprintln(w, "\n  The session starts without them; the agent sees them listed as unavailable.")
}
