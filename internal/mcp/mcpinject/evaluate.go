package mcpinject

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/displaytext"
)

const refusalExposure = `An MCP entry in the harness config runs inside the sandbox or keeps its login in the harness's state directory, where the agent can reach it.`

const refusalRotate = `If an entry already holds a token, rotate it.`

const ignoredSuffix = " is ignored by strict mode"

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
	ignored := p.Unregistered == ModeIgnore
	problems, hinted := scanProblems(ex, f, ignored)
	if hinted {
		problems = append(problems, argumentsNotice)
	}
	if p.Unregistered == ModeWarn || ignored {
		return problems, nil
	}
	if len(problems) > 0 {
		return nil, errors.New("MCP policy refuses this start:\n  " + strings.Join(problems, "\n  ") + "\n" + refusalExplanation(ex, f))
	}
	return nil, nil
}

// refusalExplanation says what the user can do about refused entries. Remote
// entries cannot be registered yet, so they get no registration advice.
func refusalExplanation(ex []Existing, f []Finding) string {
	registerable := len(f) > 0
	remote := false
	for _, e := range ex {
		if isRemote(e) {
			remote = true
		} else {
			registerable = true
		}
	}
	var ways string
	switch {
	case !registerable:
		ways = `Remote MCP servers are not supported yet, so they cannot be registered. Remove the entry, or run once with --mcp-unregistered=allow.`
	case remote:
		ways = `Register the server with "omac mcp register" so its credentials stay on the host, remove the entry, or run once with --mcp-unregistered=allow. Remote MCP servers are not supported yet, so they cannot be registered.`
	default:
		ways = `Register the server with "omac mcp register" so its credentials stay on the host, remove the entry, or run once with --mcp-unregistered=allow.`
	}
	return refusalExposure + "\n" + ways + "\n" + refusalRotate
}

func isRemote(e Existing) bool { return e.Transport != "" && e.Transport != "stdio" }

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
	found, hinted := scanProblems(ex, f, false)
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

// scanProblems lists the entries and unreadable files. With ignored set the
// harness does not load them, so the lines say so and the hints that only apply
// to loaded or refused entries are left out.
func scanProblems(ex []Existing, f []Finding, ignored bool) (problems []string, hinted bool) {
	for _, e := range ex {
		file := shown(e.File)
		problem := fmt.Sprintf("unregistered MCP server %q in %s", e.Name, file)
		if e.AlsoRegistered {
			problem = fmt.Sprintf("%q in %s (also registered with omac; the harness entry still runs inside the sandbox)", e.Name, file)
			if ignored {
				problem = fmt.Sprintf("%q in %s (also registered with omac)", e.Name, file)
			}
		}
		if e.PluginProvided {
			problem += " (provided by a plugin)"
		}
		if ignored {
			problem += ignoredSuffix
		} else if e.Managed {
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
		problem := fmt.Sprintf("cannot %s MCP config %s (%s)", verb, shown(finding.File), shown(finding.Detail))
		if finding.PluginProvided {
			problem += " (provided by a plugin)"
		}
		if ignored {
			problem += "; anything it defines" + ignoredSuffix
		} else if finding.Managed {
			problem += managedNotice
		}
		problems = append(problems, problem)
	}
	return problems, hinted
}

// UnloadedNames lists, without repeats, the server names a scan found and the
// files it could not read or parse, which may hide servers. Values never appear.
func UnloadedNames(ex []Existing, f []Finding) []string {
	var names []string
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, e := range ex {
		add(shown(e.Name))
	}
	for _, finding := range f {
		add(shown(finding.File))
	}
	return names
}

// shown returns s for the terminal: as it is, or quoted with escapes when it
// holds a character that could change how the line displays. Server names,
// file names and parser messages come from files the agent may have written.
func shown(s string) string {
	if strings.IndexFunc(s, displaytext.Unsafe) < 0 {
		return s
	}
	return strconv.Quote(s)
}
