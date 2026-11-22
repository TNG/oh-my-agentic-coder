package audit

import "unicode/utf8"

// Constructors for the typed events. Emit points call these to build an
// Event with the right Type and fields set, keeping call sites terse and
// making the sensitive-field contract explicit (secret NAMES, never
// values; namespaces are hashed downstream by the redactor).

// SessionStart builds a session.start event.
func SessionStart(version, harness, sandboxProfile, sandboxBackend string) Event {
	return Event{
		Type:           TypeSessionStart,
		Version:        version,
		Harness:        harness,
		SandboxProfile: sandboxProfile,
		SandboxBackend: sandboxBackend,
	}
}

// SessionStop builds a session.stop event.
func SessionStop(exitCode int) Event {
	return Event{Type: TypeSessionStop, ExitCode: iptr(exitCode)}
}

// ProcessExec builds a process.exec event. secretNames/configNames are
// env-var names only.
func ProcessExec(skill, namespace, cwd string, argv, secretNames, configNames []string) Event {
	return Event{
		Type:        TypeProcessExec,
		Skill:       skill,
		Namespace:   namespace,
		Cwd:         cwd,
		Argv:        argv,
		SecretNames: secretNames,
		ConfigNames: configNames,
	}
}

// InnerExec builds a process.exec event for the sandboxed inner command.
func InnerExec(argv []string, sandboxProfile string, sandboxed bool) Event {
	return Event{
		Type:           TypeProcessExec,
		Argv:           argv,
		SandboxProfile: sandboxProfile,
		Sandboxed:      bptr(sandboxed),
	}
}

// ProcessExit builds a process.exit event.
func ProcessExit(skill, namespace string, exitCode int, durationMS int64) Event {
	return Event{
		Type:       TypeProcessExit,
		Skill:      skill,
		Namespace:  namespace,
		ExitCode:   iptr(exitCode),
		DurationMS: durationMS,
	}
}

// NetDecision builds a net.decision event.
func NetDecision(host string, port int, allow bool, scope, source string, persisted bool) Event {
	return Event{
		Type:      TypeNetDecision,
		Host:      host,
		Port:      port,
		Allow:     bptr(allow),
		Scope:     scope,
		Source:    source,
		Persisted: bptr(persisted),
	}
}

// maxPathBytes caps the agent-controlled path field so a single audit record
// cannot exceed the ~4 KiB threshold that forces two write(2) calls.
const maxPathBytes = 512

// capPath bounds an agent-controlled path to maxPathBytes, cut at a rune
// boundary.
func capPath(path string) string {
	if len(path) <= maxPathBytes {
		return path
	}
	cut := maxPathBytes
	for cut > 0 && !utf8.RuneStart(path[cut]) {
		cut--
	}
	return path[:cut] + "…"
}

// FacadeRequest builds a facade.request event.
func FacadeRequest(method, mount, namespace, path string, status int, bytesOut, durationMS int64) Event {
	path = capPath(path)
	return Event{
		Type:           TypeFacadeRequest,
		Method:         method,
		Mount:          mount,
		Namespace:      namespace,
		Path:           path,
		UpstreamStatus: status,
		BytesOut:       bytesOut,
		DurationMS:     durationMS,
	}
}

// FacadeApprovalInfo is the gate's decision for one request to a skill that
// declares approvals.
type FacadeApprovalInfo struct {
	Skill, Method, Mount, Namespace string
	// Path is the cleaned, decoded path, as in facade.request; for a path
	// the gate refused to clean, the escaped path as received.
	Path string
	// Result is "allow" or "deny".
	Result string
	// Source is why: declared, prompt, session, always, invalid-path,
	// timeout, unavailable, abandoned, deny-all.
	Source string
	// Scope is once|session|always when a user answer was involved.
	Scope           string
	Unlisted        bool
	StrippedHeaders []string
	// Tool, ToolClass and ToolArgs are set for a call to an MCP tool.
	Tool, ToolClass, ToolArgs string
}

// FacadeApproval builds a facade.approval event. The query string is never
// recorded.
func FacadeApproval(i FacadeApprovalInfo) Event {
	return Event{
		Type: TypeFacadeApproval, Skill: i.Skill, Method: i.Method, Mount: i.Mount,
		Namespace: i.Namespace, Path: capPath(i.Path), Result: i.Result, Source: i.Source, Scope: i.Scope,
		Unlisted: i.Unlisted, StrippedHeaders: i.StrippedHeaders,
	}
}

// ControlMutation builds a control.mutation event.
func ControlMutation(action, dir, result string) Event {
	return Event{Type: TypeControlMutation, Action: action, Dir: dir, Result: result}
}

// SecretInject builds a secret.inject event (names only).
func SecretInject(skill, namespace string, secretNames, configNames []string) Event {
	return Event{
		Type:        TypeSecretInject,
		Skill:       skill,
		Namespace:   namespace,
		SecretNames: secretNames,
		ConfigNames: configNames,
	}
}

// RouteStateEvent builds a route.state event for a non-ready route.
func RouteStateEvent(skill, namespace, state, detail string) Event {
	return Event{
		Type:      TypeRouteState,
		Skill:     skill,
		Namespace: namespace,
		State:     state,
		Detail:    detail,
	}
}
