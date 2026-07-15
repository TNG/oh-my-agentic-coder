// Package sandboxdeny holds the configurable text returned to an agent
// when it touches a sandbox-protected path. The text is intentionally
// neutral and deterrent: it states the path is intentionally restricted,
// not missing and not a bug, and directs the agent to escalate only
// when the task genuinely cannot proceed otherwise.
//
// All user-facing strings live here so they form a single tunable knob.
package sandboxdeny

// Text holds the configurable denial messages. It is a programmatic value
// type built by sandboxrun.resolvedDenial from sandboxprofile.Denial; it is
// never (un)marshalled itself, so it carries no struct tags — the config
// surface lives on sandboxprofile.Denial (JSON), not here.
type Text struct {
	// MarkerFile is written into the sandbox over a protected file (Linux
	// bwrap fast path). The first line is a machine-parseable sentinel.
	MarkerFile string

	// MarkerDirName is the name of the placeholder file placed inside a
	// protected directory masked with a tmpfs. Empty inherits the default
	// (Resolve treats an empty override as "keep the default").
	MarkerDirName string

	// FacadeNote is the "note" field in the JSON body returned by the
	// facade GET /sandbox/denied endpoint for a protected path.
	FacadeNote string

	// NotProtectedNote is the "note" field returned by GET /sandbox/denied
	// for a path that is NOT protected — it steers the agent away from
	// concluding the path is missing and explains there is no live popup
	// for filesystem paths (network-only).
	NotProtectedNote string
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
		NotProtectedNote: "Not protected by the sandbox — but this does not confirm the path exists. " +
			"If it is inside your granted directories it is genuinely missing; if it is outside them it is " +
			"simply not mounted into the sandbox and may exist on the host. Do not conclude it is missing. " +
			"A running sandbox cannot mount new folders and there is no live approval popup for a path — " +
			"that popup is network-only. The only way to reach it is for the user to add it to the sandbox " +
			"profile (~/.config/omac/sandbox-profiles/default.json) and relaunch. Declaring intent " +
			"(POST $OMAC_BASE/sandbox/intent) only records your reason for the session-end review; it does " +
			"not grant access and raises no dialog. So tell the user which path you need and why, and ask " +
			"them to add it and relaunch — do not tell them to approve a popup.",
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
	if override.NotProtectedNote != "" {
		d.NotProtectedNote = override.NotProtectedNote
	}
	return d
}
