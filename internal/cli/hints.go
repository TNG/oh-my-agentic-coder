// Remediation hints shared by the consolidated "refusing to start" reports
// that `omac start` and `omac serve` print. Both commands list the same
// classes of skill problem, so the rendering lives here to keep the suggested
// commands identical and copy-pasteable.
package cli

import "strings"

// registerCmd renders an `omac register` invocation in the order the command
// documents itself (`Usage: omac register <skill> [flags]`), i.e. skill name
// first and flags after it.
//
// The order is cosmetic to the parser — reorderFlagsFirst normalizes either
// form — but it matters to the reader: it matches the usage line, matches the
// disambiguation hints in register.go, and stays correct when the user appends
// another flag (typically `--harness`) to the command we handed them.
func registerCmd(skill string, flags ...string) string {
	cmd := "omac register " + skill
	if len(flags) > 0 {
		cmd += " " + strings.Join(flags, " ")
	}
	return cmd
}

// skillProblemLine renders one entry of a problem report: the offending skill,
// a label naming the remedy, then the command to run.
//
// The label is not optional. Without it the line reads as one run-on string
// ("skill-marketplace — omac register ..."), which is ambiguous about where the
// command starts; see issue #227. Coloring the command reinforces the split on
// terminals that support it.
func skillProblemLine(s styler, skill, label, cmd string) string {
	return "    " + skill + " — " + label + ": " + s.cyan(cmd)
}
