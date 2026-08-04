package buildmanifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ControlDir is the OMAC-owned control root inside the cache leaf where
// manifest approval records live. It is the SAME directory the build-run
// control state already uses (`.omac-control/`), so the read-only control
// path protection in internal/buildrun/control.go covers these files too.
// Approval records are OMAC-owned and read-only to the executor; they live
// UNDER THE CACHE LEAF (per-developer), NEVER in the worktree (which is
// committed/shared).
const ControlDir = ".omac-control"

// ApprovalFilename is the approval record filename under ControlDir.
const ApprovalFilename = "manifest-approval.json"

// ActiveFilename is the active (frozen-for-session) manifest record under
// ControlDir. The active record stores the digest + capability set currently
// in effect for this OMAC session; a build compares the worktree manifest's
// digest against it to decide unattended-start vs re-approval gate.
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

// LoadApproval reads the approval record from `<leaf>/.omac-control/manifest-approval.json`.
// A missing file yields a zero record and nil error (first-ever approval).
func LoadApproval(leaf string) (ApprovalRecord, error) {
	data, err := os.ReadFile(approvalPath(leaf))
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

// StoreApproval writes the approval record to `<leaf>/.omac-control/manifest-approval.json`.
// The directory is created (0o700) if absent. The file is written 0o644 (the
// control dir is 0o700, owned by omac; the executor reads it read-only via
// the build-run control-state protection).
func StoreApproval(leaf string, rec ApprovalRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest approval: %w", err)
	}
	if err := ensureControlDir(leaf); err != nil {
		return err
	}
	if err := os.WriteFile(approvalPath(leaf), data, 0o644); err != nil {
		return fmt.Errorf("write manifest approval: %w", err)
	}
	return nil
}

// LoadActive reads the active (frozen-for-session) record. Missing → zero, nil.
func LoadActive(leaf string) (ActiveRecord, error) {
	data, err := os.ReadFile(activePath(leaf))
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

// StoreActive writes the active (frozen-for-session) record.
func StoreActive(leaf string, rec ActiveRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal active manifest: %w", err)
	}
	if err := ensureControlDir(leaf); err != nil {
		return err
	}
	if err := os.WriteFile(activePath(leaf), data, 0o644); err != nil {
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

func approvalPath(leaf string) string { return filepath.Join(leaf, ControlDir, ApprovalFilename) }
func activePath(leaf string) string   { return filepath.Join(leaf, ControlDir, ActiveFilename) }

func ensureControlDir(leaf string) error {
	dir := filepath.Join(leaf, ControlDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("prepare control dir: %w", err)
	}
	return os.Chmod(dir, 0o700)
}
