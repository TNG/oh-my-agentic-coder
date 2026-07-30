package containerproxy

import (
	"fmt"
	"strings"
)

// PolicyErrKind classifies a ContainerPolicyError so the diagnostic's fix
// hint is exact rather than substring-derived. Mirrors the discipline of
// credproxy.CredentialErrKind / buildmanifest.HostForbiddenError.
type PolicyErrKind int

const (
	// KindUnapprovedImage: the create body or images/{ref}/json named an
	// image reference that is not in the frozen-for-session approved
	// manifest image set.
	KindUnapprovedImage PolicyErrKind = iota
	// KindPrivilegedForbidden: HostConfig.Privileged was true. Forbidden by
	// host policy; cannot be enabled through the manifest.
	KindPrivilegedForbidden
	// KindBindMountForbidden: HostConfig.Binds or .Mounts was non-empty.
	// Forbidden by host policy (no host bind mount in any accepted
	// workflow; the docker.sock bind is a Ryuk pattern v1 eliminates).
	KindBindMountForbidden
	// KindHostNamespaceForbidden: HostConfig.NetworkMode/.PidMode/.IpcMode
	// /.UsernsMode/.CgroupnsMode/.Runtime was non-default. Forbidden.
	KindHostNamespaceForbidden
	// KindDeviceForbidden: HostConfig.CapAdd/.Devices/.SecurityOpt/.Dns
	// /.ExtraHosts/.CgroupParent was non-empty. Forbidden.
	KindDeviceForbidden
	// KindUnknownEndpoint: the request path/method is not in the ticket-02
	// measured v1 allowlist. Fail-closed.
	KindUnknownEndpoint
	// KindNotOwnedByExecutor: a follow-up op targeted a container that
	// does not carry this executor's ownership label. One executor cannot
	// inspect, modify, or remove another executor's resources.
	KindNotOwnedByExecutor
	// KindRyukForbidden: the image is testcontainers/ryuk (or the create
	// body matches the Ryuk socket-nesting pattern). v1 disables Ryuk via
	// TESTCONTAINERS_RYUK_DISABLED=true; the filter rejects it fail-closed
	// (a client could unset the env).
	KindRyukForbidden
	// KindRegistryAuthForbidden: an /images/create request carried an
	// X-Registry-Auth header. Private registry auth is deliberately
	// untested in v1 (credential-lift territory, issue #92); the v1 filter
	// denies the header.
	KindRegistryAuthForbidden
	// KindReservedLabel: the create body attempted to set a reserved
	// omac.* ownership label. Client-controlled labels are forgeable and
	// must not override ownership.
	KindReservedLabel
)

// ContainerPolicyError is a structured diagnostic for a Docker-API request
// the mediated container proxy denied. It names the kind, a human reason,
// the container id / image involved (never credential values), and renders
// spec-exact text (spec.md:230-256 — "correlate low-level network and
// container denials with the active build request"). Mirrors
// buildmanifest.MissingCapabilityError / credproxy.RegistryCredentialError.
//
// The error is returned to the caller (for audit) AND rendered as a JSON
// Docker-API-style error response to the client (see proxy.go denyJSON) so
// Testcontainers/Gradle wrapping does not hide the OMAC cause.
type ContainerPolicyError struct {
	Kind        PolicyErrKind
	Reason      string
	ContainerID string
	Image       string
}

func (e *ContainerPolicyError) Error() string { return e.Render() }

// Render produces the spec-exact diagnostic text. The wording distinguishes
// a host-forbidden capability (cannot be enabled through the manifest) from
// a requestable capability (image not approved — add to .omac/build.yaml).
func (e *ContainerPolicyError) Render() string {
	var b strings.Builder
	switch e.Kind {
	case KindUnapprovedImage:
		fmt.Fprintf(&b, "OMAC build denied container image %s.\n", e.Image)
		fmt.Fprintf(&b, "Add the image to .omac/build.yaml, then restart OMAC to review and activate\n")
		fmt.Fprintf(&b, "the changed capability set. The current session policy is frozen; do not retry.")
	case KindPrivilegedForbidden:
		fmt.Fprintf(&b, "OMAC rejected privileged mode for container %s.\n", e.ident())
		fmt.Fprintf(&b, "Privileged mode is forbidden by host policy and cannot be enabled through\n.omac/build.yaml.")
	case KindBindMountForbidden:
		fmt.Fprintf(&b, "OMAC rejected host bind mount for container %s.\n", e.ident())
		fmt.Fprintf(&b, "Host bind mounts are forbidden by host policy and cannot be enabled through\n.omac/build.yaml.")
	case KindHostNamespaceForbidden:
		fmt.Fprintf(&b, "OMAC rejected host namespace for container %s.\n", e.ident())
		if e.Reason != "" {
			fmt.Fprintf(&b, "Rejected field: %s\n", e.Reason)
		}
		fmt.Fprintf(&b, "Host namespaces are forbidden by host policy and cannot be enabled through\n.omac/build.yaml.")
	case KindDeviceForbidden:
		fmt.Fprintf(&b, "OMAC rejected devices/capabilities/security options for container %s.\n", e.ident())
		if e.Reason != "" {
			fmt.Fprintf(&b, "Rejected field: %s\n", e.Reason)
		}
		fmt.Fprintf(&b, "Devices, extra capabilities, and unsafe security options are forbidden by host\npolicy and cannot be enabled through .omac/build.yaml.")
	case KindUnknownEndpoint:
		fmt.Fprintf(&b, "OMAC denied unknown Docker API endpoint.\n")
		fmt.Fprintf(&b, "%s\n", e.Reason)
		fmt.Fprintf(&b, "The v1 container proxy implements only the measured Testcontainers allowlist;\nunknown endpoints are denied fail-closed.")
	case KindNotOwnedByExecutor:
		fmt.Fprintf(&b, "OMAC denied access to container %s.\n", e.ident())
		fmt.Fprintf(&b, "The container is not owned by this executor; one executor cannot inspect,\nmodify, or remove another executor's resources.")
	case KindRyukForbidden:
		fmt.Fprintf(&b, "OMAC rejected Testcontainers Ryuk container.\n")
		fmt.Fprintf(&b, "Ryuk and socket nesting are unsupported in v1 (TESTCONTAINERS_RYUK_DISABLED=true\nis injected); the filter rejects them fail-closed.")
	case KindRegistryAuthForbidden:
		fmt.Fprintf(&b, "OMAC rejected X-Registry-Auth on image pull.\n")
		fmt.Fprintf(&b, "Private registry credential lift is not supported in v1 (issue #92); the filter\ndenies the X-Registry-Auth header.")
	case KindReservedLabel:
		fmt.Fprintf(&b, "OMAC rejected reserved omac.* label in container create.\n")
		fmt.Fprintf(&b, "Client-controlled labels are forgeable and must not override executor ownership;\nthe omac.* label prefix is reserved.")
	default:
		fmt.Fprintf(&b, "OMAC denied container request: %s", e.Reason)
	}
	return b.String()
}

// ident returns the container id or a placeholder for diagnostics.
func (e *ContainerPolicyError) ident() string {
	if e.ContainerID != "" {
		return e.ContainerID
	}
	if e.Image != "" {
		return e.Image
	}
	return "<unknown>"
}
