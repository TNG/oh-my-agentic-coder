// Package profileaudit statically lints a resolved sandbox profile against
// known secret locations and network attack vectors, producing a list of
// findings ranked by severity. It is the engine behind
// `omac provenance --check`.
package profileaudit

// Severity ranks the risk of a finding.
type Severity string

const (
	SeverityHigh   Severity = "high"
	SeverityMedium Severity = "medium"
	SeverityLow    Severity = "low"
)

// Category groups findings by subsystem.
type Category string

const (
	CatFSGrant      Category = "filesystem"
	CatNetwork      Category = "network"
	CatOverrideDeny Category = "override_deny"
)

// Finding is one static-check result.
type Finding struct {
	Severity Severity `json:"severity"`
	Category Category `json:"category"`
	Field    string   `json:"field"`
	Value    string   `json:"value"`
	Message  string   `json:"message"`
}

// ExitCode returns 2 if any finding is HIGH, else 0. The value 2 is a
// non-zero exit signaling a config-level problem; the CLI layer forwards
// it as the process exit code. (The omac cli package defines
// ExitMisuse=2 and ExitConfigInvalid=3; profileaudit avoids importing
// cli to prevent an import cycle, so the literal is intentional.)
func ExitCode(findings []Finding) int {
	for _, f := range findings {
		if f.Severity == SeverityHigh {
			return 2
		}
	}
	return 0
}

// severityRank returns a sort key for a Severity (lower = more severe).
func severityRank(s Severity) int {
	switch s {
	case SeverityHigh:
		return 0
	case SeverityMedium:
		return 1
	case SeverityLow:
		return 2
	default:
		return 3
	}
}
