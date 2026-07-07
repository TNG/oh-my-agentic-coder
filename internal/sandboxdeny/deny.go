// Package sandboxdeny holds the configurable text returned to an agent
// when it touches a sandbox-protected path. The text is intentionally
// neutral and deterrent: it states the path is intentionally restricted,
// not missing and not a bug, and directs the agent to escalate only
// when the task genuinely cannot proceed otherwise.
//
// All user-facing strings live here so they form a single tunable knob.
package sandboxdeny

// Text holds the configurable denial messages.
type Text struct {
	// MarkerFile is written into the sandbox over a protected file (Linux
	// bwrap fast path). The first line is a machine-parseable sentinel.
	MarkerFile string `yaml:"marker_file" json:"marker_file"`

	// MarkerDir is the name of the placeholder file placed inside a
	// protected directory masked with a tmpfs. Empty disables dir-level
	// markers (agent sees an empty directory).
	MarkerDirName string `yaml:"marker_dir_name" json:"marker_dir_name"`

	// FacadeNote is the "note" field in the JSON body returned by the
	// facade GET /sandbox/denied endpoint.
	FacadeNote string `yaml:"facade_note" json:"facade_note"`
}

// Default returns the compiled-in denial text. Neutral wording: never
// classifies the protected resource as a "secret" or "credential" —
// only states it is intentionally restricted.
func Default() Text {
	return Text{
		MarkerFile: `X-Omac-Sandbox: denied
This path is protected by the omac sandbox policy and is intentionally
restricted. It is not missing and not a bug. Do not request access unless
the task explicitly requires this path and cannot proceed otherwise.

If you need this path for your task, declare why first:
  POST $OMAC_BASE/sandbox/intent  {"target":"<absolute path>","reason":"..."}
The user will see your reason when reviewing access.
`,
		MarkerDirName: ".omac-denied",
		FacadeNote: "Intentionally restricted by sandbox policy. Not missing, not a bug. " +
			"Escalate to the user only if the task cannot proceed without this path. " +
			"To declare why you need a path, POST $OMAC_BASE/sandbox/intent " +
			`{"target":"<absolute path>","reason":"..."}` +
			" — the user sees your reason when reviewing access.",
	}
}

// Resolve returns override when it has non-empty MarkerFile content,
// otherwise the Default. This mirrors sandboxbrief.Resolve precedence.
func Resolve(override Text) Text {
	d := Default()
	if override.MarkerFile != "" {
		d.MarkerFile = override.MarkerFile
	}
	if override.MarkerDirName != "" {
		d.MarkerDirName = override.MarkerDirName
	}
	if override.FacadeNote != "" {
		d.FacadeNote = override.FacadeNote
	}
	return d
}
