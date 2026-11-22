package mcpinject

import (
	"errors"
	"fmt"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

const refusalExplanation = `An MCP entry in the harness config runs inside the sandbox or keeps its login in the harness's state directory, where the agent can reach it.
Register the server with "omac mcp register" so its credentials stay on the host, remove the entry, or run once with --mcp-unregistered=allow.
If an entry already holds a token, rotate it.`

const unsupportedHarnessWays = `Use a supported harness, or run once with --mcp-unregistered=warn (start without the registered servers) or --mcp-unregistered=allow.`

const unsupportedEntriesExplanation = `An MCP entry in the harness config runs inside the sandbox or keeps its login in the harness's state directory, where the agent can reach it.
Remove the entry, or run once with --mcp-unregistered=allow. If an entry already holds a token, rotate it.`

const managedNotice = "\n    (administrator-managed file: you cannot remove it; start with --mcp-unregistered=allow to accept it)"

const argumentsNotice = `Check the copied arguments for tokens before you run the "omac mcp add" commands above.`

// UnsupportedHarnessMessage says that omac cannot hand registered servers to
// harness. The supported harnesses come from the harness descriptors.
func UnsupportedHarnessMessage(harness string) string {
	return fmt.Sprintf("omac cannot provide MCP servers to %s yet (supported: %s)", harness, strings.Join(config.MCPCapableHarnessNames(), ", "))
}

// Evaluate applies the policy to what Scan found. Every harness entry is
// unregistered from the harness's point of view, including one named like a
// registered server.
func Evaluate(p Policy, ex []Existing, f []Finding) (warnings []string, refusal error) {
	if !p.Active || p.Unregistered == ModeAllow {
		return nil, nil
	}
	problems, hinted := scanProblems(ex, f)
	if hinted {
		problems = append(problems, argumentsNotice)
	}
	if p.Unregistered == ModeWarn {
		return problems, nil
	}
	if len(problems) > 0 {
		return nil, errors.New("MCP policy refuses this start:\n  " + strings.Join(problems, "\n  ") + "\n" + refusalExplanation)
	}
	return nil, nil
}

// EvaluateUnsupported applies the policy to a start of a harness omac cannot
// provide MCP servers to. Registered servers are the names of the registrations
// that will not be available; ex and f are what Scan found in the harness's own
// config.
func EvaluateUnsupported(p Policy, harness string, registered []string, ex []Existing, f []Finding) (warnings []string, refusal error) {
	if !p.Active || p.Unregistered == ModeAllow {
		return nil, nil
	}
	var problems []string
	if len(registered) > 0 {
		problems = append(problems, UnsupportedHarnessMessage(harness)+". Registered servers that will not be available: "+strings.Join(registered, ", ")+".")
	}
	found, hinted := scanProblems(ex, f)
	problems = append(problems, found...)
	if hinted {
		problems = append(problems, argumentsNotice)
	}
	if p.Unregistered == ModeWarn {
		return problems, nil
	}
	if len(problems) == 0 {
		return nil, nil
	}
	explanation := unsupportedHarnessWays
	if len(found) > 0 {
		explanation += "\n" + unsupportedEntriesExplanation
	}
	return nil, errors.New("MCP policy refuses this start:\n  " + strings.Join(problems, "\n  ") + "\n" + explanation)
}

func scanProblems(ex []Existing, f []Finding) (problems []string, hinted bool) {
	for _, e := range ex {
		problem := fmt.Sprintf("unregistered MCP server %q in %s", e.Name, e.File)
		if e.AlsoRegistered {
			problem = fmt.Sprintf("%q in %s (also registered with omac; the harness entry still runs inside the sandbox)", e.Name, e.File)
		}
		if e.PluginProvided {
			problem += " (provided by a plugin)"
		}
		if e.Managed {
			problem += managedNotice
		}
		if hint := AddCommand(e); hint != "" {
			hinted = hinted || strings.HasPrefix(hint, "omac ")
			problem += "\n    " + hint
		}
		problems = append(problems, problem)
	}
	for _, finding := range f {
		verb := "read"
		if finding.Kind == FindingParseError {
			verb = "parse"
		}
		problem := fmt.Sprintf("cannot %s MCP config %s (%s)", verb, finding.File, finding.Detail)
		if finding.PluginProvided {
			problem += " (provided by a plugin)"
		}
		if finding.Managed {
			problem += managedNotice
		}
		problems = append(problems, problem)
	}
	return problems, hinted
}
