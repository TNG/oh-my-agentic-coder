package mcpinject

import (
	"errors"
	"fmt"
	"strings"
)

const refusalExplanation = `An MCP entry in the harness config runs inside the sandbox or keeps its login in the harness's state directory, where the agent can reach it.
Register the server with "omac mcp register" so its credentials stay on the host, remove the entry, or run once with --mcp-unregistered=allow.
If an entry already holds a token, rotate it.`

// Evaluate applies the policy to what Scan found. Every harness entry is
// unregistered from the harness's point of view, including one named like a
// registered server.
func Evaluate(p Policy, ex []Existing, f []Finding) (warnings []string, refusal error) {
	if !p.Active || p.Unregistered == ModeAllow {
		return nil, nil
	}
	var problems []string
	for _, e := range ex {
		problem := fmt.Sprintf("unregistered MCP server %q in %s", e.Name, e.File)
		if e.AlsoRegistered {
			problem = fmt.Sprintf("%q in %s (also registered with omac; the harness entry still runs inside the sandbox)", e.Name, e.File)
		}
		problems = append(problems, problem)
	}
	for _, finding := range f {
		problems = append(problems, fmt.Sprintf("cannot parse MCP config %s (%s)", finding.File, finding.Detail))
	}

	if p.Unregistered == ModeWarn {
		return problems, nil
	}
	if len(problems) > 0 {
		return nil, errors.New("MCP policy refuses this start:\n  " + strings.Join(problems, "\n  ") + "\n" + refusalExplanation)
	}
	return nil, nil
}
