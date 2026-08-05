package buildmanifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ControlDir is the OMAC-owned control root inside the cache leaf where
// the build-run control state (gradle.properties, init.d, README) lives.
// Approval records are OMAC-owned and read-only to the executor.
//
// Ticket 06 relocates DURABLE APPROVAL STORAGE from `<leaf>/.omac-control/`
// to the host-only build-control root at `<cache-root>/build-control/
// approvals/<sha256(canonical-worktree)>.json` (see internal/buildcontrol).
// The on-leaf `.omac-control/` directory is retained for the build-run
// control state (gradle.properties, init.d scripts) which the executor
// must read; the durable approval record no longer lives under the leaf
// (it is host-only, namespaced by canonical worktree so shared-leaf
// worktrees keep distinct approval records, and never included in
// outer-agent or executor grants).
//
// The approval path helpers (approvalPath/activePath) now take an
// explicit storage root + canonical worktree; legacy callers that pass
// only a leaf fall back to the historical on-leaf path so existing
// tests and the direct-host invocation-before-restart path stay
// behavior-preserving.
const ControlDir = ".omac-control"

// ApprovalFilename is the approval record filename. Under the legacy
// on-leaf layout it lived at `<leaf>/.omac-control/manifest-approval.json`;
// under the ticket-06 build-control layout it lives at
// `<build-control-root>/approvals/<sha256(worktree)>.json` and the
// filename below is unused (the path is fully derived from the worktree
// hash). Retained for the legacy path helpers and tests.
const ApprovalFilename = "manifest-approval.json"

// ActiveFilename is the active (frozen-for-session) manifest record
// filename under the legacy on-leaf ControlDir. Under the ticket-06
// layout the active record is an IN-MEMORY parent snapshot keyed by
// canonical worktree (see internal/buildengine); it is no longer
// written to disk. The filename is retained for the legacy path
// helpers and tests.
const ActiveFilename = "active-manifest.json"

// ApprovalRecord is the persisted approval: the manifest content digest and
// the effective (post-ceiling) capability set the host user accepted, plus a
// timestamp. OMAC reuses this approval while the digest is unchanged and the
// effective set still matches the current host ceiling; a changed digest OR a
// host-ceiling drop below what was approved forces a consolidated review.
type ApprovalRecord struct {
	// Digest is the SHA-256 digest of the approved manifest content.
	Digest string `json:"digest"`
	// Capabilities is the effective (post-ceiling) capability set approved.
	Capabilities CapabilitySet `json:"capabilities"`
	// ApprovedAt is when the host user accepted this digest + set.
	ApprovedAt time.Time `json:"approvedAt"`
}

// ActiveRecord is the frozen-for-session manifest record. Once a digest is
// approved for this OMAC session, subsequent builds in the same session use
// the FROZEN capability set even if `.omac/build.yaml` changes on disk
// mid-session. The session boundary is the cache leaf (per-developer-per-
// machine), since each `omac build` is a separate process (the daemon is
// Gradle's own process under the leaf, not an omac supervisor per ADR 0001).
type ActiveRecord struct {
	// Digest is the SHA-256 digest of the manifest currently frozen for
	// this session.
	Digest string `json:"digest"`
	// Capabilities is the frozen effective capability set.
	Capabilities CapabilitySet `json:"capabilities"`
	// ActivatedAt is when this digest was first frozen for the session.
	ActivatedAt time.Time `json:"activatedAt"`
}

// Location selects where durable approval records are stored. The
// legacy OnLeaf layout kept approvals at `<leaf>/.omac-control/
// manifest-approval.json`; the ticket-06 BuildControl layout stores
// them at `<cacheRoot>/build-control/approvals/
// <sha256(canonical-worktree)>.json` so approvals are host-only,
// namespaced by canonical worktree (shared-leaf worktrees keep
// distinct records), and never included in outer-agent or executor
// grants.
//
// A zero Location defaults to the legacy OnLeaf layout so existing
// buildmanifest tests (which pass only a leaf) stay
// behavior-preserving. Production wires BuildControl via
// NewBuildControlLocation.
type Location struct {
	kind          locationKind
	cacheRoot     string // BuildControl: shared cache root (~/.cache/omac)
	worktree      string // BuildControl: canonical worktree
}

type locationKind int

const (
	locationOnLeaf locationKind = iota
	locationBuildControl
)

// NewOnLeafLocation returns a Location that stores approval records
// under `<leaf>/.omac-control/` (the legacy layout). Used by tests and
// by the direct-host invocation path that has no parent-owned snapshot
// (the parent snapshot is ticket 06; the direct adapter keeps the
// historical on-leaf gate semantics).
func NewOnLeafLocation() Location { return Location{kind: locationOnLeaf} }

// NewBuildControlLocation returns a Location that stores approval
// records under the host-only build-control root at
// `<cacheRoot>/build-control/approvals/<sha256(worktree)>.json`.
// cacheRoot is the shared cache root (parent of cache-scope dirs);
// worktree is the canonical (EvalSymlinks-resolved) worktree root.
// The build-control root is NEVER included in outer-agent or executor
// grants.
func NewBuildControlLocation(cacheRoot, canonicalWorktree string) Location {
	return Location{kind: locationBuildControl, cacheRoot: cacheRoot, worktree: canonicalWorktree}
}

// LoadApproval reads the approval record. A missing file yields a zero
// record and nil error (first-ever approval).
func LoadApproval(leaf string) (ApprovalRecord, error) {
	return LoadApprovalAt(leaf, NewOnLeafLocation())
}

// LoadApprovalAt reads the approval record at the location-selected
// path. A missing file yields a zero record and nil error.
func LoadApprovalAt(leaf string, loc Location) (ApprovalRecord, error) {
	data, err := os.ReadFile(approvalPathAt(leaf, loc))
	if err != nil {
		if os.IsNotExist(err) {
			return ApprovalRecord{}, nil
		}
		return ApprovalRecord{}, fmt.Errorf("load manifest approval: %w", err)
	}
	var rec ApprovalRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return ApprovalRecord{}, fmt.Errorf("parse manifest approval: %w", err)
	}
	return rec, nil
}

// StoreApproval writes the approval record. Legacy on-leaf layout:
// `<leaf>/.omac-control/manifest-approval.json` (the control dir is
// created 0o700 if absent). The file is written 0o644 (the control dir
// is 0o700, owned by omac; the executor reads it read-only via the
// build-run control-state protection).
func StoreApproval(leaf string, rec ApprovalRecord) error {
	return StoreApprovalAt(leaf, NewOnLeafLocation(), rec)
}

// StoreApprovalAt writes the approval record at the location-selected
// path. For the BuildControl layout the build-control root and its
// approvals subdirectory are created 0o700 if absent; the file is
// written 0o600 (host-only, never granted to the executor or outer
// agent).
func StoreApprovalAt(leaf string, loc Location, rec ApprovalRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest approval: %w", err)
	}
	path := approvalPathAt(leaf, loc)
	if err := ensureParent(path, loc); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if loc.kind == locationBuildControl {
		mode = 0o600
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("write manifest approval: %w", err)
	}
	return nil
}

// LoadActive reads the active (frozen-for-session) record. Missing → zero, nil.
//
// Under the ticket-06 BuildControl layout the active record is an
// in-memory parent snapshot (see internal/buildengine) and is NOT
// written to disk; LoadActiveAt on a BuildControl location reads the
// on-disk approval record as the active set's durable source (the
// parent snapshot is derived from it at activation). The on-disk
// "active-manifest.json" file remains for the legacy OnLeaf layout
// only.
func LoadActive(leaf string) (ActiveRecord, error) {
	return LoadActiveAt(leaf, NewOnLeafLocation())
}

// LoadActiveAt reads the active record at the location-selected path.
// For the BuildControl layout the active record is in-memory in the
// parent; on disk the approval record IS the durable source, so this
// returns the approval record's digest + capability set (the parent
// freezes them at activation when the digest matches the durable
// approval).
func LoadActiveAt(leaf string, loc Location) (ActiveRecord, error) {
	if loc.kind == locationBuildControl {
		// The active record is the parent's in-memory snapshot; on disk
		// the durable approval record IS the source. Return it so a
		// direct-host invocation (which has no parent snapshot) can
		// still gate against the durable approval.
		rec, err := LoadApprovalAt(leaf, loc)
		if err != nil {
			return ActiveRecord{}, err
		}
		return ActiveRecord{
			Digest:       rec.Digest,
			Capabilities: rec.Capabilities,
			ActivatedAt:  rec.ApprovedAt,
		}, nil
	}
	data, err := os.ReadFile(activePathAt(leaf, loc))
	if err != nil {
		if os.IsNotExist(err) {
			return ActiveRecord{}, nil
		}
		return ActiveRecord{}, fmt.Errorf("load active manifest: %w", err)
	}
	var rec ActiveRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return ActiveRecord{}, fmt.Errorf("parse active manifest: %w", err)
	}
	return rec, nil
}

// StoreActive writes the active (frozen-for-session) record. Under the
// BuildControl layout the active record is in-memory in the parent and
// is NOT written to disk (ticket 06: a build request cannot advance or
// replace the parent snapshot); StoreApprovalAt is the single durable
// write path. This function is retained for the legacy OnLeaf layout
// and for tests.
func StoreActive(leaf string, rec ActiveRecord) error {
	return StoreActiveAt(leaf, NewOnLeafLocation(), rec)
}

// StoreActiveAt writes the active record at the location-selected path.
// For the BuildControl layout this is a no-op (the active record lives
// in parent memory; the durable approval record is the on-disk source).
func StoreActiveAt(leaf string, loc Location, rec ActiveRecord) error {
	if loc.kind == locationBuildControl {
		return nil // active record is in-memory in the parent; nothing to write
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal active manifest: %w", err)
	}
	if err := ensureControlDir(leaf); err != nil {
		return err
	}
	if err := os.WriteFile(activePathAt(leaf, loc), data, 0o644); err != nil {
		return fmt.Errorf("write active manifest: %w", err)
	}
	return nil
}

// CapabilityDiff is the consolidated diff between a previous and a current
// capability set, used to present one consolidated review when a manifest
// changes. Each list is the set of names ADDED, REMOVED, or CHANGED.
type CapabilityDiff struct {
	AddedBuildRoots   []string
	RemovedBuildRoots []string
	AddedImages       []string
	RemovedImages     []string
	AddedRegistries   []string
	RemovedRegistries []string
	// ResourcesChanged is true when the effective resource set changed.
	ResourcesChanged bool
}

// IsEmpty reports whether the diff has no changes (the manifests are
// equivalent in capability terms).
func (d CapabilityDiff) IsEmpty() bool {
	return len(d.AddedBuildRoots) == 0 && len(d.RemovedBuildRoots) == 0 &&
		len(d.AddedImages) == 0 && len(d.RemovedImages) == 0 &&
		len(d.AddedRegistries) == 0 && len(d.RemovedRegistries) == 0 &&
		!d.ResourcesChanged
}

// Diff computes the consolidated capability diff between a previous and a
// current capability set. Used to present one consolidated review when a
// manifest changes (spec.md:101 — "a changed manifest produces one
// consolidated review at the next OMAC start").
func Diff(prev, cur CapabilitySet) CapabilityDiff {
	return CapabilityDiff{
		AddedBuildRoots:   sliceMinus(cur.BuildRoots, prev.BuildRoots),
		RemovedBuildRoots: sliceMinus(prev.BuildRoots, cur.BuildRoots),
		AddedImages:       sliceMinus(cur.Images, prev.Images),
		RemovedImages:     sliceMinus(prev.Images, cur.Images),
		AddedRegistries:   sliceMinus(cur.Registries, prev.Registries),
		RemovedRegistries: sliceMinus(prev.Registries, cur.Registries),
		ResourcesChanged:  prev.Resources != cur.Resources,
	}
}

// Render produces a human-readable consolidated review of the diff, suitable
// for the unattended-agent "present" path: a clear, structured stderr
// message describing what changed and the action required (restart to
// activate). Caller wraps with the "do not retry" framing.
func (d CapabilityDiff) Render() string {
	var parts []string
	if len(d.AddedBuildRoots) > 0 {
		parts = append(parts, fmt.Sprintf("added build roots: %v", d.AddedBuildRoots))
	}
	if len(d.RemovedBuildRoots) > 0 {
		parts = append(parts, fmt.Sprintf("removed build roots: %v", d.RemovedBuildRoots))
	}
	if len(d.AddedImages) > 0 {
		parts = append(parts, fmt.Sprintf("added images: %v", d.AddedImages))
	}
	if len(d.RemovedImages) > 0 {
		parts = append(parts, fmt.Sprintf("removed images: %v", d.RemovedImages))
	}
	if len(d.AddedRegistries) > 0 {
		parts = append(parts, fmt.Sprintf("added registries: %v", d.AddedRegistries))
	}
	if len(d.RemovedRegistries) > 0 {
		parts = append(parts, fmt.Sprintf("removed registries: %v", d.RemovedRegistries))
	}
	if d.ResourcesChanged {
		parts = append(parts, "resource requests changed")
	}
	if len(parts) == 0 {
		return "no capability changes"
	}
	out := "OMAC build manifest changed. Consolidated capability diff:"
	for _, p := range parts {
		out += "\n  - " + p
	}
	out += "\nRestart OMAC to review and activate the changed capability set."
	return out
}

// sliceMinus returns elements of a that are not in b.
func sliceMinus(a, b []string) []string {
	bset := map[string]bool{}
	for _, x := range b {
		bset[x] = true
	}
	var out []string
	for _, x := range a {
		if !bset[x] {
			out = append(out, x)
		}
	}
	return out
}

func approvalPath(leaf string) string   { return filepath.Join(leaf, ControlDir, ApprovalFilename) }
func activePath(leaf string) string     { return filepath.Join(leaf, ControlDir, ActiveFilename) }

// approvalPathAt returns the approval record path for the given
// location. OnLeaf → `<leaf>/.omac-control/manifest-approval.json`;
// BuildControl → `<cacheRoot>/build-control/approvals/<sha256(worktree)>.json`.
func approvalPathAt(leaf string, loc Location) string {
	if loc.kind == locationBuildControl {
		return buildcontrolApprovalPath(loc.cacheRoot, loc.worktree)
	}
	return approvalPath(leaf)
}

// activePathAt returns the active record path for the given location.
// OnLeaf → `<leaf>/.omac-control/active-manifest.json`; BuildControl →
// the approval record path (the active record is in-memory in the
// parent; on disk the approval record IS the durable source).
func activePathAt(leaf string, loc Location) string {
	if loc.kind == locationBuildControl {
		return buildcontrolApprovalPath(loc.cacheRoot, loc.worktree)
	}
	return activePath(leaf)
}

// buildcontrolApprovalPath mirrors internal/buildcontrol.ApprovalPath
// without importing the package (buildmanifest must not depend on
// buildcontrol — buildcontrol imports buildmanifest's path constants
// via this package's exported filenames, so the dependency direction
// is buildcontrol → buildmanifest). The hashing MUST stay identical
// to buildcontrol.HashWorktree (sha256, lowercase hex).
func buildcontrolApprovalPath(cacheRoot, worktree string) string {
	d := sha256.Sum256([]byte(worktree))
	return filepath.Join(cacheRoot, "build-control", "approvals", hex.EncodeToString(d[:])+".json")
}

// ensureParent creates the parent directory of path with the
// location-appropriate mode (0o700 for both layouts).
func ensureParent(path string, loc Location) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("prepare approval dir: %w", err)
	}
	return os.Chmod(dir, 0o700)
}

func ensureControlDir(leaf string) error {
	dir := filepath.Join(leaf, ControlDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("prepare control dir: %w", err)
	}
	return os.Chmod(dir, 0o700)
}
