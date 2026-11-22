package mcpinject

import (
	"errors"
	"fmt"
	"strings"
)

// Evaluate applies the policy to what Scan found. Entries named like a
// registered server are shadowed by the injected one and never offend.
func Evaluate(p Policy, ex []Existing, f []Finding, registered map[string]bool) (warnings []string, refusal error) {
	if !p.Active {
		return nil, nil
	}
	var refusals []string
	report := func(refuse bool, msg string) {
		if refuse {
			refusals = append(refusals, msg)
		} else {
			warnings = append(warnings, msg)
		}
	}

	for _, e := range ex {
		if registered[e.Name] {
			continue
		}
		msg := fmt.Sprintf("unregistered MCP server %q in %s", e.Name, e.File)
		switch p.Unregistered {
		case ModeWarn:
			report(false, msg)
		case ModeRefuse:
			report(true, msg)
		}
	}

	for _, finding := range f {
		switch finding.Kind {
		case FindingParseError:
			failClosed := p.Unregistered == ModeRefuse || p.LiteralCredentials == CredRefuse
			quiet := p.Unregistered == ModeAllow && p.LiteralCredentials == CredAllow
			if !quiet {
				report(failClosed, fmt.Sprintf("cannot parse MCP config %s (%s)", finding.File, finding.Field))
			}
		case FindingLiteralCredential:
			if p.LiteralCredentials == CredAllow {
				continue
			}
			report(p.LiteralCredentials == CredRefuse,
				fmt.Sprintf("literal credential in %s: server %q, %s", finding.File, finding.Server, finding.Field))
		}
	}

	if len(refusals) > 0 {
		return warnings, errors.New("MCP policy refuses this start:\n  " + strings.Join(refusals, "\n  "))
	}
	return warnings, nil
}
