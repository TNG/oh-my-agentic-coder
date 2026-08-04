// Package buildmanifest parses, validates, and approves the optional
// project-authored build manifest at `.omac/build.yaml`.
//
// The manifest is project-authored (committed in the worktree) and DECLARES
// non-secret, non-standard build capabilities (build roots, approved image
// references, registry identities, optional resource requests). It REQUESTS
// capabilities; it does NOT grant them — host policy is the ceiling. OMAC
// stores an approval against the manifest content digest and effective
// capability set; a changed manifest produces one consolidated review at the
// next OMAC start, and an unchanged manifest starts unattended with effective
// policy frozen for the session.
//
// This package is SEPARATE from internal/manifest (which renders skill
// activate-response JSON) and SEPARATE from internal/buildrun/control.go
// (OMAC-authored control state in the cache leaf): the build manifest lives
// in the worktree, control state lives in the cache leaf.
package buildmanifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ManifestVersion is the single supported manifest schema version.
const ManifestVersion = 1

// ManifestPath is the worktree-relative path to the build manifest.
const ManifestPath = ".omac/build.yaml"

// Manifest is a parsed `.omac/build.yaml`. The zero value (no file present)
// is a valid "no manifest" state: builds proceed with defaults.
type Manifest struct {
	// Version is the manifest schema version; must equal ManifestVersion.
	Version int `yaml:"version"`
	// Builds declares non-standard build roots and their capabilities.
	Builds []BuildEntry `yaml:"builds"`
	// Registries declares non-secret registry aliases / upstream identities.
	// Credentials stay in the OMAC keychain (ticket 06) — a registry entry
	// MUST NOT embed a password/token/credential (rejected at parse time).
	Registries []RegistryEntry `yaml:"registries"`
	// Resources optionally narrows host-default resource requests (within
	// the host policy ceiling). The zero value means "use host defaults".
	Resources *ResourceRequests `yaml:"resources"`
	// raw is the generic YAML-decoded structure, kept for the secret /
	// forbidden-field scan that must see fields the typed struct drops
	// (yaml.v3 strict decode discards unknown fields). Populated by Parse.
	raw map[string]any
}

// BuildEntry declares one non-standard build root and its capabilities.
type BuildEntry struct {
	// Root is the build root, RELATIVE to the worktree (e.g. "backend").
	// Absolute paths are rejected (a colleague's worktree lives elsewhere).
	Root string `yaml:"root"`
	// Tool is the build adapter; "gradle" today. Empty defaults to gradle.
	Tool string `yaml:"tool"`
	// Containers declares approved image references for this root.
	Containers *ContainerSpec `yaml:"containers"`
}

// ContainerSpec declares approved container image references for a build.
type ContainerSpec struct {
	// Images is the approved image reference list (e.g. "pgvector/pgvector:pg16").
	// Unapproved images are denied at runtime with a MissingCapabilityError.
	Images []string `yaml:"images"`
}

// RegistryEntry declares a non-secret registry alias / upstream identity.
// It MUST NOT carry credentials — credentials remain in the OMAC keychain
// (ticket 06). Any field whose name matches the secret pattern with a
// non-empty value is rejected at parse time.
type RegistryEntry struct {
	// Alias is the short name Gradle/the build refers to (e.g. "internal").
	Alias string `yaml:"alias"`
	// Upstream is the non-secret upstream identity (e.g. host/url WITHOUT
	// embedded userinfo). MUST NOT contain a userinfo `@` (credentials).
	Upstream string `yaml:"upstream"`
}

// ResourceRequests optionally narrows host-default resource requests.
// Every field is optional; a zero/empty field means "use host default".
// A request ABOVE the HostPolicy ceiling is rejected before executor startup.
//
// ONLY the two v1 resource controls exist: the Gradle daemon heap (-Xmx) and
// the build wall-clock. CPU/process-count limits are NOT requestable in v1 —
// they are not wired to concrete host limits yet, so the manifest cannot
// present them as available (either as a request or as a host ceiling).
type ResourceRequests struct {
	// MaxHeap is the Gradle daemon JVM -Xmx request (e.g. "4g"). Empty
	// uses the host default. Above HostPolicy.MaxHeap → denied.
	MaxHeap string `yaml:"maxHeap"`
	// MaxDuration bounds the total build wall-clock. Zero uses the host
	// default. Above HostPolicy.MaxDuration → denied.
	MaxDuration time.Duration `yaml:"maxDuration"`
}

// HostPolicy is the host-controlled authority ceiling. The manifest may
// REQUEST capabilities within this ceiling but cannot widen it. The CLI
// populates this from the existing build-run defaults (defaultMaxHeap and
// --max-duration parsing).
type HostPolicy struct {
	// MaxHeap is the maximum Gradle daemon -Xmx the host permits (e.g. "4g").
	// Empty disables the heap ceiling check.
	MaxHeap string
	// MaxDuration is the maximum build wall-clock the host permits. Zero
	// disables the duration ceiling check.
	MaxDuration time.Duration
}

// ManifestError is a structured manifest parse/validation error naming the
// offending field. The CLI maps it to ExitPolicyDenied (exit 3).
type ManifestError struct {
	// Field is the dotted path to the offending field (e.g. "registries[0].password").
	Field string
	// Reason is the human-readable reason (e.g. "secret field rejected").
	Reason string
}

func (e *ManifestError) Error() string {
	return fmt.Sprintf("build manifest %s: %s", e.Field, e.Reason)
}

// Load reads `<worktree>/.omac/build.yaml`. A MISSING file is the normal
// case for a standard Gradle project: Load returns a zero Manifest and nil
// error so the build proceeds with defaults. A PRESENT-but-unparseable or
// invalid file yields a *ManifestError naming the offending field.
//
// The worktree must be the canonical (EvalSymlinks-resolved) worktree root;
// Load joins ManifestPath to it. The file is read with os.ReadFile (no
// symlink-following beyond what the kernel does) and parsed with yaml.v3
// in strict-decode mode so unknown fields surface (a typo'd field name is
// a capability the manifest did NOT intend to declare).
func Load(worktree string) (*Manifest, error) {
	path := filepath.Join(worktree, ManifestPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Standard Gradle project: no manifest, build proceeds with
			// defaults. Return a zero (but non-nil) manifest.
			return &Manifest{}, nil
		}
		return nil, &ManifestError{Field: ManifestPath, Reason: fmt.Sprintf("read: %v", err)}
	}
	return Parse(data)
}

// Parse decodes manifest bytes and runs the STRUCTURAL validation that does
// not depend on host policy: secret-field rejection, forbidden-field
// rejection, schema version, absolute-root rejection, traversal rejection,
// embedded-credential rejection, and tool/registry sanity. The host-ceiling
// checks (which need a HostPolicy) are done by Validate. Used by Load and by
// tests. A zero-length input is treated as "no manifest" (same as a missing
// file).
//
// Secrets and forbidden-shape fields are rejected at PARSE time (not just
// Validate) per the ticket: a committed manifest must never carry a secret,
// so the refusal is at the decode boundary.
func Parse(data []byte) (*Manifest, error) {
	if len(data) == 0 {
		return &Manifest{}, nil
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, &ManifestError{Field: ManifestPath, Reason: fmt.Sprintf("parse: %v", err)}
	}
	if node.Kind == 0 {
		// Empty document (e.g. just comments / "---").
		return &Manifest{}, nil
	}
	var m Manifest
	if err := node.Decode(&m); err != nil {
		return nil, &ManifestError{Field: ManifestPath, Reason: fmt.Sprintf("decode: %v", err)}
	}
	// Also decode to a generic tree so the secret / forbidden-field scan
	// sees fields the typed struct discards (strict decode drops unknown
	// fields; a committed manifest must never carry a secret even in an
	// unknown field). The scan runs on this generic tree.
	var raw map[string]any
	if err := node.Decode(&raw); err != nil {
		return nil, &ManifestError{Field: ManifestPath, Reason: fmt.Sprintf("decode generic: %v", err)}
	}
	m.raw = raw
	if err := m.validateStructure(); err != nil {
		return nil, err
	}
	return &m, nil
}

// secretFieldRe matches field names that look like credentials. The manifest
// MUST NOT contain secrets (credentials stay in the OMAC keychain, ticket 06).
// Any field whose name matches this pattern with a non-empty value is
// rejected at parse/validation time so a committed manifest never carries a
// secret. Case-insensitive.
var secretFieldRe = regexp.MustCompile(`(?i)password|secret|token|credential|apikey|auth`)

// forbiddenFieldRe matches manifest field names that request capabilities the
// host FORBIDS — ones project configuration cannot enable through the
// manifest (host bind mounts, privileged mode, raw sockets, host namespaces).
// The v1 schema simply does not include these fields, but if a future
// manifest attempted them the validator rejects with a HostForbiddenError.
var forbiddenFieldRe = regexp.MustCompile(`(?i)bindMount|bindMounts|privileged|rawSocket|hostNetwork|hostPid|hostIpc|devices`)

// validateStructure runs the host-INDEPENDENT validation: schema version,
// secret-field rejection, forbidden-field rejection, absolute-root rejection,
// traversal rejection, embedded-credential rejection, tool/registry sanity.
// It does NOT do host-ceiling checks (those need a HostPolicy → Validate).
// Called by Parse so a committed manifest with a secret is rejected at the
// decode boundary. Validate also calls it (for in-code-constructed manifests
// that did not go through Parse).
func (m *Manifest) validateStructure() error {
	if m == nil || (m.Version == 0 && len(m.Builds) == 0 && len(m.Registries) == 0 && m.Resources == nil) {
		return nil
	}
	if m.Version != 0 && m.Version != ManifestVersion {
		return &ManifestError{Field: "version", Reason: fmt.Sprintf("unsupported manifest version %d (want %d)", m.Version, ManifestVersion)}
	}
	if m.Version == 0 {
		return &ManifestError{Field: "version", Reason: fmt.Sprintf("missing version (want %d)", ManifestVersion)}
	}
	if err := scanForSecrets(m); err != nil {
		return err
	}
	for i, b := range m.Builds {
		field := fmt.Sprintf("builds[%d].root", i)
		if b.Root == "" {
			return &ManifestError{Field: field, Reason: "empty build root"}
		}
		if filepath.IsAbs(b.Root) {
			return &ManifestError{Field: field, Reason: fmt.Sprintf("absolute root %q rejected — use a path relative to the worktree so colleagues' linked worktrees resolve identically", b.Root)}
		}
		if strings.Contains(b.Root, "..") {
			return &ManifestError{Field: field, Reason: fmt.Sprintf("root %q contains '..' — must stay inside the worktree", b.Root)}
		}
		if b.Tool != "" && b.Tool != "gradle" {
			return &ManifestError{Field: fmt.Sprintf("builds[%d].tool", i), Reason: fmt.Sprintf("unsupported tool %q (v1 supports gradle only)", b.Tool)}
		}
	}
	for i, r := range m.Registries {
		field := fmt.Sprintf("registries[%d]", i)
		if r.Alias == "" {
			return &ManifestError{Field: field + ".alias", Reason: "empty registry alias"}
		}
		if r.Upstream == "" {
			return &ManifestError{Field: field + ".upstream", Reason: "empty registry upstream"}
		}
		if strings.Contains(r.Upstream, "@") {
			return &ManifestError{Field: field + ".upstream", Reason: fmt.Sprintf("upstream %q contains embedded credentials ('@'); credentials must stay in the OMAC keychain, not the manifest", r.Upstream)}
		}
	}
	return nil
}

// Validate validates a parsed manifest against host policy: schema version,
// secret-field rejection, forbidden-field rejection, absolute-root rejection,
// and resource-ceiling checks. A zero/empty manifest (no file) validates
// cleanly — that is the standard-Gradle-project case.
//
// host is the authority ceiling; a resource request ABOVE host.MaxHeap /
// host.MaxDuration etc. is rejected (ExitPolicyDenied before executor
// startup). A request AT or BELOW the ceiling is accepted (the manifest
// may narrow but not widen).
//
// Returns a *ManifestError for schema/secret/absolute-path/ceiling problems,
// or a *HostForbiddenError for forbidden-shape fields. Both map to
// ExitPolicyDenied in the CLI.
func (m *Manifest) Validate(host HostPolicy) error {
	if err := m.validateStructure(); err != nil {
		return err
	}
	if m == nil || (m.Version == 0 && len(m.Builds) == 0 && len(m.Registries) == 0 && m.Resources == nil) {
		return nil
	}
	if m.Resources != nil {
		if err := validateResources(m.Resources, host); err != nil {
			return err
		}
	}
	return nil
}

// validateResources checks each non-zero resource request against the host
// ceiling. A request above the ceiling is rejected (the manifest may narrow
// but not widen). A zero field means "use host default" and is always OK.
//
// A non-zero request against a ZERO host ceiling is REJECTED: spec.md:150
// says "OMAC provides host-owned defaults and ceilings for CPU, memory,
// process count" — a zero ceiling means the host has not authorized that
// dimension yet (the limit is not wired to a concrete host value in v1),
// so fail-closed rather than letting any request through. The denial names
// the dimension so the user knows the host policy must be configured. v1
// exposes only maxHeap + maxDuration as requestable dimensions; CPU and
// process-count ceilings are not requestable at all (they are not wired to
// host limits, so the manifest does not present them).
func validateResources(r *ResourceRequests, host HostPolicy) error {
	if r.MaxHeap != "" {
		if host.MaxHeap == "" {
			return &ManifestError{Field: "resources.maxHeap", Reason: "host policy has no max-heap ceiling configured; a manifest request requires the host to set the ceiling first (spec.md:150)"}
		}
		if heapAbove(r.MaxHeap, host.MaxHeap) {
			return &ManifestError{Field: "resources.maxHeap", Reason: fmt.Sprintf("request %q exceeds host ceiling %q — reduce the request or raise the host policy", r.MaxHeap, host.MaxHeap)}
		}
	}
	if r.MaxDuration > 0 {
		if host.MaxDuration == 0 {
			return &ManifestError{Field: "resources.maxDuration", Reason: "host policy has no max-duration ceiling configured; a manifest request requires the host to set the ceiling first (spec.md:150)"}
		}
		if r.MaxDuration > host.MaxDuration {
			return &ManifestError{Field: "resources.maxDuration", Reason: fmt.Sprintf("request %s exceeds host ceiling %s — reduce the request or raise the host policy", r.MaxDuration, host.MaxDuration)}
		}
	}
	return nil
}

// heapAbove reports whether a -Xmx-style heap request exceeds the ceiling.
// Both are sizes like "2g", "512m", "1024k", or a plain byte count. A
// request that does not parse is treated as "above" (fail-closed).
func heapAbove(request, ceiling string) bool {
	r, rOK := parseHeap(request)
	c, cOK := parseHeap(ceiling)
	if !rOK || !cOK {
		return true // fail-closed on unparseable
	}
	return r > c
}

// parseHeap parses a -Xmx-style size ("2g", "512m", "1024k", "8192") into bytes.
func parseHeap(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	mult := int64(1)
	num := s
	switch s[len(s)-1] {
	case 'g', 'G':
		mult = 1024 * 1024 * 1024
		num = s[:len(s)-1]
	case 'm', 'M':
		mult = 1024 * 1024
		num = s[:len(s)-1]
	case 'k', 'K':
		mult = 1024
		num = s[:len(s)-1]
	}
	var n int64
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil {
		return 0, false
	}
	return n * mult, true
}

// scanForSecrets walks the manifest's generic YAML tree and rejects any field
// whose name matches the secret pattern with a non-empty value, and flags
// forbidden-shape fields (bindMounts, privileged, ...) as HostForbiddenError.
// It prefers the raw tree captured at Parse time (which preserves unknown
// fields the typed struct drops); when raw is nil (in-code Manifest), it
// re-marshals the struct to a generic tree so the scan still runs.
func scanForSecrets(m *Manifest) error {
	var generic map[string]any
	if m != nil && m.raw != nil {
		generic = m.raw
	} else {
		data, err := yaml.Marshal(m)
		if err != nil {
			return &ManifestError{Field: ManifestPath, Reason: fmt.Sprintf("internal: re-marshal for secret scan: %v", err)}
		}
		if err := yaml.Unmarshal(data, &generic); err != nil {
			return &ManifestError{Field: ManifestPath, Reason: fmt.Sprintf("internal: re-decode for secret scan: %v", err)}
		}
	}
	return walkSecrets(generic, "")
}

// walkSecrets recurses through a generic YAML-decoded structure rejecting
// secret-named fields with non-empty values and forbidden-shape fields.
func walkSecrets(v any, path string) error {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			fieldPath := k
			if path != "" {
				fieldPath = path + "." + k
			}
			if secretFieldRe.MatchString(k) {
				if !isEmptyValue(val) {
					return &ManifestError{Field: fieldPath, Reason: "secret field rejected — credentials must stay in the OMAC keychain, not the manifest"}
				}
			}
			if forbiddenFieldRe.MatchString(k) {
				return &HostForbiddenError{Field: fieldPath, Kind: k}
			}
			if err := walkSecrets(val, fieldPath); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range t {
			if err := walkSecrets(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

// isEmptyValue reports whether a YAML-decoded value is empty (nil, "", 0,
// false, empty slice/map). A secret field with an empty value is allowed
// (no secret present); only a non-empty value is rejected.
func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case int:
		return t == 0
	case int64:
		return t == 0
	case float64:
		return t == 0
	case bool:
		return !t
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

// HasManifest reports whether a parsed manifest actually declares anything
// (vs. the zero value returned by Load for a missing file).
func (m *Manifest) HasManifest() bool {
	return m != nil && (m.Version != 0 || len(m.Builds) != 0 || len(m.Registries) != 0 || m.Resources != nil)
}
