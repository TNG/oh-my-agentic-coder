package cli

import (
	"flag"
	"fmt"
	"text/tabwriter"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/approver"
	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// approvalsSummary is the one line `omac register` prints about what the
// facade will enforce for the skill.
func approvalsSummary(m *config.Meta) string {
	if m == nil || m.Sidecar == nil || m.Sidecar.Approvals == nil {
		return "  approvals: none declared; every endpoint of this skill is forwarded without asking (ungated)"
	}
	required := 0
	for _, e := range m.Sidecar.Approvals.Endpoints {
		if e.Approval == approval.Required {
			required++
		}
	}
	return fmt.Sprintf("  approvals: %d endpoint(s) declared, %d ask you first; anything not declared is rejected",
		len(m.Sidecar.Approvals.Endpoints), required)
}

// runApprovals implements `omac approvals list|revoke`: the always-verdicts
// given in the sidecar approval dialog. They are stored under ~/.config/omac,
// which the sandbox cannot reach, so this runs from a host terminal.
func runApprovals(args []string, env *Env) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printApprovalsUsage(env)
		if len(args) == 0 {
			return ExitMisuse
		}
		return ExitOK
	}
	store := approver.NewStore("")
	switch args[0] {
	case "list":
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
		fmt.Fprintln(tw, "ID\tSKILL\tENDPOINT\tSCOPE\tSINCE")
		for _, v := range verdicts {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.ID, v.Skill, v.Match, v.Scope, v.CreatedAt.Format("2006-01-02"))
		}
		tw.Flush()
		return ExitOK
	case "revoke":
		fs := flag.NewFlagSet("approvals revoke", flag.ContinueOnError)
		fs.SetOutput(env.Stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return ExitMisuse
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(env.Stderr, "omac approvals revoke: want exactly one id (see `omac approvals list`)")
			return ExitMisuse
		}
		found, err := store.Revoke(fs.Arg(0))
		if err != nil {
			fmt.Fprintln(env.Stderr, "omac approvals revoke:", err)
			return ExitIOError
		}
		if !found {
			fmt.Fprintf(env.Stderr, "omac approvals revoke: no stored approval %q\n", fs.Arg(0))
			return ExitMisuse
		}
		fmt.Fprintf(env.Stdout, "revoked %s; the next request to that endpoint asks again\n", fs.Arg(0))
		return ExitOK
	default:
		fmt.Fprintf(env.Stderr, "omac approvals: unknown verb %q\n", args[0])
		printApprovalsUsage(env)
		return ExitMisuse
	}
}

func printApprovalsUsage(env *Env) {
	fmt.Fprint(env.Stderr, `Usage: omac approvals <list|revoke>

  list           Show the "allow always" answers given in the sidecar approval dialog.
  revoke <id>    Remove one; the next request to that endpoint asks again.
`)
}
