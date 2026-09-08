package buildmanifest

import (
	"errors"
	"strings"
	"testing"
)

func TestMissingCapabilityError_RenderMatchesSpec(t *testing.T) {
	// spec.md:236-242 example:
	//   OMAC build denied container image postgres:17.
	//   Add the image to .omac/build.yaml, then restart OMAC to review and activate
	//   the changed capability set. The current session policy is frozen; do not retry.
	e := &MissingCapabilityError{
		Kind:         "container image",
		Name:         "postgres:17",
		ManifestPath: ".omac/build.yaml",
	}
	out := e.Render()
	for _, want := range []string{
		"OMAC build denied container image postgres:17",
		"Add the container image to .omac/build.yaml",
		"restart OMAC",
		"current session policy is frozen; do not retry",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

// TestMissingCapabilityError_ProposedChangeEmitted asserts the diagnostic
// emits the concrete proposed non-secret manifest snippet when set
// (spec.md:234: the diagnostic names "the proposed non-secret manifest
// change", not just a generic "add the image" hint).
func TestMissingCapabilityError_ProposedChangeEmitted(t *testing.T) {
	e := &MissingCapabilityError{
		Kind:           "container image",
		Name:           "postgres:17",
		ManifestPath:   ".omac/build.yaml",
		ProposedChange: "Add `postgres:17` under builds[].containers.images in .omac/build.yaml",
	}
	out := e.Render()
	if !strings.Contains(out, e.ProposedChange) {
		t.Errorf("render must emit the concrete ProposedChange verbatim:\n%s", out)
	}
}

func TestMissingCapabilityError_Is(t *testing.T) {
	e := &MissingCapabilityError{Kind: "registry", Name: "internal"}
	var target *MissingCapabilityError
	if !errors.As(e, &target) {
		t.Error("errors.As should match MissingCapabilityError")
	}
}

func TestHostForbiddenError_RenderMatchesSpec(t *testing.T) {
	// spec.md:247-252 example:
	//   OMAC rejected host bind mount /Users/me/.ssh.
	//   Host bind mounts are forbidden by host policy and cannot be enabled through
	//   .omac/build.yaml.
	e := &HostForbiddenError{Field: "builds[0].containers.bindMounts[0]", Kind: "bindMounts"}
	out := e.Render()
	for _, want := range []string{
		"OMAC rejected",
		"host bind mount",
		"forbidden by host policy",
		"cannot be enabled through",
		".omac/build.yaml",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestHostForbiddenError_Privileged(t *testing.T) {
	e := &HostForbiddenError{Kind: "privileged"}
	out := e.Render()
	if !strings.Contains(out, "privileged mode") {
		t.Errorf("privileged should render as 'privileged mode': %s", out)
	}
}
