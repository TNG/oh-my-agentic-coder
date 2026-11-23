package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/approver"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
)

// approvalsSummary is the one line `omac register` prints about what the
// facade will enforce for the skill. Answers stored with "allow always"
// still apply, so it points at `omac approvals list`.
func approvalsSummary(m *config.Meta) string {
	if m == nil || m.Sidecar == nil || m.Sidecar.Approvals == nil {
		return "  approvals: no approvals block; every request to this skill is forwarded without asking"
	}
	unasked := 0
	for _, e := range m.Sidecar.Approvals.Endpoints {
		if e.Approval == approval.None {
			unasked++
		}
	}
	asks := "every request to this skill asks you first"
	if unasked > 0 {
		asks = fmt.Sprintf("%d endpoint(s) run without asking; every other request asks you first", unasked)
	}
	return "  approvals: " + asks + ", unless you answered \"allow always\" (see `omac approvals list`)"
}

// runApprovals implements `omac approvals list|revoke`: the always-verdicts
// given in the sidecar approval dialog. They are stored under ~/.config/omac,
// which the sandbox cannot reach, so this runs from a host terminal.
func runApprovals(args []string, env *Env) int {
	if len(args) == 0 {
		printApprovalsUsage(env.Stderr)
		return ExitMisuse
	}
	if args[0] == "help" || wantsHelp(args) {
		printApprovalsUsage(env.Stdout)
		return ExitOK
	}
	verb, rest := args[0], args[1:]
	if verb != "list" && verb != "revoke" {
		fmt.Fprintf(env.Stderr, "omac approvals: unknown verb %q\n", verb)
		printApprovalsUsage(env.Stderr)
		return ExitMisuse
	}
	if registry.GlobalDir() == "" {
		fmt.Fprintf(env.Stderr, "omac approvals %s: no host config directory (set $HOME or $XDG_CONFIG_HOME)\n", verb)
		return ExitIOError
	}
	store := approver.NewStore("")
	switch verb {
	case "list":
		if len(rest) > 0 {
			fmt.Fprintf(env.Stderr, "omac approvals list: unexpected argument %q\n", rest[0])
			return ExitMisuse
		}
		return listApprovals(store, env)
	default:
		if len(rest) != 1 || strings.HasPrefix(rest[0], "-") {
			fmt.Fprintln(env.Stderr, "omac approvals revoke: want exactly one id (see `omac approvals list`)")
			return ExitMisuse
		}
		found, err := store.Revoke(rest[0])
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac approvals revoke:", err)
			return ExitIOError
		}
		if !found {
			fmt.Fprintf(env.Stderr, "omac approvals revoke: no stored approval %q\n", rest[0])
			return ExitMisuse
		}
		fmt.Fprintf(env.Stdout, "revoked %s; the next request to that endpoint asks again\n", rest[0])
		return ExitOK
	}
}

func listApprovals(store *approver.Store, env *Env) int {
	verdicts, err := store.List()
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac approvals list:", err)
		return ExitIOError
	}
	if len(verdicts) == 0 {
		fmt.Fprintln(env.Stdout, "no stored approvals")
		return ExitOK
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSKILL\tENDPOINT\tPROJECT\tSINCE")
	for _, v := range verdicts {
		endpoint := v.Match
		switch v.Approval {
		case approver.KindUnlisted:
			endpoint += " (not declared)"
		case approver.KindCatchAll:
			endpoint += " (this route only)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.ID, stripControlChars(v.Skill), stripControlChars(endpoint),
			stripControlChars(approvalProject(v)), v.CreatedAt.Format("2006-01-02"))
	}
	tw.Flush()
	return ExitOK
}

// approvalProject names what a stored answer is bound to.
func approvalProject(v approver.Verdict) string {
	switch {
	case v.Dir != "":
		return v.Dir
	case v.Scope == "global":
		return "all projects (user-global skill in omac serve)"
	}
	return v.Scope
}

func printApprovalsUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: omac approvals <list|revoke>

  list           Show the "allow always" answers given in the sidecar approval dialog.
  revoke <id>    Remove one; the next request to that endpoint asks again.

Answers "for this session" and "deny everything from this skill" are not
stored; they end when omac start or omac serve stops.
`)
}

// revokeStoredApprovals removes the stored "allow always" answers of one
// skill (or, with the "mcp:" prefix, one MCP server). With allScopes it takes
// the answers of every project and of serve's global scope; otherwise only
// those bound to scopeKey, so another project's answers stay.
func revokeStoredApprovals(skill, scopeKey string, allScopes bool) (int, error) {
	if registry.GlobalDir() == "" {
		return 0, nil
	}
	return approver.NewStore("").RevokeWhere(func(v approver.Verdict) bool {
		return v.Skill == skill && (allScopes || v.Scope == scopeKey)
	})
}
