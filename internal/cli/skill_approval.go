package cli

import (
	"fmt"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
	"github.com/tngtech/oh-my-agentic-coder/internal/registry"
	"github.com/tngtech/oh-my-agentic-coder/internal/skilltrust"
	"github.com/tngtech/oh-my-agentic-coder/internal/supervisor"
)

// A skill sidecar runs UNSANDBOXED, so it may spawn only when its current
// on-disk content is recorded in the host-only approval store the sandbox
// cannot write. See internal/skilltrust and docs/SECURITY_MODEL.md.

// skillSpawnAuthorizer is the supervisor spawn-gate backstop: every spawn
// funnels through it even if a caller forgets its own pre-flight check. A
// sidecar may run only from the host-only approval snapshot (which the
// sandbox cannot write); anything else is refused. This is unforgeable by
// construction and, unlike re-hashing, does not spuriously reject a skill
// whose snapshot legitimately differs from the workdir hash (recreated
// symlinks). The pre-flight approvedSpawnDir is what actually maps an
// approved skill to its snapshot dir.
func skillSpawnAuthorizer() func(supervisor.SidecarSpec) error {
	return func(spec supervisor.SidecarSpec) error {
		if skilltrust.IsSnapshotPath(spec.SkillDir) {
			return nil
		}
		name := spec.SkillName
		if name == "" {
			name = spec.Name
		}
		return errSkillNotApproved(name)
	}
}

// approvalStatus reports whether skill `name` with the content currently on
// disk at skillDir is host-approved. Any error (unreadable dir, unresolvable
// store) fails closed: ok=false.
func approvalStatus(name, skillDir string) (bool, error) {
	hash, err := config.BundleHash(skillDir)
	if err != nil {
		return false, fmt.Errorf("bundle hash: %w", err)
	}
	ok, err := skilltrust.IsApproved(name, hash)
	if err != nil {
		return false, fmt.Errorf("approval check: %w", err)
	}
	return ok, nil
}

// approvedSpawnDir resolves the directory a skill's sidecar must be spawned
// FROM: the immutable, host-only snapshot frozen at approval time (see
// internal/skilltrust snapshot.go). workdirSkillDir is the agent-writable
// source; its current content is hashed and matched against the approval,
// then the corresponding snapshot is returned. ok=false means refuse — either
// the content is not approved, or it is approved but has no snapshot yet
// (re-register to create one). Spawning from the snapshot, not the workdir,
// is what makes the executed bytes exactly the approved bytes.
func approvedSpawnDir(name, workdirSkillDir string) (snapshotDir string, ok bool, err error) {
	hash, herr := config.BundleHash(workdirSkillDir)
	if herr != nil {
		return "", false, fmt.Errorf("bundle hash: %w", herr)
	}
	approved, aerr := skilltrust.IsApproved(name, hash)
	if aerr != nil {
		return "", false, aerr
	}
	if !approved {
		return "", false, nil
	}
	snap, present := skilltrust.SnapshotPath(name, hash)
	if !present {
		return "", false, nil
	}
	return snap, true, nil
}

// errSkillNotApproved is the uniform refusal error/detail; it names the
// host-side remedy the sandboxed agent cannot perform itself.
func errSkillNotApproved(name string) error {
	return fmt.Errorf("spawn refused: skill %q is not host-approved; run `omac register %s` from a host terminal (outside the sandbox) to approve its current code", name, name)
}

// brokenApprovalRoute is the facade route an unapproved skill is mounted as:
// a broken (502) stub naming the remedy, so a probe is answered instead of
// 404'd — and no sidecar is spawned.
func brokenApprovalRoute(mount, name, skillDir string) facade.Route {
	return facade.Route{
		Mount:    mount,
		Skill:    name,
		SkillDir: skillDir,
		State:    facade.RouteBroken,
		Detail:   errSkillNotApproved(name).Error(),
	}
}

// grandfatherApprovals approves every skill in reg using the hash of its
// content ON DISK (what will actually run), resolving workdir-relative
// SkillDirs against workdir. Callers invoke this only on the first upgraded
// run (see firstApprovalUpgrade): before this control existed every
// registered skill already spawned at launch, so upgrading omac must not
// newly break a working setup. This is trust-on-first-upgrade — it approves
// whatever is registered right now, once; everything registered afterwards
// needs an explicit out-of-sandbox `omac register`. Returns the count
// approved. A skill whose dir cannot be hashed is skipped (the spawn gate
// will refuse it and name the remedy).
func grandfatherApprovals(workdir string, reg *registry.Registry) (int, error) {
	n := 0
	for _, e := range reg.Registered {
		absDir := e.SkillDir
		if !filepath.IsAbs(absDir) {
			absDir = filepath.Join(workdir, absDir)
		}
		hash, herr := config.BundleHash(absDir)
		if herr != nil {
			continue
		}
		if err := skilltrust.Approve(e.Name, hash, absDir); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// firstApprovalUpgrade reports whether the host-only approval store has never
// been created — i.e. this is the first run under approval-gated spawning.
// Capture it once at an entry point's start, BEFORE any grandfathering (which
// creates the store), so every registry loaded during that first run is
// grandfathered while later runs are not.
func firstApprovalUpgrade() bool { return !skilltrust.Exists() }
