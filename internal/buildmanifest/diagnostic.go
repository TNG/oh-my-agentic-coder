package buildmanifest

import (
	"fmt"
	"strings"
)

// MissingCapabilityError is a structured diagnostic for a build that requests
// a capability (image, registry, build root) not in the active approved
// manifest. It names the resource, the manifest path, the proposed non-secret
// manifest change, the restart requirement, and the fact that retrying in the
// current frozen session cannot succeed. The CLI maps it to ExitPolicyDenied.
//
// Matches spec.md:236-242:
//
//	OMAC build denied container image postgres:17.
//	Add the image to .omac/build.yaml, then restart OMAC to review and activate
//	the changed capability set. The current session policy is frozen; do not retry.
type MissingCapabilityError struct {
	// Kind is the capability kind ("container image", "registry", "build root").
	Kind string
	// Name is the specific resource name (e.g. "postgres:17").
	Name string
	// ManifestPath is the worktree-relative manifest path (".omac/build.yaml").
	ManifestPath string
	// ProposedChange is the non-secret manifest snippet to add (rendered).
	ProposedChange string
}

func (e *MissingCapabilityError) Error() string { return e.Render() }

// Render produces the spec-exact diagnostic text. It names the resource,
// the manifest path, the proposed non-secret manifest change, the restart
// requirement, and "current session policy is frozen; do not retry"
// (spec.md:234). The wording fragments are asserted in tests.
func (e *MissingCapabilityError) Render() string {
	proposed := e.ProposedChange
	if proposed == "" {
		// Fall back to a generic hint when no concrete snippet was supplied
		// (kept so a future caller that forgets ProposedChange still gets a
		// spec-shaped message, but callers SHOULD set it).
		proposed = fmt.Sprintf("Add the %s to %s", e.Kind, e.ManifestPath)
	}
	return fmt.Sprintf(
		"OMAC build denied %s %s.\n"+
			"%s, then restart OMAC to review and activate\n"+
			"the changed capability set. The current session policy is frozen; do not retry.",
		e.Kind, e.Name, proposed,
	)
}

// HostForbiddenError is a structured diagnostic for a capability the host
// FORBIDS — one project configuration cannot enable through the manifest
// (host bind mounts, privileged mode, raw sockets, host namespaces). The v1
// manifest schema simply does not include those fields, but the validator
// rejects any forbidden-shape field with this error.
//
// Matches spec.md:247-252:
//
//	OMAC rejected host bind mount /Users/me/.ssh.
//	Host bind mounts are forbidden by host policy and cannot be enabled through
//	.omac/build.yaml.
type HostForbiddenError struct {
	// Field is the dotted path to the offending manifest field.
	Field string
	// Kind is the forbidden capability kind ("bindMounts", "privileged", ...).
	Kind string
}

func (e *HostForbiddenError) Error() string { return e.Render() }

// Render produces the spec-exact diagnostic text. The wording fragments
// ("Host ... are forbidden by host policy", "cannot be enabled through
// .omac/build.yaml") are asserted in tests.
func (e *HostForbiddenError) Render() string {
	kind := humanizeKind(e.Kind)
	return fmt.Sprintf(
		"OMAC rejected %s %s.\n"+
			"%s are forbidden by host policy and cannot be enabled through\n"+
			".omac/build.yaml.",
		kind, e.Field, capFirst(kind),
	)
}

// humanizeKind turns a manifest field name into a human capability label:
// "bindMounts" → "host bind mount", "privileged" → "privileged mode", etc.
func humanizeKind(kind string) string {
	switch strings.ToLower(kind) {
	case "bindmount", "bindmounts":
		return "host bind mount"
	case "privileged":
		return "privileged mode"
	case "rawsocket":
		return "raw socket"
	case "hostnetwork":
		return "host network"
	case "hostpid":
		return "host PID namespace"
	case "hostipc":
		return "host IPC namespace"
	case "devices":
		return "host devices"
	}
	return kind
}

// capFirst returns s with its first rune upper-cased.
func capFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}
