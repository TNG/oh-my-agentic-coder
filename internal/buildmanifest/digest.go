package buildmanifest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Digest computes a SHA-256 digest over a canonical form of the parsed
// manifest. Canonicalization re-marshals the manifest to YAML with
// yaml.v3's stable map-key ordering, so the digest is DETERMINISTIC: the
// same manifest content yields the same digest regardless of source file
// key order, whitespace, or comments. A zero/empty manifest has a stable
// digest distinct from any non-empty manifest (so "no manifest" vs "empty
// manifest" are distinguishable from "changed manifest").
//
// The digest is the approval key: OMAC stores an approval record keyed by
// digest + effective capability set; a changed digest triggers a
// consolidated review.
func Digest(m *Manifest) string {
	if m == nil {
		m = &Manifest{}
	}
	// Marshal with yaml.v3 (which sorts map keys for stable output). The
	// zero-value fields are omitted by `yaml:"..."` omitempty semantics we
	// add below via a canonical encoding helper. yaml.v3 Marshal emits maps
	// in sorted key order and uses a fixed indentation, so the output is
	// deterministic for structurally-equal inputs.
	data, err := yaml.Marshal(canonicalManifest(m))
	if err != nil {
		// Marshaling a *Manifest cannot fail in practice; fail to a
		// distinct digest so a bug never silently matches an approval.
		return fmt.Sprintf("err:%v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// canonicalManifest returns a generic-map form of the manifest with nil
// slices/maps dropped, so a zero-value field and an empty field produce the
// same digest (they are semantically equal). This is what makes the digest
// stable across "omitempty" drift.
func canonicalManifest(m *Manifest) any {
	out := map[string]any{}
	if m.Version != 0 {
		out["version"] = m.Version
	}
	if len(m.Builds) > 0 {
		builds := make([]any, 0, len(m.Builds))
		for _, b := range m.Builds {
			entry := map[string]any{}
			if b.Root != "" {
				entry["root"] = b.Root
			}
			if b.Tool != "" {
				entry["tool"] = b.Tool
			}
			if b.Containers != nil && len(b.Containers.Images) > 0 {
				imgs := make([]any, 0, len(b.Containers.Images))
				for _, img := range b.Containers.Images {
					imgs = append(imgs, img)
				}
				entry["containers"] = map[string]any{"images": imgs}
			}
			builds = append(builds, entry)
		}
		out["builds"] = builds
	}
	if len(m.Registries) > 0 {
		regs := make([]any, 0, len(m.Registries))
		for _, r := range m.Registries {
			entry := map[string]any{}
			if r.Alias != "" {
				entry["alias"] = r.Alias
			}
			if r.Upstream != "" {
				entry["upstream"] = r.Upstream
			}
			regs = append(regs, entry)
		}
		out["registries"] = regs
	}
	if m.Resources != nil {
		res := map[string]any{}
		if m.Resources.MaxHeap != "" {
			res["maxHeap"] = m.Resources.MaxHeap
		}
		if m.Resources.MaxDuration > 0 {
			res["maxDuration"] = m.Resources.MaxDuration.String()
		}
		if len(res) > 0 {
			out["resources"] = res
		}
	}
	return out
}

// CapabilitySet is the post-ceiling effective capability set derived from
// a parsed manifest. "Post-ceiling" means each manifest request is
// intersected with HostPolicy: a request above the ceiling was already
// rejected by Validate, so CapabilitySet narrows each request to the host
// default when the request is zero, and keeps the request otherwise. The
// approval record stores the digest AND this effective set, so a later
// host-ceiling drop below what was approved forces re-approval.
type CapabilitySet struct {
	// BuildRoots is the set of declared build roots (relative paths).
	BuildRoots []string
	// Images is the union of approved image references across all builds.
	Images []string
	// Registries is the set of approved registry aliases.
	Registries []string
	// Resources is the effective (post-ceiling) resource set. A zero field
	// means "host default applies"; a non-zero field is the narrowed
	// request (already validated to be <= ceiling).
	Resources ResourceRequests
	// HostPolicy is a snapshot of the host policy the capability set was
	// intersected against. If the host ceiling later drops below this, the
	// stored capability set is no longer valid → re-approval forced.
	HostPolicy HostPolicy
}

// CapabilitySet computes the post-ceiling effective capability set for a
// parsed manifest. Validate(host) MUST have been called first (this function
// assumes the request is within the ceiling; it does not re-check).
func (m *Manifest) CapabilitySet(host HostPolicy) CapabilitySet {
	cs := CapabilitySet{HostPolicy: host}
	if m == nil {
		return cs
	}
	for _, b := range m.Builds {
		if b.Root != "" {
			cs.BuildRoots = append(cs.BuildRoots, b.Root)
		}
		if b.Containers != nil {
			cs.Images = append(cs.Images, b.Containers.Images...)
		}
	}
	for _, r := range m.Registries {
		if r.Alias != "" {
			cs.Registries = append(cs.Registries, r.Alias)
		}
	}
	if m.Resources != nil {
		cs.Resources = *m.Resources
	} else {
		cs.Resources = ResourceRequests{}
	}
	return cs
}

// HasImage reports whether an image reference is in the approved set.
func (cs CapabilitySet) HasImage(img string) bool {
	for _, i := range cs.Images {
		if i == img {
			return true
		}
	}
	return false
}

// HasRegistry reports whether a registry alias is in the approved set.
func (cs CapabilitySet) HasRegistry(alias string) bool {
	for _, r := range cs.Registries {
		if r == alias {
			return true
		}
	}
	return false
}

// HasBuildRoot reports whether a build root is in the approved set.
func (cs CapabilitySet) HasBuildRoot(root string) bool {
	for _, r := range cs.BuildRoots {
		if r == root {
			return true
		}
	}
	return false
}

// Equal reports whether two capability sets are structurally equal (used to
// decide whether the stored approval's effective set still matches the
// current host-intersected set; a mismatch forces re-approval).
func (cs CapabilitySet) Equal(other CapabilitySet) bool {
	if !sliceEqual(cs.BuildRoots, other.BuildRoots) {
		return false
	}
	if !sliceEqual(cs.Images, other.Images) {
		return false
	}
	if !sliceEqual(cs.Registries, other.Registries) {
		return false
	}
	if cs.Resources != other.Resources {
		return false
	}
	if cs.HostPolicy != other.HostPolicy {
		return false
	}
	return true
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
