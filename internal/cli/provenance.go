// Package cli provenance command: read-only dump of every effective
// allow/deny entry across network, filesystem, environment, and skills
// subsystems, each row annotated with the config layer it came from.
//
// omac provenance [--profile <ref>] [--json]
//
// Reuses existing loaders (sandboxprofile.Resolve, netprompt.LoadLearnedPolicy,
// registry.Load, PlatformBaseline, EffectiveProtectedPaths) — no new
// resolution logic, just a presentation layer over what the sandbox
// actually enforces.
package cli

// provEntry is one row in any provenance section.
type provEntry struct {
	Entry  string `json:"entry"`
	Action string `json:"action"`
	Source string `json:"source"`
}

// profileSource identifies which profile was resolved and where it came from.
type profileSource struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Source string `json:"source"`
}

// networkView holds the effective network policy + entries.
type networkView struct {
	Mode          string      `json:"mode"`
	PromptOn      bool        `json:"prompt_enabled"`
	OnUnavailable string      `json:"on_unavailable"`
	Entries       []provEntry `json:"entries"`
}

// filesystemView holds the effective filesystem policy + entries.
type filesystemView struct {
	WorkdirAccess string      `json:"workdir_access"`
	Entries       []provEntry `json:"entries"`
}

// environmentView holds env-var allow/deny entries.
type environmentView struct {
	Entries []provEntry `json:"entries"`
}

// skillsView holds the registered-skill entries.
type skillsView struct {
	Workdir string      `json:"workdir"`
	Entries []provEntry `json:"entries"`
}

// provenanceView is the top-level payload. JSON mode marshals this
// directly; text mode walks each section.
type provenanceView struct {
	Profile     profileSource   `json:"profile"`
	Network     networkView     `json:"network"`
	Filesystem  filesystemView  `json:"filesystem"`
	Environment environmentView `json:"environment"`
	Skills      skillsView      `json:"skills"`
}
